// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// manifest_for_test.go — tests for SecretService.ManifestFor, the
// decrypt-free seam the conditional pod-bootstrap 304 decision runs on
// (US-70.2 Part 2). ManifestFor loads rows and hashes the manifest tier
// ONLY; if any code path it reaches touches a ciphertext, these tests
// fail via the panicking decrypt dependencies wired below.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panickingRootKeyProvider fails the test the moment anything decrypts
// through it.
type panickingRootKeyProvider struct{ t *testing.T }

func (p *panickingRootKeyProvider) Encrypt(_ context.Context, _ []byte) ([]byte, error) {
	p.t.Fatalf("ManifestFor must never encrypt; got an Encrypt call")
	return nil, nil
}

func (p *panickingRootKeyProvider) Decrypt(_ context.Context, _ []byte) ([]byte, error) {
	p.t.Fatalf("ManifestFor must never decrypt; got a Decrypt call")
	return nil, nil
}

// panickingKeyStore fails the test if the DEK tier is consulted
// (GetDEKServerSide reaches GetUserKey).
type panickingKeyStore struct{ t *testing.T }

func (p *panickingKeyStore) GetUserKey(_ context.Context, _ string) (*UserKeyRecord, error) {
	p.t.Fatalf("ManifestFor must never touch user_keys; got a GetUserKey call")
	return nil, nil
}
func (p *panickingKeyStore) CreateUserKey(_ context.Context, _ *UserKeyRecord) error {
	p.t.Fatalf("ManifestFor must never touch user_keys; got a CreateUserKey call")
	return nil
}
func (p *panickingKeyStore) UpdateWrappedDEK(_ context.Context, _ string, _, _ []byte, _ int) error {
	p.t.Fatalf("ManifestFor must never touch user_keys; got an UpdateWrappedDEK call")
	return nil
}

// failingCredentialStore makes the row load fail so error propagation is
// observable.
type failingCredentialStore struct{ mockCredentialStore }

func (f *failingCredentialStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]CredentialBinding, error) {
	return nil, assert.AnError
}

// TestManifestFor_MatchesBuilderRevision: the hash ManifestFor computes
// from rows alone must equal the revision a full BuildWorkspaceBatch
// stamps — otherwise a client presenting the builder's manifestHash
// would 304 against a different manifest than the one a 200 would deliver.
func TestManifestFor_MatchesBuilderRevision(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	hash, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)
	assert.Equal(t, batch.Revision.ManifestHash, hash,
		"the 304-decision hash and the built revision must describe the same manifest")
}

// TestManifestFor_NeverDecrypts: with every decrypt dependency wired to
// panic (DEK key store, admin and org providers) ManifestFor must still
// succeed — the manifest tier is rows + hashing, nothing else. This is
// the zero-decrypt pin for the 304 path.
func TestManifestFor_NeverDecrypts(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{
		env.adminCred,
		{ID: "cred-user", OwnerType: "user", OwnerID: "user-1", Kind: "openai", Slug: "openai",
			Ciphertext: []byte("user-cipher"), Version: 1, SourceType: "explicit", WithinPriority: 10},
	}}
	svc.store = env.store()
	addUserSecretRowDirect(t, env, "ws-1", "sec-x", "user-1", "tok", SecretTypeEnvSecret)

	svc.SetAdminProvider(&panickingRootKeyProvider{t: t})
	svc.SetOrgProvider(&panickingRootKeyProvider{t: t})
	svc.keys = NewKeyService(&panickingKeyStore{t: t}, newMockDEKCache())

	hash, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
}

// addUserSecretRowDirect seeds a user secret + binding without going
// through the service (no session required).
func addUserSecretRowDirect(t *testing.T, env *builderEnv, workspaceID, secretID, userID, name string, typ SecretType) {
	t.Helper()
	require.NoError(t, env.secrets.CreateSecret(context.Background(), &UserSecret{
		ID: secretID, UserID: userID, Name: name, Type: typ,
		Ciphertext: []byte("cipher"), KeyVersion: 1,
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	}))
	require.NoError(t, env.secrets.AddBindings(context.Background(), workspaceID, []string{secretID}))
}

// TestManifestFor_TracksRowChanges: a row mutation that bumps a version
// must change the manifest hash (that is what flips a 304 into a 200).
func TestManifestFor_TracksRowChanges(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	created := addUserSecret(t, svc, sessionID, "db_url")

	before, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	row, err := env.secrets.GetSecret(context.Background(), "user-1", created.ID)
	require.NoError(t, err)
	row.Ciphertext = []byte("rotated")
	require.NoError(t, env.secrets.UpdateSecret(context.Background(), row))

	after, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "a version-bumping mutation must change the manifest hash")
}

// TestManifestFor_StoreErrors_Propagate: row-load failures surface as
// errors — the handler turns them into the same 500 the builder path
// produces today.
func TestManifestFor_StoreErrors_Propagate(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	svc.store = &builderTestStore{
		SecretStore:       env.secrets,
		CredentialStore:   &failingCredentialStore{},
		fakeRevisionStore: env.revis,
	}

	_, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
}

// TestManifestFor_DeterministicAcrossCalls: identical row state yields
// the identical hash on every call (I6 — any replica would agree).
func TestManifestFor_DeterministicAcrossCalls(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	first, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	second, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

// TestCurrentRevision_DelegatesToStore: the service-level reader returns
// exactly what the revision row holds, including the no-row state.
func TestCurrentRevision_DelegatesToStore(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	svc.store = env.store()

	seq, hash, ok, err := svc.CurrentRevision(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.False(t, ok, "no revision minted yet")
	assert.Zero(t, seq)
	assert.Empty(t, hash)

	_, err = env.revis.EnsureRevision(context.Background(), "ws-1", "h1")
	require.NoError(t, err)
	seq, hash, ok, err = svc.CurrentRevision(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 1, seq)
	assert.Equal(t, "h1", hash)
}

// TestCurrentRevision_StoreWithoutRevisionStoreErrors mirrors the
// builder's loud-failure contract: a store that cannot read revisions
// must fail, never fabricate.
func TestCurrentRevision_StoreWithoutRevisionStoreErrors(t *testing.T) {
	svc, _, _ := setupBuilder(t)
	svc.store = revisionlessStore{SecretStore: newMockSecretStore()}

	_, _, _, err := svc.CurrentRevision(context.Background(), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RevisionStore")
}

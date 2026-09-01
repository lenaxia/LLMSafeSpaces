// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// force_revoke_test.go — tests for SecretService.ForceRevokeSecret
// (US-70.3, I12: revocation is absence). One operation removes every
// workspace binding for the secret, force-refreshes each affected
// workspace's stored revision (manifest tier only — zero decrypts), and
// reports the affected set so the handler can notify the live pods.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMultiWorkspaceSecret binds one env-secret to three workspaces and
// a second, unrelated secret to one of them, so tests can pin "exactly
// the affected set".
func seedMultiWorkspaceSecret(t *testing.T, env *builderEnv) (revokedID, unrelatedID string) {
	t.Helper()
	ctx := context.Background()
	revoked := &UserSecret{
		ID: "sec-revoked", UserID: "user-1", Name: "doomed", Type: SecretTypeEnvSecret,
		Ciphertext: []byte("cipher"), KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"var_name":"DOOMED"}`),
	}
	unrelated := &UserSecret{
		ID: "sec-keep", UserID: "user-1", Name: "keeper", Type: SecretTypeEnvSecret,
		Ciphertext: []byte("cipher"), KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"var_name":"KEEP"}`),
	}
	require.NoError(t, env.secrets.CreateSecret(ctx, revoked))
	require.NoError(t, env.secrets.CreateSecret(ctx, unrelated))
	for _, ws := range []string{"ws-1", "ws-2", "ws-3"} {
		require.NoError(t, env.secrets.AddBindings(ctx, ws, []string{revoked.ID}))
	}
	require.NoError(t, env.secrets.AddBindings(ctx, "ws-2", []string{unrelated.ID}))
	return revoked.ID, unrelated.ID
}

func TestForceRevokeSecret_RemovesEveryBindingAndReportsAffected(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{}
	svc.store = env.store()
	revokedID, _ := seedMultiWorkspaceSecret(t, env)
	ctx := context.Background()

	affected, err := svc.ForceRevokeSecret(ctx, "user-1", revokedID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ws-1", "ws-2", "ws-3"}, affected)

	for _, ws := range []string{"ws-1", "ws-2", "ws-3"} {
		bound, err := env.secrets.GetBindings(ctx, ws)
		require.NoError(t, err)
		for _, sec := range bound {
			assert.NotEqual(t, revokedID, sec.ID, "workspace %s must no longer bind the revoked secret", ws)
		}
	}
	gone, err := env.secrets.GetSecret(ctx, "user-1", revokedID)
	require.NoError(t, err)
	assert.Nil(t, gone, "the secret row itself must be gone")
}

func TestForceRevokeSecret_RefreshesStoredRevisionForExactlyAffectedSet(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{}
	svc.store = env.store()
	revokedID, _ := seedMultiWorkspaceSecret(t, env)
	ctx := context.Background()

	for _, ws := range []string{"ws-1", "ws-2", "ws-3", "ws-untouched"} {
		hash, err := svc.ManifestFor(ctx, "user-1", ws)
		require.NoError(t, err)
		_, err = env.revis.EnsureRevision(ctx, ws, hash)
		require.NoError(t, err)
	}
	before := map[string]int64{}
	for _, ws := range []string{"ws-1", "ws-2", "ws-3", "ws-untouched"} {
		seq, _, ok, err := env.revis.CurrentRevision(ctx, ws)
		require.NoError(t, err)
		require.True(t, ok)
		before[ws] = seq
	}

	_, err := svc.ForceRevokeSecret(ctx, "user-1", revokedID)
	require.NoError(t, err)

	for _, ws := range []string{"ws-1", "ws-2", "ws-3"} {
		seq, hash, ok, err := env.revis.CurrentRevision(ctx, ws)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, before[ws]+1, seq,
			"affected workspace %s must have its stored revision bumped immediately (revoke that left the stored row stale would look converged to the reconcile loop)", ws)
		wantHash, err := svc.ManifestFor(ctx, "user-1", ws)
		require.NoError(t, err)
		assert.Equal(t, wantHash, hash, "stored hash must equal the post-revoke manifest")
	}
	seq, _, ok, err := env.revis.CurrentRevision(ctx, "ws-untouched")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, before["ws-untouched"], seq, "unaffected workspace must NOT mint a new seq")
}

func TestForceRevokeSecret_NeverDecrypts(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{}
	svc.store = env.store()
	revokedID, _ := seedMultiWorkspaceSecret(t, env)

	svc.SetAdminProvider(&panickingRootKeyProvider{t: t})
	svc.SetOrgProvider(&panickingRootKeyProvider{t: t})
	svc.keys = NewKeyService(&panickingKeyStore{t: t}, newMockDEKCache())

	_, err := svc.ForceRevokeSecret(context.Background(), "user-1", revokedID)
	require.NoError(t, err, "the revocation refresh is manifest-tier only — no decrypt may be reachable")
}

func TestForceRevokeSecret_NotFoundAndForeignSecret(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	svc.store = env.store()
	revokedID, _ := seedMultiWorkspaceSecret(t, env)

	_, err := svc.ForceRevokeSecret(context.Background(), "user-1", "sec-missing")
	assert.ErrorIs(t, err, ErrSecretNotFound)

	_, err = svc.ForceRevokeSecret(context.Background(), "user-2", revokedID)
	assert.ErrorIs(t, err, ErrSecretNotFound, "a foreign owner must not revoke — uniform not-found, no existence leak")

	bound, err := env.secrets.GetBindings(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.NotEmpty(t, bound, "failed revoke must not mutate bindings")
}

func TestForceRevokeSecret_AuditsPerWorkspace(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{}
	svc.store = env.store()
	revokedID, _ := seedMultiWorkspaceSecret(t, env)

	resetAudit(env.secrets)
	_, err := svc.ForceRevokeSecret(context.Background(), "user-1", revokedID)
	require.NoError(t, err)

	actions := auditActions(env.secrets)
	assert.Contains(t, actions, "delete")
	count := 0
	for _, a := range env.secrets.audit {
		if a.Action == "revoke" {
			count++
			require.NotNil(t, a.WorkspaceID)
		}
	}
	assert.Equal(t, 3, count, "one revoke audit per affected workspace")
}

func TestForceRevokeSecret_UnboundSecretIsPlainDelete(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	svc.store = env.store()
	ctx := context.Background()
	orphan := &UserSecret{
		ID: "sec-orphan", UserID: "user-1", Name: "unbound", Type: SecretTypeEnvSecret,
		Ciphertext: []byte("c"), KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"var_name":"X"}`),
	}
	require.NoError(t, env.secrets.CreateSecret(ctx, orphan))

	affected, err := svc.ForceRevokeSecret(ctx, "user-1", orphan.ID)
	require.NoError(t, err)
	assert.Empty(t, affected, "a secret bound to nothing revokes from nothing")
}

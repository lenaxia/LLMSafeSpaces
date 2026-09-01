// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// builder_test.go — unit tests for SecretService.BuildWorkspaceBatch,
// the one builder (US-70.2 / design 0052 §4.1). Session identity is
// absent from construction by design; user entries decrypt through the
// server-side DEK unwrap. These tests pin the behavioral contracts the
// three deleted builders used to carry: happy path, sessionless
// degrade, per-entry failure audit, MCP inclusion, ordering — plus the
// new W12 entry contract and the two-tier revision stamp.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRevisionStore is the in-memory RevisionStore for builder unit
// tests (PG-backed semantics live in revision_integration_test.go).
// Rows are per-workspace like the workspace_secret_revisions table.
type fakeRevisionStore struct {
	mu   sync.Mutex
	rows map[string]mockRevisionRow
}

func (f *fakeRevisionStore) CurrentRevision(_ context.Context, workspaceID string) (int64, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[workspaceID]
	if !ok {
		return 0, "", false, nil
	}
	return row.seq, row.manifestHash, true, nil
}

func (f *fakeRevisionStore) EnsureRevision(_ context.Context, workspaceID, manifestHash string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = make(map[string]mockRevisionRow)
	}
	if row, ok := f.rows[workspaceID]; ok && row.manifestHash == manifestHash {
		return row.seq, nil
	}
	f.rows[workspaceID] = mockRevisionRow{seq: f.rows[workspaceID].seq + 1, manifestHash: manifestHash}
	return f.rows[workspaceID].seq, nil
}

// builderTestStore satisfies SecretStore + CredentialStore + RevisionStore.
type builderTestStore struct {
	SecretStore
	CredentialStore
	*fakeRevisionStore
}

// builderEnv bundles the fakes a test asserts against.
type builderEnv struct {
	secrets   *mockSecretStore
	creds     *mockCredentialStore
	revis     *fakeRevisionStore
	adminKey  []byte
	orgKey    []byte
	adminCred CredentialBinding
	orgCred   CredentialBinding
}

func (e *builderEnv) store() SecretStore {
	if e.creds == nil {
		e.creds = &mockCredentialStore{}
	}
	return &builderTestStore{SecretStore: e.secrets, CredentialStore: e.creds, fakeRevisionStore: e.revis}
}

// setupBuilder wires a SecretService whose user key material wraps
// under a real master provider (so GetDEKServerSide works) and whose
// admin/org credentials decrypt under independent static keys.
func setupBuilder(t *testing.T) (*SecretService, *builderEnv, string) {
	t.Helper()
	ctx := context.Background()

	rootProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0x1D))
	require.NoError(t, err)
	keySvc := NewKeyService(newMockKeyStore(), newMockDEKCache())
	keySvc.SetAPIKeyStore(nil, rootProv)
	require.NoError(t, keySvc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek"))
	sessionID := "builder-session"
	require.NoError(t, keySvc.UnlockDEK(ctx, "user-1", []byte("pw"), sessionID, time.Hour))

	env := &builderEnv{
		secrets:  newMockSecretStore(),
		revis:    &fakeRevisionStore{},
		adminKey: deterministicTestKey(t, 0xA1),
		orgKey:   deterministicTestKey(t, 0xC3),
	}
	env.adminCred = CredentialBinding{
		ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai-admin-row-slug",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "admin-key"}),
		Version:    3, SourceType: "auto",
	}
	env.orgCred = CredentialBinding{
		ID: "cred-org", OwnerType: "org", OwnerID: "org-1", Kind: "openai_compatible", Slug: "custom",
		Ciphertext: adminCiphertext(t, env.orgKey, LLMProviderData{Kind: "openai_compatible", Slug: "custom", APIKey: "org-key"}),
		Version:    2, SourceType: "auto",
	}

	adminProv, err := NewStaticKeyProvider(env.adminKey)
	require.NoError(t, err)
	orgProv, err := NewStaticKeyProvider(env.orgKey)
	require.NoError(t, err)

	svc := NewSecretService(keySvc, env.store())
	svc.SetAdminProvider(adminProv)
	svc.SetOrgProvider(orgProv)
	return svc, env, sessionID
}

func adminCiphertext(t *testing.T, key []byte, pd LLMProviderData) []byte {
	t.Helper()
	plaintext, err := json.Marshal(pd)
	require.NoError(t, err)
	cipher, err := EncryptSecret(key, plaintext)
	require.NoError(t, err)
	return cipher
}

// addUserSecret creates a user-DEK env-secret through the service and
// binds it to ws-1.
func addUserSecret(t *testing.T, svc *SecretService, sessionID, name string) *SecretResponse {
	t.Helper()
	s, err := svc.CreateSecret(context.Background(), "user-1", sessionID, nil, CreateSecretRequest{
		Name:     name,
		Type:     SecretTypeEnvSecret,
		Value:    "v-" + name,
		Metadata: json.RawMessage(`{"var_name":"` + name + `"}`),
	})
	require.NoError(t, err)
	_, err = svc.SetBindings(context.Background(), "user-1", "ws-1", []string{s.ID})
	require.NoError(t, err)
	return s
}

func findEntry(batch *Batch, typ SecretType, name string) (BatchEntry, bool) {
	for _, e := range batch.Entries {
		if e.Type == typ && e.Name == name {
			return e, true
		}
	}
	return BatchEntry{}, false
}

func auditActions(store *mockSecretStore) []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	actions := make([]string, 0, len(store.audit))
	for _, a := range store.audit {
		actions = append(actions, a.Action)
	}
	return actions
}

func resetAudit(store *mockSecretStore) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.audit = nil
}

func TestBuildWorkspaceBatch_HappyPath_DeliversAllClasses(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{
		bindings: []CredentialBinding{env.adminCred},
		mcpRows: []MCPServerBindingRow{{
			ServerID: "srv-1", Name: "github-tools", Transport: "stdio", Command: "npx",
			Args: []string{"-y", "pkg"}, Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{}),
			OwnerType: "admin", KeyVersion: 1, Version: 5, Enabled: true,
		}},
	}
	svc.store = env.store()

	envSecret := addUserSecret(t, svc, sessionID, "db_url")

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade, "fully decryptable workspace must not degrade")
	require.Len(t, batch.Entries, 3)

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Equal(t, "cred-admin", llm.SecretID, "llm-provider entry identified by the credential row's ID")
	assert.EqualValues(t, 3, llm.Version, "entry carries the row's value-version")
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`)

	env1, ok := findEntry(batch, SecretTypeEnvSecret, "db_url")
	require.True(t, ok)
	assert.Equal(t, envSecret.ID, env1.SecretID)
	assert.EqualValues(t, 1, env1.Version)
	assert.Equal(t, "v-db_url", env1.Value)

	mcp, ok := findEntry(batch, SecretTypeMcpServer, "github-tools")
	require.True(t, ok)
	assert.Equal(t, "srv-1", mcp.SecretID)
	assert.EqualValues(t, 5, mcp.Version)
	var mcpMeta map[string]any
	require.NoError(t, json.Unmarshal(mcp.Metadata, &mcpMeta))
	assert.Equal(t, "stdio", mcpMeta["transport"])
	assert.Equal(t, []any{"-y", "pkg"}, mcpMeta["args"], "MCP metadata keeps stored native JSON types")

	assert.EqualValues(t, 1, batch.Revision.Seq, "first build mints seq 1")
	assert.NotEmpty(t, batch.Revision.ManifestHash)
	assert.Equal(t, BatchHash(*batch), batch.Revision.BatchHash)
}

// TestBuildWorkspaceBatch_OwnerNoKeys pins the quiet-degrade tier: an
// owner with no user_keys record owns no DEK-encrypted content, so the
// batch is NOT degraded in content terms but carries the machine-
// readable reason + per-entry skip audits.
func TestBuildWorkspaceBatch_OwnerNoKeys(t *testing.T) {
	rootProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0x2D))
	require.NoError(t, err)
	keySvc := NewKeyService(newMockKeyStore(), newMockDEKCache())
	keySvc.SetAPIKeyStore(nil, rootProv)

	env := &builderEnv{
		secrets:  newMockSecretStore(),
		revis:    &fakeRevisionStore{},
		adminKey: deterministicTestKey(t, 0xA1),
	}
	env.adminCred = CredentialBinding{
		ID: "cred-admin", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "admin-key"}),
		Version:    1, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}

	adminProv, err := NewStaticKeyProvider(env.adminKey)
	require.NoError(t, err)
	svc := NewSecretService(keySvc, env.store())
	svc.SetAdminProvider(adminProv)

	// Seed a user-DEK secret + binding directly in the store (no session
	// exists to create it through the service).
	secret := &UserSecret{
		ID: "sec-orphan", UserID: "user-1", Name: "orphan", Type: SecretTypeSSHKey,
		Ciphertext: []byte("wrapped-under-a-dek"), KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"key_type":"ed25519"}`),
	}
	require.NoError(t, env.secrets.CreateSecret(context.Background(), secret))
	require.NoError(t, env.secrets.SetBindings(context.Background(), "ws-1", []string{secret.ID}))

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeOwnerNoKeys, degrade.Reason)

	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.True(t, ok, "server-KEK entries still deliver")
	_, ok = findEntry(batch, SecretTypeSSHKey, "orphan")
	assert.False(t, ok, "user-DEK entry must not appear without a DEK")

	actions := auditActions(env.secrets)
	assert.Contains(t, actions, "secret_skipped_no_session", "skipped user secret must be audited")
	assert.NotContains(t, actions, "pod_bootstrap_dek_failed", "owner_no_keys is expected, not an infrastructure failure")
}

// TestBuildWorkspaceBatch_DEKUnwrapFailed_LoudDegrade pins the I10
// contract: an unwrappable user_keys row yields server-KEK entries +
// BuildDegrade{dek_unwrap_failed} + the pod_bootstrap_dek_failed audit.
func TestBuildWorkspaceBatch_DEKUnwrapFailed_LoudDegrade(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	alienProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xEE))
	require.NoError(t, err)
	garbage, err := alienProv.Encrypt(context.Background(), []byte("not-the-dek"))
	require.NoError(t, err)
	require.NoError(t, svc.keys.store.(*mockKeyStore).UpdateWrappedDEK(context.Background(), "user-1", garbage, nil, 1))

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "degrade is a result, not an error")
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeDEKUnwrapFailed, degrade.Reason)

	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.True(t, ok, "server-KEK entries still deliver")

	assert.Contains(t, auditActions(env.secrets), "pod_bootstrap_dek_failed",
		"the DEK degrade MUST be audited — silence was the 2026-08-28 diagnosis cost")
}

// TestBuildWorkspaceBatch_PerEntryFailure_StaysInManifest pins the
// loud-degrade manifest contract: a corrupted ciphertext leaves the
// entry in the manifest hash (the intended set) while omitting it from
// the batch, and healing the row (version bump) mints a new revision.
func TestBuildWorkspaceBatch_PerEntryFailure_StaysInManifest(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)

	good := addUserSecret(t, svc, sessionID, "good_secret")

	// A second secret whose ciphertext is encrypted under an alien DEK:
	// the DEK tier works, but this row's AEAD open fails.
	dek, err := svc.keys.cache.GetDEK(context.Background(), sessionID)
	require.NoError(t, err)
	_ = dek
	alienDEK := deterministicTestKey(t, 0x9A)
	badCipher, err := EncryptSecret(alienDEK, []byte("v-bad_secret"))
	require.NoError(t, err)

	bad := &UserSecret{
		ID: "sec-bad", UserID: "user-1", Name: "bad_secret", Type: SecretTypeEnvSecret,
		Ciphertext: badCipher, KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"var_name":"BAD"}`),
	}
	require.NoError(t, env.secrets.CreateSecret(context.Background(), bad))
	require.NoError(t, env.secrets.AddBindings(context.Background(), "ws-1", []string{bad.ID}))

	resetAudit(env.secrets)
	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade, "per-entry decrypt failure is an audit-and-continue, not a builder degrade")

	_, ok := findEntry(batch, SecretTypeEnvSecret, "good_secret")
	assert.True(t, ok, "healthy entries still deliver")
	_, ok = findEntry(batch, SecretTypeEnvSecret, "bad_secret")
	assert.False(t, ok, "corrupted entry must be omitted from the batch")
	assert.Contains(t, auditActions(env.secrets), "secret_decrypt_failed")

	// The manifest still describes the intended set: it contains the
	// failed entry's row. Heal the row (new value under the real DEK →
	// version bump) and the revision must move.
	withFailed := batch.Revision

	healed, err := EncryptSecret(dek, []byte("v-healed"))
	require.NoError(t, err)
	healedRow, err := env.secrets.GetSecret(context.Background(), "user-1", bad.ID)
	require.NoError(t, err)
	healedRow.Ciphertext = healed
	require.NoError(t, env.secrets.UpdateSecret(context.Background(), healedRow))

	healedBatch, degrade2, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade2)
	assert.NotEqual(t, withFailed.ManifestHash, healedBatch.Revision.ManifestHash,
		"healing bumps the row version → manifest hash changes")
	assert.EqualValues(t, withFailed.Seq+1, healedBatch.Revision.Seq, "changed manifest mints the next seq")

	fixed, ok := findEntry(healedBatch, SecretTypeEnvSecret, "bad_secret")
	require.True(t, ok, "healed entry delivers")
	assert.Equal(t, "v-healed", fixed.Value)
	_ = good
}

// TestBuildWorkspaceBatch_FallbackCredentialIdentifiedByWinnerRow: when
// the top-priority binding for a slug fails to decrypt and a
// lower-priority server-KEK binding for the same slug succeeds, the
// batch entry carries the winning row's identity (per-entry fallback
// semantics preserved from loadLLMCredentials).
func TestBuildWorkspaceBatch_FallbackCredentialIdentifiedByWinnerRow(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	userDEK := deterministicTestKey(t, 0x55)
	userCipher, err := EncryptSecret(userDEK, []byte(`{"kind":"openai","slug":"openai","apiKey":"user-key"}`))
	require.NoError(t, err)

	env.creds = &mockCredentialStore{bindings: []CredentialBinding{
		{ID: "cred-user", OwnerType: "user", OwnerID: "user-1", Kind: "openai", Slug: "openai", Ciphertext: userCipher, Version: 7, SourceType: "explicit", WithinPriority: 10},
		env.adminCred,
	}}
	svc.store = env.store()

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade, "the fallback decrypts, so nothing is missing")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Equal(t, "cred-admin", llm.SecretID, "batch entry identified by the row that actually decrypted")
	assert.EqualValues(t, env.adminCred.Version, llm.Version)
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`)
}

// TestBuildWorkspaceBatch_UserBindingDoesNotShadowServerKEK: a user
// binding skipped for DEK reasons must not block the lower-priority
// admin binding for the same slug from materializing.
func TestBuildWorkspaceBatch_UserBindingDoesNotShadowServerKEK(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{
		{ID: "cred-user", OwnerType: "user", OwnerID: "user-1", Kind: "openai", Slug: "openai", Ciphertext: []byte("user-dek-cipher"), Version: 1, SourceType: "explicit", WithinPriority: 10},
		env.adminCred,
	}}
	svc.store = env.store()

	// Remove the user_keys row entirely → owner_no_keys degrade, user
	// binding skipped.
	require.NoError(t, svc.keys.store.(*mockKeyStore).DeleteUserKey(context.Background(), "user-1"))

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeOwnerNoKeys, degrade.Reason)

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok, "admin credential must materialize as fallback")
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`)
}

// TestBuildWorkspaceBatch_UserScopeMCPNoDEK_SkippedWithAudit keeps the
// mcp_skipped_no_session vocabulary for user-scope servers on the
// DEK-degraded path.
func TestBuildWorkspaceBatch_UserScopeMCPNoDEK_SkippedWithAudit(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{
		bindings: []CredentialBinding{env.adminCred},
		mcpRows: []MCPServerBindingRow{{
			ServerID: "srv-user", Name: "user-mcp", Transport: "stdio", Command: "npx",
			Args: []string{}, Ciphertext: []byte("user-dek-cipher"), OwnerType: "user", KeyVersion: 1, Version: 1,
		}},
	}
	svc.store = env.store()
	require.NoError(t, svc.keys.store.(*mockKeyStore).DeleteUserKey(context.Background(), "user-1"))

	resetAudit(env.secrets)
	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade)

	_, ok := findEntry(batch, SecretTypeMcpServer, "user-mcp")
	assert.False(t, ok)
	assert.Contains(t, auditActions(env.secrets), "mcp_skipped_no_session")
}

// TestBuildWorkspaceBatch_NeedsNoSessionIdentity is the design-0052 §4.1
// pin: the builder produces the same batch for the same state whether
// or not a session ever existed — construction reads no session
// identity (no jwt_sessions store, no sessionID argument).
func TestBuildWorkspaceBatch_NeedsNoSessionIdentity(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	first, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)

	// Expire/evict every session-shaped state; the rebuild must be
	// byte-identical.
	require.NoError(t, svc.keys.cache.EvictDEK(context.Background(), sessionID))
	second, degrade2, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade2)
	assert.Equal(t, first.Revision, second.Revision)
	assert.Equal(t, string(LegacyBatchJSON(*first)), string(LegacyBatchJSON(*second)))
}

// TestBuildWorkspaceBatch_LegacyOrdering pins the mixed-fleet wire
// order: llm-provider entries first (binding priority order), then
// non-LLM user secrets, then MCP servers.
func TestBuildWorkspaceBatch_LegacyOrdering(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{
		bindings: []CredentialBinding{env.adminCred, env.orgCred},
		mcpRows: []MCPServerBindingRow{{
			ServerID: "srv-1", Name: "mcp", Transport: "stdio", Command: "x", Args: []string{},
			Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{}), OwnerType: "admin", KeyVersion: 1, Version: 1,
		}},
	}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)

	types := make([]SecretType, 0, len(batch.Entries))
	for _, e := range batch.Entries {
		types = append(types, e.Type)
	}
	assert.Equal(t, []SecretType{SecretTypeLLMProvider, SecretTypeLLMProvider, SecretTypeEnvSecret, SecretTypeMcpServer}, types)
}

// TestBatchEntryContract is the W12 enforcement: BatchEntry and
// ManifestEntry carry exactly the contract fields — no timestamps
// (compile-level: reflect finds no time.Time), every produced entry has
// a non-empty SecretID, a positive Version, and a known Type.
func TestBatchEntryContract(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(BatchEntry{}), reflect.TypeOf(ManifestEntry{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			assert.NotEqual(t, reflect.TypeOf(time.Time{}), f.Type,
				"%s.%s: entry contract forbids timestamped fields", typ.Name(), f.Name)
			if f.Type == reflect.TypeOf(json.RawMessage{}) {
				assert.Equal(t, "metadata", strings.Split(f.Tag.Get("json"), ",")[0],
					"%s.%s: raw JSON on an entry may only be metadata", typ.Name(), f.Name)
			}
		}
	}

	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)
	require.NotEmpty(t, batch.Entries)

	for _, e := range batch.Entries {
		assert.NotEmpty(t, e.SecretID, "entry %q must carry a non-empty secretId", e.Name)
		assert.Greater(t, e.Version, int64(0), "entry %q must carry a positive version", e.Name)
		assert.True(t, ValidSecretTypes[e.Type], "entry %q has type %q outside the known enum", e.Name, e.Type)
	}
}

// TestBuildWorkspaceBatch_StoreWithoutRevisionStoreErrors: a store that
// cannot mint revisions must fail loudly, never silently fabricate one.
func TestBuildWorkspaceBatch_StoreWithoutRevisionStoreErrors(t *testing.T) {
	svc, _, _ := setupBuilder(t)
	creds := &mockCredentialStore{}
	svc.store = revisionlessStore{SecretStore: newMockSecretStore(), CredentialStore: creds}

	_, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RevisionStore")
}

// revisionlessStore delegates SecretStore + CredentialStore but
// structurally cannot satisfy RevisionStore — the loud-failure fixture
// for the builder's revision cast.
type revisionlessStore struct {
	SecretStore
	CredentialStore
}

// TestBuildWorkspaceBatch_EmptyWorkspace pins the quiet tier (law 5):
// no bindings at all → empty batch, no degrade, revision still minted.
func TestBuildWorkspaceBatch_EmptyWorkspace(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{}
	svc.store = env.store()

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade, "owner-with-nothing-bound is the one quiet case")
	assert.Empty(t, batch.Entries)
	assert.Equal(t, []byte("[]"), LegacyBatchJSON(*batch))
	assert.EqualValues(t, 1, batch.Revision.Seq)
}

// TestBuildWorkspaceBatch_ManifestTierNeedsNoDecrypts pins epic #1158's
// AC "the manifest tier computes with zero decrypts": the revision
// (seq + manifest hash) is minted from rows BEFORE any ciphertext is
// touched, so a total DEK failure yields the SAME manifest hash and
// seq as the healthy build — only the batch values degrade.
func TestBuildWorkspaceBatch_ManifestTierNeedsNoDecrypts(t *testing.T) {
	svc, env, sessionID := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred}}
	svc.store = env.store()
	addUserSecret(t, svc, sessionID, "db_url")

	healthy, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)
	require.Len(t, healthy.Entries, 2)

	// Destroy the DEK tier completely: alien wrap, no session source.
	alienProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xED))
	require.NoError(t, err)
	garbage, err := alienProv.Encrypt(context.Background(), []byte("not-the-dek"))
	require.NoError(t, err)
	require.NoError(t, svc.keys.store.(*mockKeyStore).UpdateWrappedDEK(context.Background(), "user-1", garbage, nil, 1))

	degraded, degrade2, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade2)
	require.Equal(t, DegradeDEKUnwrapFailed, degrade2.Reason)

	assert.Equal(t, healthy.Revision.Seq, degraded.Revision.Seq,
		"identical rows ⇒ identical manifest ⇒ no new seq, even though no DEK exists")
	assert.Equal(t, healthy.Revision.ManifestHash, degraded.Revision.ManifestHash,
		"the manifest tier never decrypts — the revision describes the intended set")
	assert.NotEqual(t, healthy.Revision.BatchHash, degraded.Revision.BatchHash,
		"the batch tier DOES degrade (user entry value absent)")
	require.Len(t, degraded.Entries, 1, "only the server-KEK entry delivers")
}

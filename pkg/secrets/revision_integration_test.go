// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration
// +build integration

package secrets

// revision_integration_test.go — PG-backed tests for US-70.2's
// server-side core: per-row version counters, the workspace revision
// row (DB-as-single-writer seq mint), and the one builder end-to-end
// against real Postgres rows.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupRevisionRows(t *testing.T, pool *pgxpool.Pool, workspaceID string) {
	t.Helper()
	ctx := context.Background()
	pool.Exec(ctx, `DELETE FROM workspace_secret_revisions WHERE workspace_id = $1`, workspaceID)
	pool.Exec(ctx, `DELETE FROM user_secret_bindings WHERE workspace_id = $1`, workspaceID)
	pool.Exec(ctx, `DELETE FROM workspace_credential_bindings WHERE workspace_id = $1`, workspaceID)
	pool.Exec(ctx, `DELETE FROM mcp_server_bindings WHERE workspace_id = $1`, workspaceID)
	pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, workspaceID)
}

// --- Version-counter discipline ---

func TestVersionCounters_CreateStartsAtOne_UpdateBumps(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	userID := "ver-test-" + uuid.NewString()[:8]
	ensureTestUser(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	created := &UserSecret{
		UserID:     userID,
		Name:       "ver-secret",
		Type:       SecretTypeEnvSecret,
		Ciphertext: []byte("cipher-bytes"),
		KeyVersion: 1,
		Metadata:   json.RawMessage(`{"var_name":"V"}`),
	}
	require.NoError(t, store.CreateSecret(ctx, created), "CreateSecret must insert")

	got, err := store.GetSecret(ctx, userID, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.EqualValues(t, 1, got.Version, "create must start at version 1")

	got.Ciphertext = []byte("rotated-cipher")
	got.UpdatedAt = time.Now()
	require.NoError(t, store.UpdateSecret(ctx, got), "UpdateSecret must succeed")

	reread, err := store.GetSecret(ctx, userID, created.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, reread.Version, "every value update must bump version by exactly 1")
}

func TestVersionCounters_CredentialUpdateBumps(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	orgID := "org-ver-" + uuid.NewString()[:8]
	created := &CredentialRow{
		ID:         uuid.NewString(),
		Name:       "ver-cred",
		Kind:       "openai",
		Slug:       "ver-cred",
		Ciphertext: []byte("cipher"),
	}
	require.NoError(t, store.CreateCredential(ctx, "org", orgID, created))
	defer pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id = $1`, created.ID)

	got, err := store.GetCredential(ctx, "org", orgID, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.EqualValues(t, 1, got.Version)

	got.Ciphertext = []byte("rotated")
	require.NoError(t, store.UpdateCredential(ctx, "org", orgID, created.ID, got))
	reread, err := store.GetCredential(ctx, "org", orgID, created.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, reread.Version, "UpdateCredential must bump version")
}

func TestVersionCounters_MCPServerUpdateBumps(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	serverID := uuid.NewString()
	now := time.Now()
	created := &MCPServerRow{
		ID: serverID, OwnerType: "admin", OwnerID: "_platform",
		Name: "ver-mcp", Transport: "stdio", Command: "npx",
		Args: []string{"pkg"}, Ciphertext: []byte("cipher"),
		KeyVersion: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.CreateMCPServer(ctx, created))
	defer pool.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, serverID)

	got, err := store.GetMCPServer(ctx, "admin", "_platform", serverID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.EqualValues(t, 1, got.Version)

	got.Command = "node"
	require.NoError(t, store.UpdateMCPServer(ctx, "admin", "_platform", serverID, got))
	reread, err := store.GetMCPServer(ctx, "admin", "_platform", serverID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, reread.Version, "UpdateMCPServer must bump version")
}

// --- Revision row: CurrentRevision / EnsureRevision ---

func TestEnsureRevision_FreshWorkspaceSeqOne(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	wsID := uuid.NewString()
	defer cleanupRevisionRows(t, pool, wsID)

	_, _, ok, err := store.CurrentRevision(ctx, wsID)
	require.NoError(t, err)
	assert.False(t, ok, "unknown workspace must report ok=false, not an error")

	seq, err := store.EnsureRevision(ctx, wsID, "hash-a")
	require.NoError(t, err)
	assert.EqualValues(t, 1, seq, "first-ever revision must mint seq 1")

	curSeq, curHash, ok, err := store.CurrentRevision(ctx, wsID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 1, curSeq)
	assert.Equal(t, "hash-a", curHash)
}

func TestEnsureRevision_SameHashNoBump_ChangedHashBumps(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	wsID := uuid.NewString()
	defer cleanupRevisionRows(t, pool, wsID)

	seq, err := store.EnsureRevision(ctx, wsID, "hash-a")
	require.NoError(t, err)
	require.EqualValues(t, 1, seq)

	again, err := store.EnsureRevision(ctx, wsID, "hash-a")
	require.NoError(t, err)
	assert.EqualValues(t, 1, again, "identical content already current → seq unchanged")

	changed, err := store.EnsureRevision(ctx, wsID, "hash-b")
	require.NoError(t, err)
	assert.EqualValues(t, 2, changed, "changed manifest → seq +1")
}

// TestEnsureRevision_ConcurrentDistinctHashes_AllSucceed is the
// DB-as-single-writer property: concurrent builders racing different
// manifests must all converge with distinct, monotonic seqs — no
// duplicate seq is ever handed out. Run under -race.
func TestEnsureRevision_ConcurrentDistinctHashes_AllSucceed(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := NewPgSecretStore(pool)

	wsID := uuid.NewString()
	defer cleanupRevisionRows(t, pool, wsID)

	const racers = 8
	results := make([]int64, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			seq, err := store.EnsureRevision(ctx, wsID, fmt.Sprintf("hash-racer-%d", i))
			results[i] = seq
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int64]bool, racers)
	var maxSeq int64
	for i := 0; i < racers; i++ {
		require.NoError(t, errs[i], "racer %d must converge", i)
		assert.False(t, seen[results[i]], "duplicate seq %d handed out (racer %d)", results[i], i)
		seen[results[i]] = true
		if results[i] > maxSeq {
			maxSeq = results[i]
		}
	}
	curSeq, _, ok, err := store.CurrentRevision(ctx, wsID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.EqualValues(t, maxSeq, curSeq, "stored seq must equal the highest minted seq")
	assert.Equal(t, racers, len(seen), "all %d racers must have received distinct seqs", racers)
}

// --- Builder end-to-end against PG ---

// builderFixture wires a SecretService over real PG stores with a real
// static RootKeyProvider, one workspace, and an in-memory DEK cache.
type builderFixture struct {
	pool     *pgxpool.Pool
	store    *PgSecretStore
	svc      *SecretService
	keySvc   *KeyService
	keyStore *mockKeyStore
	userID   string
	wsID     string
	adminKEK []byte
}

func setupBuilderFixture(t *testing.T) *builderFixture {
	t.Helper()
	pool := getTestPool(t)
	store := NewPgSecretStore(pool)

	f := &builderFixture{
		pool:     pool,
		store:    store,
		userID:   "bld-" + uuid.NewString()[:12],
		wsID:     uuid.NewString(),
		adminKEK: deterministicTestKey(t, 0xB1),
	}

	ensureTestUser(t, pool, f.userID)
	ensureTestWorkspace(t, pool, f.wsID, f.userID)
	t.Cleanup(func() {
		cleanupRevisionRows(t, pool, f.wsID)
		cleanupSecrets(t, pool, f.userID)
		pool.Exec(context.Background(), `DELETE FROM provider_credentials WHERE owner_id = $1`, "bld-admin-"+f.userID)
		pool.Exec(context.Background(), `DELETE FROM mcp_servers WHERE owner_id = $1`, "bld-admin-"+f.userID)
		pool.Close()
	})

	adminProv, err := NewStaticKeyProvider(f.adminKEK)
	require.NoError(t, err)

	f.keyStore = newMockKeyStore()
	f.keySvc = NewKeyService(f.keyStore, newMockDEKCache())
	f.keySvc.SetAPIKeyStore(nil, adminProv)

	f.svc = NewSecretService(f.keySvc, store)
	f.svc.SetAdminProvider(adminProv)

	ctx := context.Background()
	require.NoError(t, f.keySvc.InitializeUserKeysServerKEK(ctx, f.userID, "server_kek"))
	require.NoError(t, f.keySvc.UnlockDEK(ctx, f.userID, []byte("pw"), "bld-session-"+f.userID, time.Hour))
	return f
}

func (f *builderFixture) bindAdminCredential(t *testing.T, slug string) string {
	t.Helper()
	ctx := context.Background()
	plaintext, err := json.Marshal(LLMProviderData{Kind: "openai", Slug: slug, APIKey: "admin-key"})
	require.NoError(t, err)
	cipher, err := EncryptSecret(f.adminKEK, plaintext)
	require.NoError(t, err)

	credID := uuid.NewString()
	row := &CredentialRow{
		ID: credID, Name: slug, Kind: "openai", Slug: slug,
		Ciphertext: cipher, KeyVersion: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	adminOwner := "bld-admin-" + f.userID
	require.NoError(t, f.store.CreateCredential(ctx, "admin", adminOwner, row))
	require.NoError(t, f.store.BindCredentialToWorkspace(ctx, credID, f.wsID))
	return credID
}

func (f *builderFixture) bindEnvSecret(t *testing.T, name string) *SecretResponse {
	t.Helper()
	ctx := context.Background()
	s, err := f.svc.CreateSecret(ctx, f.userID, "bld-session-"+f.userID, nil, CreateSecretRequest{
		Name:     name,
		Type:     SecretTypeEnvSecret,
		Value:    "v-" + name,
		Metadata: json.RawMessage(`{"var_name":"` + name + `"}`),
	})
	require.NoError(t, err)
	_, err = f.svc.SetBindings(ctx, f.userID, f.wsID, []string{s.ID})
	require.NoError(t, err)
	return s
}

func (f *builderFixture) bindAdminMCPServer(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	cipher, err := EncryptSecret(f.adminKEK, []byte(`{"env":{},"headers":{}}`))
	require.NoError(t, err)
	serverID := uuid.NewString()
	now := time.Now()
	row := &MCPServerRow{
		ID: serverID, OwnerType: "admin", OwnerID: "bld-admin-" + f.userID,
		Name: name, Transport: "stdio", Command: "npx", Args: []string{"pkg"},
		Ciphertext: cipher, KeyVersion: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, f.store.CreateMCPServer(ctx, row))
	require.NoError(t, f.store.BindMCPServerToWorkspace(ctx, serverID, f.wsID))
	return serverID
}

// TestBuildWorkspaceBatch_PG_EndToEnd binds one secret of each class,
// builds, verifies identification + revision, then proves the revision
// invariants: mutate → hash change + seq bump; unchanged rebuild →
// identical revision (I2/I5).
func TestBuildWorkspaceBatch_PG_EndToEnd(t *testing.T) {
	f := setupBuilderFixture(t)
	ctx := context.Background()

	credID := f.bindAdminCredential(t, "openai")
	secret := f.bindEnvSecret(t, "db_url")
	serverID := f.bindAdminMCPServer(t, "github-tools")

	batch, degrade, err := f.svc.BuildWorkspaceBatch(ctx, f.userID, f.wsID)
	require.NoError(t, err)
	require.Nil(t, degrade, "fully decryptable workspace must not degrade")
	require.Len(t, batch.Entries, 3)
	assert.EqualValues(t, 1, batch.Revision.Seq, "first build mints seq 1")

	byType := map[SecretType]BatchEntry{}
	for _, e := range batch.Entries {
		byType[e.Type] = e
	}
	llm := byType[SecretTypeLLMProvider]
	assert.Equal(t, credID, llm.SecretID, "llm-provider entry identified by the credential row's ID")
	assert.Equal(t, "openai", llm.Name, "llm-provider entry named by the slug")
	assert.EqualValues(t, 1, llm.Version)
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`)

	env := byType[SecretTypeEnvSecret]
	assert.Equal(t, secret.ID, env.SecretID)
	assert.EqualValues(t, 1, env.Version)
	assert.Equal(t, "v-db_url", env.Value)

	mcp := byType[SecretTypeMcpServer]
	assert.Equal(t, serverID, mcp.SecretID)
	assert.EqualValues(t, 1, mcp.Version)
	assert.Equal(t, "github-tools", mcp.Name)
	var mcpMeta map[string]any
	require.NoError(t, json.Unmarshal(mcp.Metadata, &mcpMeta))
	assert.Equal(t, "stdio", mcpMeta["transport"])

	firstRevision := batch.Revision

	rev, err := f.store.EnsureRevision(ctx, f.wsID, firstRevision.ManifestHash)
	require.NoError(t, err)
	assert.EqualValues(t, 1, rev, "rebuild with unchanged state must not mint a new seq")

	// Mutate the env-secret's value: manifest hash must change and the
	// seq must bump.
	require.NoError(t, f.svc.UpdateSecret(ctx, f.userID, "bld-session-"+f.userID, nil, secret.ID,
		UpdateSecretRequest{Value: "v2-db_url"}))

	batch2, degrade2, err := f.svc.BuildWorkspaceBatch(ctx, f.userID, f.wsID)
	require.NoError(t, err)
	require.Nil(t, degrade2)
	assert.EqualValues(t, 2, batch2.Revision.Seq, "value mutation → version bump → new manifest hash → seq +1")
	assert.NotEqual(t, firstRevision.ManifestHash, batch2.Revision.ManifestHash)
	assert.NotEqual(t, firstRevision.BatchHash, batch2.Revision.BatchHash)

	// Identical state rebuilt again → identical revision.
	batch3, degrade3, err := f.svc.BuildWorkspaceBatch(ctx, f.userID, f.wsID)
	require.NoError(t, err)
	require.Nil(t, degrade3)
	assert.Equal(t, batch2.Revision, batch3.Revision, "I2/I5: identical inputs → identical revision")
	assert.Equal(t, batch2.Revision.BatchHash, batch3.Revision.BatchHash)
}

// TestBuildWorkspaceBatch_PG_CorruptedDEK_DegradesLoudly pins the I10
// contract on real rows: an unwrappable user_keys row yields the
// server-KEK entries + BuildDegrade{dek_unwrap_failed} + the
// pod_bootstrap_dek_failed audit row — never a silent partial.
func TestBuildWorkspaceBatch_PG_CorruptedDEK_DegradesLoudly(t *testing.T) {
	f := setupBuilderFixture(t)
	ctx := context.Background()

	f.bindAdminCredential(t, "openai")
	f.bindEnvSecret(t, "db_url")

	// Corrupt the user_keys row under an alien key so the master
	// provider genuinely cannot unwrap it, and the heal path has no
	// jwt_sessions source to fall back on.
	alienProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xEF))
	require.NoError(t, err)
	garbage, err := alienProv.Encrypt(ctx, []byte("not-the-dek"))
	require.NoError(t, err)
	require.NoError(t, f.keyStore.UpdateWrappedDEK(ctx, f.userID, garbage, nil, 1))

	pool := f.pool
	pool.Exec(ctx, `DELETE FROM secret_audit_log WHERE user_id = $1`, f.userID)

	batch, degrade, err := f.svc.BuildWorkspaceBatch(ctx, f.userID, f.wsID)
	require.NoError(t, err, "degrade is a result, not an error")
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeDEKUnwrapFailed, degrade.Reason)

	require.Len(t, batch.Entries, 1, "server-KEK entries still deliver")
	assert.Equal(t, SecretTypeLLMProvider, batch.Entries[0].Type)

	audits, err := f.store.QueryAudit(ctx, f.userID, AuditQuery{WorkspaceID: f.wsID, Limit: 50})
	require.NoError(t, err)
	var sawDEKFailure bool
	for _, a := range audits {
		if a.Action == "pod_bootstrap_dek_failed" {
			sawDEKFailure = true
		}
	}
	assert.True(t, sawDEKFailure, "the DEK degrade MUST be audited (I10)")
}

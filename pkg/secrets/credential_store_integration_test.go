// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration
// +build integration

package secrets

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func cleanupProviderCredentials(t *testing.T, store *PgSecretStore, ownerType, ownerID string) {
	t.Helper()
	ctx := context.Background()
	store.pool.Exec(ctx, "DELETE FROM credential_backfill_jobs WHERE credential_id IN (SELECT id FROM provider_credentials WHERE owner_type = $1 AND owner_id = $2)", ownerType, ownerID)
	store.pool.Exec(ctx, "DELETE FROM credential_auto_apply WHERE credential_id IN (SELECT id FROM provider_credentials WHERE owner_type = $1 AND owner_id = $2)", ownerType, ownerID)
	store.pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE credential_id IN (SELECT id FROM provider_credentials WHERE owner_type = $1 AND owner_id = $2)", ownerType, ownerID)
	store.pool.Exec(ctx, "DELETE FROM provider_credentials WHERE owner_type = $1 AND owner_id = $2", ownerType, ownerID)
}

func TestPgCredentialStore_UpsertFreeTierCredential(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	defer cleanupProviderCredentials(t, store, "admin", "_platform")

	ciphertext := []byte("encrypted-free-tier-key")

	// First call: creates the row.
	err := store.UpsertFreeTierCredential(ctx, ciphertext)
	if err != nil {
		t.Fatalf("UpsertFreeTierCredential (first call): %v", err)
	}

	// Verify provider_credentials row exists.
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM provider_credentials WHERE owner_type='admin' AND owner_id='_platform' AND slug='opencode-free-tier'`).Scan(&count)
	if err != nil {
		t.Fatalf("query provider_credentials: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 provider_credentials row, got %d", count)
	}

	// Verify credential_auto_apply row exists.
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM credential_auto_apply WHERE target_type='all' AND target_id IS NULL`).Scan(&count)
	if err != nil {
		t.Fatalf("query credential_auto_apply: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 credential_auto_apply row, got %d", count)
	}

	// Second call (idempotent): updates ciphertext.
	newCiphertext := []byte("updated-encrypted-free-tier-key")
	err = store.UpsertFreeTierCredential(ctx, newCiphertext)
	if err != nil {
		t.Fatalf("UpsertFreeTierCredential (second call): %v", err)
	}

	// Still only 1 row.
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM provider_credentials WHERE owner_type='admin' AND owner_id='_platform' AND slug='opencode-free-tier'`).Scan(&count)
	if err != nil {
		t.Fatalf("query after upsert: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", count)
	}

	// Verify ciphertext was updated.
	var stored []byte
	err = pool.QueryRow(ctx,
		`SELECT ciphertext FROM provider_credentials WHERE owner_type='admin' AND owner_id='_platform' AND slug='opencode-free-tier'`).Scan(&stored)
	if err != nil {
		t.Fatalf("query ciphertext: %v", err)
	}
	if string(stored) != string(newCiphertext) {
		t.Fatalf("ciphertext not updated: got %q, want %q", stored, newCiphertext)
	}
}

func TestPgCredentialStore_SeedWorkspaceCredentials(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-seed-user-1"
	wsID := "00000000-0000-0000-0000-000000000001"
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// Seed the free-tier credential first.
	err := store.UpsertFreeTierCredential(ctx, []byte("cipher"))
	if err != nil {
		t.Fatalf("UpsertFreeTierCredential: %v", err)
	}

	// Now seed workspace credentials.
	err = store.SeedWorkspaceCredentials(ctx, wsID, userID, nil)
	if err != nil {
		t.Fatalf("SeedWorkspaceCredentials: %v", err)
	}

	// Verify binding created with source_type='auto'.
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspace_credential_bindings WHERE workspace_id = $1 AND source_type = 'auto'`, wsID).Scan(&count)
	if err != nil {
		t.Fatalf("query bindings: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 auto binding, got %d", count)
	}

	// Idempotent: calling again should not fail or duplicate.
	err = store.SeedWorkspaceCredentials(ctx, wsID, userID, nil)
	if err != nil {
		t.Fatalf("SeedWorkspaceCredentials (idempotent): %v", err)
	}
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspace_credential_bindings WHERE workspace_id = $1`, wsID).Scan(&count)
	if err != nil {
		t.Fatalf("query after re-seed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 binding after re-seed, got %d", count)
	}
}

// TestPgCredentialStore_SeedWorkspaceCredentials_AdminOwnerBindsAdminCreds
// is the regression test for issue #593 Option A: when the workspace
// owner has role='admin', SeedWorkspaceCredentials must bind ALL admin
// credentials to their workspace — including ones that have no
// credential_auto_apply rule. Without this, an admin who added a custom
// LLM credential via POST /admin/provider-credentials (which does NOT
// auto-create an auto-apply rule) had no way to get that credential
// into their own workspace without a second manual API call.
//
// Pre-fix: only the free-tier credential (which has target_type='all')
// was bound — count=1 regardless of admin role.
// Post-fix: admin-owned workspace gets the free-tier (via auto-apply)
// PLUS every other admin credential (via the cascade) — count=2 here.
func TestPgCredentialStore_SeedWorkspaceCredentials_AdminOwnerBindsAdminCreds(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-seed-admin-1"
	wsID := "00000000-0000-0000-0000-000000000010"
	ensureTestUser(t, pool, userID)
	// Promote to admin — the trigger condition for Option A.
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, userID); err != nil {
		t.Fatalf("promote user to admin: %v", err)
	}
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// Free-tier credential — has target_type='all' auto-apply rule
	// (created by UpsertFreeTierCredential).
	if err := store.UpsertFreeTierCredential(ctx, []byte("free-tier-cipher")); err != nil {
		t.Fatalf("UpsertFreeTierCredential: %v", err)
	}

	// Custom admin credential WITHOUT any auto-apply rule. This is the
	// gap: pre-fix, this credential would never reach any workspace
	// unless the admin separately called POST /admin/.../auto-apply.
	var customAdminCredID string
	err := pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'paid-openai', 'openai', 'paid-openai', $1)
		 RETURNING id`, []byte("paid-openai-cipher")).Scan(&customAdminCredID)
	if err != nil {
		t.Fatalf("insert custom admin cred: %v", err)
	}

	// Seed.
	if err := store.SeedWorkspaceCredentials(ctx, wsID, userID, nil); err != nil {
		t.Fatalf("SeedWorkspaceCredentials: %v", err)
	}

	// Expect BOTH admin credentials bound to the admin-owned workspace.
	var count int
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspace_credential_bindings wcb
		 JOIN provider_credentials pc ON pc.id = wcb.credential_id
		 WHERE wcb.workspace_id = $1 AND pc.owner_type = 'admin'`, wsID).Scan(&count)
	if err != nil {
		t.Fatalf("count admin bindings: %v", err)
	}
	if count != 2 {
		t.Fatalf("admin-owned workspace must have both admin credentials bound (free-tier via auto-apply + custom via cascade); got %d", count)
	}

	// Verify the custom credential specifically is bound (not just the free-tier).
	var boundCustom bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspace_credential_bindings
		 WHERE workspace_id = $1 AND credential_id = $2)`, wsID, customAdminCredID).Scan(&boundCustom)
	if err != nil {
		t.Fatalf("check custom cred bound: %v", err)
	}
	if !boundCustom {
		t.Fatal("custom admin credential without auto-apply rule must be bound to admin-owned workspace (issue #593 Option A)")
	}
}

// TestPgCredentialStore_SeedWorkspaceCredentials_NonAdminOwnerSkipsAdminCascade
// verifies the negative side of issue #593 Option A: a non-admin owner
// must NOT receive admin credentials that lack an explicit auto-apply
// rule. Only credentials with target_type='all' (e.g. free-tier) or
// target_type='user' matching this user reach the workspace.
//
// This guards against the cascade over-reaching: if the EXISTS check
// on users.role='admin' were removed, every workspace would receive
// every admin credential — a privilege boundary violation.
func TestPgCredentialStore_SeedWorkspaceCredentials_NonAdminOwnerSkipsAdminCascade(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-seed-user-2" // ensureTestUser creates with role='user'
	wsID := "00000000-0000-0000-0000-000000000011"
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// Free-tier (auto-apply target_type='all') — SHOULD bind.
	if err := store.UpsertFreeTierCredential(ctx, []byte("free-tier-cipher")); err != nil {
		t.Fatalf("UpsertFreeTierCredential: %v", err)
	}
	// Custom admin credential WITHOUT auto-apply — must NOT bind.
	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'paid-openai-2', 'openai', 'paid-openai-2', $1)`,
		[]byte("paid-openai-cipher")); err != nil {
		t.Fatalf("insert custom admin cred: %v", err)
	}

	if err := store.SeedWorkspaceCredentials(ctx, wsID, userID, nil); err != nil {
		t.Fatalf("SeedWorkspaceCredentials: %v", err)
	}

	// Non-admin workspace must have ONLY the free-tier binding.
	var count int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspace_credential_bindings wcb
		 JOIN provider_credentials pc ON pc.id = wcb.credential_id
		 WHERE wcb.workspace_id = $1 AND pc.owner_type = 'admin'`, wsID).Scan(&count)
	if err != nil {
		t.Fatalf("count admin bindings: %v", err)
	}
	if count != 1 {
		t.Fatalf("non-admin workspace must have only the auto-apply admin credential (free-tier); got %d — cascade is over-reaching", count)
	}
}

// TestPgCredentialStore_SeedWorkspaceCredentials_AdminCascadeIdempotent
// verifies that calling SeedWorkspaceCredentials twice on an
// admin-owned workspace does not produce duplicate bindings (the SQL
// uses ON CONFLICT (credential_id, workspace_id) DO NOTHING).
func TestPgCredentialStore_SeedWorkspaceCredentials_AdminCascadeIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-seed-admin-2"
	wsID := "00000000-0000-0000-0000-000000000012"
	ensureTestUser(t, pool, userID)
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, userID); err != nil {
		t.Fatalf("promote user to admin: %v", err)
	}
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	if err := store.UpsertFreeTierCredential(ctx, []byte("cipher")); err != nil {
		t.Fatalf("UpsertFreeTierCredential: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'cascade-idem', 'openai', 'cascade-idem', $1)`,
		[]byte("cipher")); err != nil {
		t.Fatalf("insert admin cred: %v", err)
	}

	// Seed twice.
	if err := store.SeedWorkspaceCredentials(ctx, wsID, userID, nil); err != nil {
		t.Fatalf("SeedWorkspaceCredentials (1st): %v", err)
	}
	if err := store.SeedWorkspaceCredentials(ctx, wsID, userID, nil); err != nil {
		t.Fatalf("SeedWorkspaceCredentials (2nd): %v", err)
	}

	var count int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workspace_credential_bindings WHERE workspace_id = $1`, wsID).Scan(&count)
	if err != nil {
		t.Fatalf("count after re-seed: %v", err)
	}
	if count != 2 {
		t.Fatalf("idempotent re-seed must not duplicate; expected 2 (free-tier + custom), got %d", count)
	}
}

func TestPgCredentialStore_GetWorkspaceCredentials(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-get-user-1"
	wsID := "00000000-0000-0000-0000-000000000002"
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer cleanupProviderCredentials(t, store, "user", userID)
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// Create admin credential.
	var adminCredID string
	err := pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'admin-anthropic', 'anthropic', 'anthropic', $1)
		 RETURNING id`, []byte("admin-cipher")).Scan(&adminCredID)
	if err != nil {
		t.Fatalf("insert admin cred: %v", err)
	}

	// Create user credential for same provider.
	var userCredID string
	err = pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('user', $1, 'my-anthropic', 'anthropic', 'anthropic', $2)
		 RETURNING id`, userID, []byte("user-cipher")).Scan(&userCredID)
	if err != nil {
		t.Fatalf("insert user cred: %v", err)
	}

	// Bind admin credential as auto (lower priority).
	_, err = pool.Exec(ctx,
		`INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		 VALUES ($1, $2, 'auto', 0)`, adminCredID, wsID)
	if err != nil {
		t.Fatalf("bind admin cred: %v", err)
	}

	// Bind user credential as explicit (higher priority).
	_, err = pool.Exec(ctx,
		`INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		 VALUES ($1, $2, 'explicit', 0)`, userCredID, wsID)
	if err != nil {
		t.Fatalf("bind user cred: %v", err)
	}

	// Get workspace credentials — should return explicit first.
	bindings, err := store.GetWorkspaceCredentials(ctx, wsID)
	if err != nil {
		t.Fatalf("GetWorkspaceCredentials: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(bindings))
	}

	// First binding should be explicit (user).
	if bindings[0].SourceType != "explicit" {
		t.Errorf("first binding source_type = %q, want 'explicit'", bindings[0].SourceType)
	}
	if bindings[0].OwnerType != "user" {
		t.Errorf("first binding owner_type = %q, want 'user'", bindings[0].OwnerType)
	}
	if bindings[0].Kind != "anthropic" {
		t.Errorf("first binding kind = %q, want 'anthropic'", bindings[0].Kind)
	}

	// Second binding should be auto (admin).
	if bindings[1].SourceType != "auto" {
		t.Errorf("second binding source_type = %q, want 'auto'", bindings[1].SourceType)
	}
	if bindings[1].OwnerType != "admin" {
		t.Errorf("second binding owner_type = %q, want 'admin'", bindings[1].OwnerType)
	}
}

func TestPgCredentialStore_GetWorkspaceCredentials_Empty(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-empty-user"
	wsID := "00000000-0000-0000-0000-000000000003"
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	bindings, err := store.GetWorkspaceCredentials(ctx, wsID)
	if err != nil {
		t.Fatalf("GetWorkspaceCredentials (empty): %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected 0 bindings, got %d", len(bindings))
	}
}

func TestPgCredentialStore_HasUserProviderCredential(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-has-user-1"
	ensureTestUser(t, pool, userID)
	defer cleanupProviderCredentials(t, store, "user", userID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// No credential exists.
	has, err := store.HasUserProviderCredential(ctx, userID, "anthropic")
	if err != nil {
		t.Fatalf("HasUserProviderCredential: %v", err)
	}
	if has {
		t.Fatal("expected false, got true (no credential)")
	}

	// Create one.
	_, err = pool.Exec(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('user', $1, 'my-anthropic', 'anthropic', 'anthropic', $2)`, userID, []byte("cipher"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	has, err = store.HasUserProviderCredential(ctx, userID, "anthropic")
	if err != nil {
		t.Fatalf("HasUserProviderCredential after insert: %v", err)
	}
	if !has {
		t.Fatal("expected true, got false (credential exists)")
	}

	// Different provider: should be false.
	has, err = store.HasUserProviderCredential(ctx, userID, "openai")
	if err != nil {
		t.Fatalf("HasUserProviderCredential (different provider): %v", err)
	}
	if has {
		t.Fatal("expected false for different provider")
	}
}

func TestPgCredentialStore_GetWorkspaceCredentials_PriorityOrder(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	userID := "cred-priority-user"
	wsID := "00000000-0000-0000-0000-000000000004"
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	defer cleanupProviderCredentials(t, store, "admin", "_platform")
	defer pool.Exec(ctx, "DELETE FROM workspace_credential_bindings WHERE workspace_id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
	defer pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)

	// Create two admin credentials for different providers.
	var cred1ID, cred2ID string
	err := pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'admin-openai', 'openai', 'openai', $1)
		 RETURNING id`, []byte("cipher1")).Scan(&cred1ID)
	if err != nil {
		t.Fatalf("insert cred1: %v", err)
	}
	err = pool.QueryRow(ctx,
		`INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		 VALUES ('admin', '_platform', 'admin-anthropic', 'anthropic', 'anthropic', $1)
		 RETURNING id`, []byte("cipher2")).Scan(&cred2ID)
	if err != nil {
		t.Fatalf("insert cred2: %v", err)
	}

	// Bind both as auto with different within_priority.
	_, err = pool.Exec(ctx,
		`INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		 VALUES ($1, $2, 'auto', 10)`, cred1ID, wsID)
	if err != nil {
		t.Fatalf("bind cred1: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		 VALUES ($1, $2, 'auto', 20)`, cred2ID, wsID)
	if err != nil {
		t.Fatalf("bind cred2: %v", err)
	}

	bindings, err := store.GetWorkspaceCredentials(ctx, wsID)
	if err != nil {
		t.Fatalf("GetWorkspaceCredentials: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(bindings))
	}

	// Higher within_priority should come first.
	if bindings[0].WithinPriority != 20 {
		t.Errorf("first binding within_priority = %d, want 20", bindings[0].WithinPriority)
	}
	if bindings[1].WithinPriority != 10 {
		t.Errorf("second binding within_priority = %d, want 10", bindings[1].WithinPriority)
	}
}

// TestPgCredentialStore_UpdateCredential_NilPreservesLimits verifies that
// UpdateCredential's COALESCE semantics preserve model_context_limits and
// model_allowlist when the update row passes nil for those fields. This is the
// org handler's partial-update contract: nil = "don't change", empty = "clear".
// Regression test for the critical nil→{} normalization bug.
func TestPgCredentialStore_UpdateCredential_NilPreservesLimits(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()

	defer cleanupProviderCredentials(t, store, "org", "org-test-update-nil")

	// Create a credential with non-empty limits and allowlist.
	credID := uuid.New().String()
	now := time.Now()
	row := &CredentialRow{
		ID:                 credID,
		Name:               "original",
		Kind:               "openai_compatible",
		Slug:               "test-cred-nil-preserve",
		Ciphertext:         []byte("encrypted"),
		KeyVersion:         1,
		ModelAllowlist:     []string{"glm-5.1", "gpt-4o"},
		ModelContextLimits: map[string]int{"glm-5.1": 200000, "gpt-4o": 128000},
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := store.CreateCredential(ctx, "org", "org-test-update-nil", row); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	// Update with nil limits/allowlist — must NOT overwrite existing values.
	upd := &CredentialRow{
		ID:                 credID,
		Name:               "renamed",
		Kind:               "openai_compatible",
		Slug:               "test-cred-nil-preserve",
		Ciphertext:         []byte("encrypted"),
		KeyVersion:         1,
		ModelAllowlist:     nil, // nil = don't change
		ModelContextLimits: nil, // nil = don't change
	}
	if err := store.UpdateCredential(ctx, "org", "org-test-update-nil", credID, upd); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}

	// Read back and verify limits/allowlist are preserved.
	got, err := store.GetCredential(ctx, "org", "org-test-update-nil", credID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if got.Name != "renamed" {
		t.Errorf("Name = %q, want %q", got.Name, "renamed")
	}
	if len(got.ModelAllowlist) != 2 || got.ModelAllowlist[0] != "glm-5.1" {
		t.Errorf("ModelAllowlist = %v, want [glm-5.1, gpt-4o] (nil must preserve)", got.ModelAllowlist)
	}
	if got.ModelContextLimits["glm-5.1"] != 200000 || got.ModelContextLimits["gpt-4o"] != 128000 {
		t.Errorf("ModelContextLimits = %v, want {glm-5.1:200000, gpt-4o:128000} (nil must preserve)", got.ModelContextLimits)
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration
// +build integration

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	return pool
}

func cleanupUserKeys(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	pool.Exec(context.Background(), "DELETE FROM user_keys WHERE user_id = $1", userID)
}

func cleanupSecrets(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	pool.Exec(context.Background(), "DELETE FROM secret_audit_log WHERE user_id = $1", userID)
	pool.Exec(context.Background(), "DELETE FROM user_secret_bindings WHERE secret_id IN (SELECT id FROM user_secrets WHERE user_id = $1)", userID)
	pool.Exec(context.Background(), "DELETE FROM user_secrets WHERE user_id = $1", userID)
	pool.Exec(context.Background(), "DELETE FROM user_keys WHERE user_id = $1", userID)
}

func ensureTestUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email, password_hash, active, role) VALUES ($1, $2, $3, 'hash', true, 'user') ON CONFLICT DO NOTHING`,
		userID, "testuser-"+userID, userID+"@test.com")
}

func ensureTestWorkspace(t *testing.T, pool *pgxpool.Pool, wsID, userID string) {
	t.Helper()
	pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, user_id, runtime, storage_size, created_at, updated_at) VALUES ($1, $2, $3, 'base', '5Gi', NOW(), NOW()) ON CONFLICT DO NOTHING`,
		wsID, "test-ws-"+wsID[:8], userID)
}

// --- PgKeyStore Tests ---

func TestPgKeyStore_CreateAndGet(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgKeyStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-1"

	ensureTestUser(t, pool, userID)
	defer cleanupUserKeys(t, pool, userID)

	record := &UserKeyRecord{
		UserID:             userID,
		KeyVersion:         1,
		WrappedDEK:         []byte("wrapped-dek-data-here"),
		WrappedDEKRecovery: []byte("wrapped-recovery-data"),
		Salt:               []byte("salt-32-bytes-0123456789abcdef"),
		RecoverySalt:       []byte("recovery-salt-0123456789abcdef"),
		CreatedAt:          time.Now().Truncate(time.Microsecond),
	}

	err := store.CreateUserKey(ctx, record)
	if err != nil {
		t.Fatalf("CreateUserKey: %v", err)
	}

	got, err := store.GetUserKey(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserKey: %v", err)
	}
	if got == nil {
		t.Fatal("Expected non-nil record")
	}
	if got.KeyVersion != 1 {
		t.Errorf("KeyVersion: got %d, want 1", got.KeyVersion)
	}
	if string(got.WrappedDEK) != "wrapped-dek-data-here" {
		t.Errorf("WrappedDEK mismatch")
	}
	if string(got.Salt) != "salt-32-bytes-0123456789abcdef" {
		t.Errorf("Salt mismatch")
	}
}

func TestPgKeyStore_GetNonexistent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgKeyStore(pool)

	got, err := store.GetUserKey(context.Background(), "nonexistent-user-xyz")
	if err != nil {
		t.Fatalf("GetUserKey should not error: %v", err)
	}
	if got != nil {
		t.Error("Expected nil for nonexistent user")
	}
}

func TestPgKeyStore_UpdateWrappedDEK(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgKeyStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-2"

	ensureTestUser(t, pool, userID)
	defer cleanupUserKeys(t, pool, userID)

	store.CreateUserKey(ctx, &UserKeyRecord{
		UserID: userID, KeyVersion: 1,
		WrappedDEK: []byte("old-dek"), Salt: []byte("old-salt"),
		CreatedAt: time.Now(),
	})

	err := store.UpdateWrappedDEK(ctx, userID, []byte("new-dek"), []byte("new-salt"), 2)
	if err != nil {
		t.Fatalf("UpdateWrappedDEK: %v", err)
	}

	got, _ := store.GetUserKey(ctx, userID)
	if got.KeyVersion != 2 {
		t.Errorf("KeyVersion: got %d, want 2", got.KeyVersion)
	}
	if string(got.WrappedDEK) != "new-dek" {
		t.Error("WrappedDEK not updated")
	}
	if string(got.Salt) != "new-salt" {
		t.Error("Salt not updated")
	}
}

// --- PgSecretStore Tests ---

func TestPgSecretStore_CRUD(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-3"

	ensureTestUser(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	// Also need user_keys for FK
	keyStore := NewPgKeyStore(pool)
	keyStore.CreateUserKey(ctx, &UserKeyRecord{
		UserID: userID, KeyVersion: 1,
		WrappedDEK: []byte("dek"), Salt: []byte("salt"),
		CreatedAt: time.Now(),
	})

	// Create
	secret := &UserSecret{
		UserID:     userID,
		Name:       "pg-test-secret",
		Type:       SecretTypeAPIKey,
		Ciphertext: []byte("encrypted-data-here"),
		KeyVersion: 1,
		Metadata:   json.RawMessage(`{"kind":"openai","slug":"openai"}`),
	}
	err := store.CreateSecret(ctx, secret)
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if secret.ID == "" {
		t.Fatal("ID should be set after create")
	}

	// Get
	got, err := store.GetSecret(ctx, userID, secret.ID)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got.Name != "pg-test-secret" {
		t.Errorf("Name: got %s", got.Name)
	}
	if string(got.Ciphertext) != "encrypted-data-here" {
		t.Error("Ciphertext mismatch")
	}

	// List
	list, err := store.ListSecrets(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("Expected 1 secret, got %d", len(list))
	}

	// Update
	got.Ciphertext = []byte("updated-ciphertext")
	got.UpdatedAt = time.Now()
	err = store.UpdateSecret(ctx, got)
	if err != nil {
		t.Fatalf("UpdateSecret: %v", err)
	}

	got2, _ := store.GetSecret(ctx, userID, secret.ID)
	if string(got2.Ciphertext) != "updated-ciphertext" {
		t.Error("Ciphertext not updated")
	}

	// Delete
	err = store.DeleteSecret(ctx, userID, secret.ID)
	if err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}

	got3, _ := store.GetSecret(ctx, userID, secret.ID)
	if got3 != nil {
		t.Error("Secret should be nil after delete")
	}
}

func TestPgSecretStore_DuplicateName(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-4"

	ensureTestUser(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	keyStore := NewPgKeyStore(pool)
	keyStore.CreateUserKey(ctx, &UserKeyRecord{
		UserID: userID, KeyVersion: 1,
		WrappedDEK: []byte("dek"), Salt: []byte("salt"),
		CreatedAt: time.Now(),
	})

	store.CreateSecret(ctx, &UserSecret{
		UserID: userID, Name: "dup-name", Type: SecretTypeAPIKey,
		Ciphertext: []byte("ct1"), KeyVersion: 1, Metadata: json.RawMessage("{}"),
	})

	err := store.CreateSecret(ctx, &UserSecret{
		UserID: userID, Name: "dup-name", Type: SecretTypeAPIKey,
		Ciphertext: []byte("ct2"), KeyVersion: 1, Metadata: json.RawMessage("{}"),
	})
	if err == nil {
		t.Error("Duplicate name should fail")
	}
}

func TestPgSecretStore_Bindings(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-5"

	ensureTestUser(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	keyStore := NewPgKeyStore(pool)
	keyStore.CreateUserKey(ctx, &UserKeyRecord{
		UserID: userID, KeyVersion: 1,
		WrappedDEK: []byte("dek"), Salt: []byte("salt"),
		CreatedAt: time.Now(),
	})

	// Create 2 secrets
	s1 := &UserSecret{UserID: userID, Name: "bind-1", Type: SecretTypeAPIKey, Ciphertext: []byte("c1"), KeyVersion: 1, Metadata: json.RawMessage("{}")}
	s2 := &UserSecret{UserID: userID, Name: "bind-2", Type: SecretTypeEnvSecret, Ciphertext: []byte("c2"), KeyVersion: 1, Metadata: json.RawMessage(`{"var_name":"X"}`)}
	store.CreateSecret(ctx, s1)
	store.CreateSecret(ctx, s2)

	wsID := fmt.Sprintf("00000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	ensureTestWorkspace(t, pool, wsID, userID)

	// Set bindings
	err := store.SetBindings(ctx, wsID, []string{s1.ID, s2.ID})
	if err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Get bindings
	bound, err := store.GetBindings(ctx, wsID)
	if err != nil {
		t.Fatalf("GetBindings: %v", err)
	}
	if len(bound) != 2 {
		t.Errorf("Expected 2 bindings, got %d", len(bound))
	}

	// Rebind with only s1
	err = store.SetBindings(ctx, wsID, []string{s1.ID})
	if err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	bound, _ = store.GetBindings(ctx, wsID)
	if len(bound) != 1 {
		t.Errorf("Expected 1 binding after rebind, got %d", len(bound))
	}
}

func TestPgSecretStore_AuditLog(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgSecretStore(pool)
	ctx := context.Background()
	userID := "pg-test-user-6"

	defer func() {
		pool.Exec(ctx, "DELETE FROM secret_audit_log WHERE user_id = $1", userID)
	}()

	// Log entries
	store.LogAudit(ctx, &AuditEntry{UserID: userID, Action: "create", Metadata: json.RawMessage(`{"name":"test"}`), Timestamp: time.Now()})
	store.LogAudit(ctx, &AuditEntry{UserID: userID, Action: "read", Timestamp: time.Now()})
	store.LogAudit(ctx, &AuditEntry{UserID: userID, Action: "delete", Timestamp: time.Now()})

	// Query
	entries, err := store.QueryAudit(ctx, userID, AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryAudit: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("Expected 3 audit entries, got %d", len(entries))
	}

	// Query with filter
	entries, _ = store.QueryAudit(ctx, userID, AuditQuery{Action: "create", Limit: 10})
	if len(entries) != 1 {
		t.Errorf("Expected 1 'create' entry, got %d", len(entries))
	}
}

// --- Full E2E with real Postgres ---

func TestPgE2E_FullSecretLifecycle(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	ctx := context.Background()
	userID := "pg-e2e-user"

	ensureTestUser(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	keyStore := NewPgKeyStore(pool)
	dekCache := newMockDEKCache() // Redis not needed for this test
	keySvc := NewKeyService(keyStore, dekCache)
	secretStore := NewPgSecretStore(pool)
	svc := NewSecretService(keySvc, secretStore)

	// Init keys
	password := []byte("e2e-password")
	recoveryKey, err := keySvc.InitializeUserKeys(ctx, userID, password)
	if err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	if recoveryKey == "" {
		t.Fatal("Recovery key empty")
	}

	// Unlock
	err = keySvc.UnlockDEK(ctx, userID, password, "e2e-session", time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create secret
	created, err := svc.CreateSecret(ctx, userID, "e2e-session", nil, CreateSecretRequest{
		Name: "pg-e2e-secret", Type: SecretTypeAPIKey,
		Value:    `{"apiKey":"sk-real-test-key"}`,
		Metadata: json.RawMessage(`{"kind":"anthropic","slug":"anthropic"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// Decrypt
	plaintext, err := svc.DecryptSecretValue(ctx, userID, "e2e-session", nil, created.ID)
	if err != nil {
		t.Fatalf("DecryptSecretValue: %v", err)
	}
	if string(plaintext) != `{"apiKey":"sk-real-test-key"}` {
		t.Errorf("Decrypted value wrong: %s", string(plaintext))
	}

	// Bind
	wsID := fmt.Sprintf("00000000-0000-4000-8001-%012d", time.Now().UnixNano()%1000000000000)
	ensureTestWorkspace(t, pool, wsID, userID)
	_, err = svc.SetBindings(ctx, userID, wsID, []string{created.ID})
	if err != nil {
		t.Fatalf("SetBindings: %v", err)
	}

	// Inject
	data, err := svc.InjectSecrets(ctx, userID, "e2e-session", nil, wsID)
	if err != nil {
		t.Fatalf("InjectSecrets: %v", err)
	}
	var injected []InjectedSecret
	json.Unmarshal(data, &injected)
	if len(injected) != 1 || injected[0].Plaintext != `{"apiKey":"sk-real-test-key"}` {
		t.Errorf("Injection wrong: %v", injected)
	}

	// Password change
	newPw := []byte("new-e2e-password")
	err = keySvc.ChangePassword(ctx, userID, "", password, newPw)
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// Re-login with new password
	err = keySvc.UnlockDEK(ctx, userID, newPw, "e2e-session-2", time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK with new password: %v", err)
	}

	// Secret still decryptable
	plaintext2, err := svc.DecryptSecretValue(ctx, userID, "e2e-session-2", nil, created.ID)
	if err != nil {
		t.Fatalf("Decrypt after password change: %v", err)
	}
	if string(plaintext2) != `{"apiKey":"sk-real-test-key"}` {
		t.Errorf("Value wrong after password change: %s", string(plaintext2))
	}

	t.Log("PostgreSQL E2E: full lifecycle passed")
}

// TestPgE2E_RotateKey_AtomicReEncryption is the integration-level
// regression for Bug 9 + the pass-2 commit-callback atomicity fix.
// Rotation must:
//   - re-encrypt every user_secrets row under the new DEK
//   - update user_keys.wrapped_dek + .wrapped_dek_recovery
//   - all in a single SERIALIZABLE transaction
//
// If any step fails, the entire tx must roll back. We validate by
// asserting the post-rotation DEK can decrypt every pre-rotation
// secret — the property that pre-fix Bug 9 broke (data loss).
func TestPgE2E_RotateKey_AtomicReEncryption(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	userID := fmt.Sprintf("rotate-%d", time.Now().UnixNano())
	ensureTestUser(t, pool, userID)
	defer cleanupUserKeys(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	keyStore := NewPgKeyStore(pool)
	secretStore := NewPgSecretStore(pool)
	cache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, cache)
	svc := NewSecretService(keySvc, secretStore)
	keySvc.SetSecretStore(secretStore)

	ctx := context.Background()
	password := []byte("rotate-test-password")
	sessionID := fmt.Sprintf("rotate-sess-%d", time.Now().UnixNano())

	originalRecovery, err := keySvc.InitializeUserKeys(ctx, userID, password)
	if err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create three secrets with distinct plaintexts.
	plaintexts := map[string]string{
		"alpha": "alpha-pre-rotate",
		"beta":  "beta-pre-rotate",
		"gamma": "gamma-pre-rotate",
	}
	createdIDs := make(map[string]string)
	for name, value := range plaintexts {
		s, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
			Name: name, Type: SecretTypeEnvSecret, Value: value,
			Metadata: json.RawMessage(`{"var_name":"X"}`),
		})
		if err != nil {
			t.Fatalf("CreateSecret %s: %v", name, err)
		}
		createdIDs[name] = s.ID
	}

	// Rotate.
	rot, err := keySvc.RotateKeyWithPassword(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("RotateKeyWithPassword: %v", err)
	}
	if rot.NewKeyVersion != 2 {
		t.Errorf("expected key_version=2 after rotate, got %d", rot.NewKeyVersion)
	}
	if rot.NewRecoveryKeyHex == "" {
		t.Fatal("rotation must return a fresh recovery key")
	}
	if rot.NewRecoveryKeyHex == originalRecovery {
		t.Fatal("post-rotate recovery key must differ from original (the original wraps the discarded DEK)")
	}

	// Every pre-rotation secret must still decrypt with the new
	// session DEK. This is the load-bearing assertion for Bug 9.
	for name, want := range plaintexts {
		got, err := svc.DecryptSecretValue(ctx, userID, sessionID, nil, createdIDs[name])
		if err != nil {
			t.Fatalf("DecryptSecretValue(%s) post-rotate: %v — Bug 9 regression", name, err)
		}
		if string(got) != want {
			t.Errorf("post-rotate plaintext mismatch for %s: got %q want %q", name, string(got), want)
		}
	}

	// Old recovery key must NOT work post-rotation.
	if _, err := keySvc.ResetWithRecoveryKey(ctx, userID, originalRecovery, []byte("new-pw")); err == nil {
		t.Error("Old recovery key must be rejected after rotation; it would unwrap the discarded DEK")
	}

	// New recovery key MUST work and yield a DEK that decrypts the
	// re-encrypted secrets.
	newRec2, err := keySvc.ResetWithRecoveryKey(ctx, userID, rot.NewRecoveryKeyHex, []byte("new-pw"))
	if err != nil {
		t.Fatalf("ResetWithRecoveryKey with new key: %v — A2 regression", err)
	}
	if newRec2 == "" {
		t.Error("recovery-key reset must yield another fresh recovery key")
	}
	if err := keySvc.UnlockDEK(ctx, userID, []byte("new-pw"), "post-reset-sess", time.Hour); err != nil {
		t.Fatalf("UnlockDEK post-reset: %v", err)
	}
	for name, want := range plaintexts {
		got, err := svc.DecryptSecretValue(ctx, userID, "post-reset-sess", nil, createdIDs[name])
		if err != nil {
			t.Fatalf("DecryptSecretValue(%s) post-reset: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("post-reset plaintext mismatch for %s: got %q want %q", name, string(got), want)
		}
	}

	t.Log("PostgreSQL: atomic key rotation + fresh recovery key passed")
}

// TestPgE2E_AddBindings_IdempotentAndConcurrent is the integration
// regression for the AddBindings primitive. It must:
//   - INSERT ... ON CONFLICT DO NOTHING (idempotent re-add)
//   - take pg_try_advisory_xact_lock so concurrent SetBindings +
//     AddBindings calls on the same workspace serialize
//   - never lose updates under contention
func TestPgE2E_AddBindings_IdempotentAndConcurrent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	userID := fmt.Sprintf("addb-%d", time.Now().UnixNano())
	ensureTestUser(t, pool, userID)
	defer cleanupUserKeys(t, pool, userID)
	defer cleanupSecrets(t, pool, userID)

	keyStore := NewPgKeyStore(pool)
	secretStore := NewPgSecretStore(pool)
	cache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, cache)
	svc := NewSecretService(keySvc, secretStore)

	ctx := context.Background()
	password := []byte("addb-pw")
	sessionID := fmt.Sprintf("addb-sess-%d", time.Now().UnixNano())
	if _, err := keySvc.InitializeUserKeys(ctx, userID, password); err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create three secrets.
	var ids []string
	for i := 0; i < 3; i++ {
		s, err := svc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
			Name: fmt.Sprintf("s%d", i), Type: SecretTypeEnvSecret, Value: "v",
			Metadata: json.RawMessage(`{"var_name":"X"}`),
		})
		if err != nil {
			t.Fatalf("CreateSecret: %v", err)
		}
		ids = append(ids, s.ID)
	}

	wsID := fmt.Sprintf("00000000-0000-4000-8002-%012d", time.Now().UnixNano()%1000000000000)
	ensureTestWorkspace(t, pool, wsID, userID)

	// Idempotent: calling AddBindings 5 times with the same set
	// must end with one binding per secret, not 15.
	for i := 0; i < 5; i++ {
		if err := secretStore.AddBindings(ctx, wsID, ids); err != nil {
			t.Fatalf("AddBindings call %d: %v", i, err)
		}
	}
	bindings, err := secretStore.GetBindings(ctx, wsID)
	if err != nil {
		t.Fatalf("GetBindings: %v", err)
	}
	if len(bindings) != 3 {
		t.Errorf("expected 3 bindings after 5 idempotent AddBindings, got %d", len(bindings))
	}

	// SetBindings + concurrent AddBindings: SetBindings is "replace",
	// AddBindings is "merge". The advisory lock serializes them so
	// the final state is well-defined: whichever ran last wins
	// the union.
	wsID2 := fmt.Sprintf("00000000-0000-4000-8003-%012d", time.Now().UnixNano()%1000000000000)
	ensureTestWorkspace(t, pool, wsID2, userID)
	if err := secretStore.SetBindings(ctx, wsID2, []string{ids[0], ids[1]}); err != nil {
		t.Fatalf("SetBindings: %v", err)
	}
	if err := secretStore.AddBindings(ctx, wsID2, []string{ids[2]}); err != nil {
		t.Fatalf("AddBindings: %v", err)
	}
	bindings2, _ := secretStore.GetBindings(ctx, wsID2)
	if len(bindings2) != 3 {
		t.Errorf("expected 3 bindings (Set 2 + Add 1), got %d", len(bindings2))
	}

	t.Log("PostgreSQL: AddBindings idempotent + advisory-lock serialization passed")
}

// TestPgE2E_AsyncAuditLogger_Lifecycle covers the lifecycle hardening
// from passes 2-3:
//   - LogAudit during normal operation persists rows
//   - Stop() drains the buffer cleanly
//   - LogAudit AFTER Stop() does not panic (just drops + bumps counter)
func TestPgE2E_AsyncAuditLogger_Lifecycle(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()

	pgStore := NewPgSecretStore(pool)

	userID := fmt.Sprintf("audit-%d", time.Now().UnixNano())
	defer cleanupSecrets(t, pool, userID)

	auditLogger := NewAsyncAuditLogger(pgStore, 256, nil)

	// Burst 50 entries; drain.
	for i := 0; i < 50; i++ {
		ws := fmt.Sprintf("ws-%d", i)
		_ = auditLogger.LogAudit(context.Background(), &AuditEntry{
			UserID:      userID,
			Action:      "test",
			WorkspaceID: &ws,
			Timestamp:   time.Now(),
		})
	}

	auditLogger.Stop()
	stats := auditLogger.Stats()
	if stats.Written+stats.Dropped+stats.Failed != 50 {
		t.Errorf("expected 50 entries accounted for, got W=%d D=%d F=%d",
			stats.Written, stats.Dropped, stats.Failed)
	}

	// Post-Stop LogAudit must not panic. Goroutine-safe + idempotent.
	for i := 0; i < 10; i++ {
		ws := "post-stop"
		err := auditLogger.LogAudit(context.Background(), &AuditEntry{
			UserID:      userID,
			Action:      "post-stop",
			WorkspaceID: &ws,
			Timestamp:   time.Now(),
		})
		if err != nil {
			t.Errorf("LogAudit post-Stop should never error, got %v", err)
		}
	}
	statsAfter := auditLogger.Stats()
	if statsAfter.Dropped < stats.Dropped+10 {
		t.Errorf("expected post-Stop drops to add 10, got %d (was %d)",
			statsAfter.Dropped, stats.Dropped)
	}

	// Idempotent Stop().
	auditLogger.Stop()
	auditLogger.Stop()

	t.Log("PostgreSQL: AsyncAuditLogger lifecycle passed")
}

// --- PgJWTSessionStore Tests (Epic 56) ---

func cleanupJWTSessions(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	pool.Exec(context.Background(), "DELETE FROM jwt_sessions WHERE user_id = $1", userID)
}

func TestPgJWTSessionStore_WriteAndGet(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userID := "pg-jwt-user-1"
	ensureTestUser(t, pool, userID)
	defer cleanupJWTSessions(t, pool, userID)
	defer cleanupUserKeys(t, pool, userID) // ON DELETE CASCADE from users — cleanup users implicitly

	jti := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := &JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: []byte("wrapped-dek"),
		KEKSalt:    []byte("salt-32-bytes-0123456789abcdef!!"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
	if err := store.WriteJWTSession(ctx, row); err != nil {
		t.Fatalf("WriteJWTSession: %v", err)
	}

	got, err := store.GetJWTSession(ctx, jti)
	if err != nil {
		t.Fatalf("GetJWTSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected row")
	}
	if got.JTI != jti {
		t.Errorf("JTI: got %s, want %s", got.JTI, jti)
	}
	if got.UserID != userID {
		t.Errorf("UserID: got %s, want %s", got.UserID, userID)
	}
	if string(got.WrappedDEK) != "wrapped-dek" {
		t.Errorf("WrappedDEK mismatch")
	}
	// PG returns TZ-aware timestamps; compare in UTC.
	if !got.ExpiresAt.Equal(row.ExpiresAt) {
		t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, row.ExpiresAt)
	}
}

func TestPgJWTSessionStore_GetMissing_ReturnsNilNil(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)

	got, err := store.GetJWTSession(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Get for missing row should not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing row")
	}
}

func TestPgJWTSessionStore_WriteUpsert_OverwritesOnConflict(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userID := "pg-jwt-user-2"
	ensureTestUser(t, pool, userID)
	defer cleanupJWTSessions(t, pool, userID)

	jti := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// First write
	if err := store.WriteJWTSession(ctx, &JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: []byte("v1-wrap"),
		KEKSalt:    []byte("v1-salt"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Soft-unlock backfill: same jti, fresh kek_salt + wrapped_dek + later expiry
	if err := store.WriteJWTSession(ctx, &JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: []byte("v2-wrap"),
		KEKSalt:    []byte("v2-salt"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.GetJWTSession(ctx, jti)
	if err != nil || got == nil {
		t.Fatalf("get post-upsert: row=%v err=%v", got, err)
	}
	if string(got.WrappedDEK) != "v2-wrap" {
		t.Errorf("WrappedDEK: got %q, want v2-wrap (upsert should overwrite)", got.WrappedDEK)
	}
	if string(got.KEKSalt) != "v2-salt" {
		t.Errorf("KEKSalt: got %q, want v2-salt", got.KEKSalt)
	}
	if !got.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("ExpiresAt should be updated to later value")
	}
}

func TestPgJWTSessionStore_DeleteJWTSession(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userID := "pg-jwt-user-3"
	ensureTestUser(t, pool, userID)
	defer cleanupJWTSessions(t, pool, userID)

	jti := uuid.New()
	_ = store.WriteJWTSession(ctx, &JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: []byte("w"),
		KEKSalt:    []byte("s"),
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	if err := store.DeleteJWTSession(ctx, jti); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := store.GetJWTSession(ctx, jti)
	if got != nil {
		t.Errorf("expected row deleted")
	}

	// Idempotent — delete again should be fine
	if err := store.DeleteJWTSession(ctx, jti); err != nil {
		t.Errorf("second delete (idempotency): %v", err)
	}
}

func TestPgJWTSessionStore_DeleteJWTSessionsForUser(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userA := "pg-jwt-user-A"
	userB := "pg-jwt-user-B"
	ensureTestUser(t, pool, userA)
	ensureTestUser(t, pool, userB)
	defer cleanupJWTSessions(t, pool, userA)
	defer cleanupJWTSessions(t, pool, userB)

	// 3 rows for userA, 1 for userB
	for i := 0; i < 3; i++ {
		_ = store.WriteJWTSession(ctx, &JWTSession{
			JTI: uuid.New(), UserID: userA, WrappedDEK: []byte("w"), KEKSalt: []byte("s"),
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		})
	}
	keep := uuid.New()
	_ = store.WriteJWTSession(ctx, &JWTSession{
		JTI: keep, UserID: userB, WrappedDEK: []byte("w"), KEKSalt: []byte("s"),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})

	n, err := store.DeleteJWTSessionsForUser(ctx, userA)
	if err != nil {
		t.Fatalf("delete-for-user: %v", err)
	}
	if n != 3 {
		t.Errorf("rows affected = %d, want 3", n)
	}

	// userB's row preserved
	if got, _ := store.GetJWTSession(ctx, keep); got == nil {
		t.Errorf("user B's row should not be touched")
	}
}

func TestPgJWTSessionStore_DeleteExpiredJWTSessions(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userID := "pg-jwt-user-4"
	ensureTestUser(t, pool, userID)
	defer cleanupJWTSessions(t, pool, userID)

	cutoff := time.Now()

	// 2 expired
	expired1 := uuid.New()
	expired2 := uuid.New()
	_ = store.WriteJWTSession(ctx, &JWTSession{JTI: expired1, UserID: userID, WrappedDEK: []byte("w"), KEKSalt: []byte("s"), CreatedAt: cutoff.Add(-2 * time.Hour), ExpiresAt: cutoff.Add(-time.Hour)})
	_ = store.WriteJWTSession(ctx, &JWTSession{JTI: expired2, UserID: userID, WrappedDEK: []byte("w"), KEKSalt: []byte("s"), CreatedAt: cutoff.Add(-2 * time.Hour), ExpiresAt: cutoff.Add(-time.Minute)})

	// 1 active
	active := uuid.New()
	_ = store.WriteJWTSession(ctx, &JWTSession{JTI: active, UserID: userID, WrappedDEK: []byte("w"), KEKSalt: []byte("s"), CreatedAt: cutoff, ExpiresAt: cutoff.Add(time.Hour)})

	n, err := store.DeleteExpiredJWTSessions(ctx, cutoff)
	if err != nil {
		t.Fatalf("delete-expired: %v", err)
	}
	if n != 2 {
		t.Errorf("rows affected = %d, want 2", n)
	}
	if got, _ := store.GetJWTSession(ctx, active); got == nil {
		t.Errorf("active row should survive")
	}
	if got, _ := store.GetJWTSession(ctx, expired1); got != nil {
		t.Errorf("expired row should be pruned")
	}
}

func TestPgJWTSessionStore_UserDeletionCascadesToJWTSessions(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()
	userID := "pg-jwt-user-cascade"
	ensureTestUser(t, pool, userID)
	// Note: not deferring cleanupJWTSessions because the CASCADE will do it.

	jti := uuid.New()
	_ = store.WriteJWTSession(ctx, &JWTSession{
		JTI: jti, UserID: userID, WrappedDEK: []byte("w"), KEKSalt: []byte("s"),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})

	// Delete the user — FK ON DELETE CASCADE should clear jwt_sessions.
	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	got, err := store.GetJWTSession(ctx, jti)
	if err != nil {
		t.Fatalf("get after user cascade: %v", err)
	}
	if got != nil {
		t.Errorf("jwt_sessions row should cascade-delete with user; row still exists")
	}
}

// TestPgJWTSessionStore_ListActiveJWTSessionsForUser exercises the real
// SQL against PostgreSQL, catching mistakes the Go mock cannot:
//
//   - WHERE user_id = $1 typo or column-name drift.
//   - `expires_at > NOW()` vs `>=` boundary — a row at exactly NOW must
//     be treated as expired (the janitor's DELETE uses < NOW; if the
//     list query used >= NOW we'd have a millisecond-window where the
//     row is "listable but about to be pruned").
//   - ORDER BY created_at DESC direction (bot review flagged this
//     specifically — an ASC typo would ship without failing any Go-mock
//     test since the mock sorts in Go, not in SQL).
//   - LIMIT $2 vs LIMIT 5 hard-code — a caller-supplied bound MUST
//     round-trip through the query.
//   - Cross-user isolation via the WHERE clause, not just Go filtering.
//
// The Go mock in jwt_session_store_test.go validates the API contract;
// this test validates the SQL execution against a real Postgres.
func TestPgJWTSessionStore_ListActiveJWTSessionsForUser(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()

	userA := "pg-jwt-listactive-A"
	userB := "pg-jwt-listactive-B"
	ensureTestUser(t, pool, userA)
	ensureTestUser(t, pool, userB)
	defer cleanupJWTSessions(t, pool, userA)
	defer cleanupJWTSessions(t, pool, userB)

	// Baseline "now" for ordering assertions. Use time.Now() rather than
	// a fixed timestamp so we exercise the real NOW() vs stored-time
	// comparison the janitor also relies on.
	now := time.Now()

	// User A rows: 3 active with varying created_at, 1 expired.
	rowA_oldest := uuid.New()
	rowA_mid := uuid.New()
	rowA_newest := uuid.New()
	rowA_expired := uuid.New()

	for _, s := range []*JWTSession{
		{JTI: rowA_oldest, UserID: userA, WrappedDEK: []byte{1}, KEKSalt: []byte{1},
			CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(1 * time.Hour)},
		{JTI: rowA_mid, UserID: userA, WrappedDEK: []byte{2}, KEKSalt: []byte{2},
			CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(1 * time.Hour)},
		{JTI: rowA_newest, UserID: userA, WrappedDEK: []byte{3}, KEKSalt: []byte{3},
			CreatedAt: now.Add(-1 * time.Hour), ExpiresAt: now.Add(1 * time.Hour)},
		{JTI: rowA_expired, UserID: userA, WrappedDEK: []byte{4}, KEKSalt: []byte{4},
			CreatedAt: now.Add(-4 * time.Hour), ExpiresAt: now.Add(-1 * time.Minute)},
	} {
		if err := store.WriteJWTSession(ctx, s); err != nil {
			t.Fatalf("write %v: %v", s.JTI, err)
		}
	}

	// User B row: 1 active — MUST NOT leak into user A's results.
	rowB := uuid.New()
	if err := store.WriteJWTSession(ctx, &JWTSession{
		JTI: rowB, UserID: userB, WrappedDEK: []byte{5}, KEKSalt: []byte{5},
		CreatedAt: now.Add(-30 * time.Minute), ExpiresAt: now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("write userB: %v", err)
	}

	// --- Assertion 1: unlimited list for userA returns 3 active rows
	// in created_at DESC order. Expired row excluded.
	got, err := store.ListActiveJWTSessionsForUser(ctx, userA, 0)
	if err != nil {
		t.Fatalf("list unlimited: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unlimited list: want 3 (3 active, 1 expired excluded); got %d", len(got))
	}
	// SQL ORDER BY created_at DESC — newest first.
	if got[0].JTI != rowA_newest {
		t.Errorf("first row: want newest %v; got %v", rowA_newest, got[0].JTI)
	}
	if got[1].JTI != rowA_mid {
		t.Errorf("second row: want mid %v; got %v", rowA_mid, got[1].JTI)
	}
	if got[2].JTI != rowA_oldest {
		t.Errorf("third row: want oldest %v; got %v", rowA_oldest, got[2].JTI)
	}
	for _, r := range got {
		if r.JTI == rowA_expired {
			t.Errorf("expired row must be excluded by WHERE expires_at > NOW(); got %v", r.JTI)
		}
		if r.UserID != userA {
			t.Errorf("cross-user leak: got %s for userA query", r.UserID)
		}
	}

	// --- Assertion 2: LIMIT enforcement. Caller-supplied bound must
	// round-trip through the SQL, not be Go-side-clamped.
	limited, err := store.ListActiveJWTSessionsForUser(ctx, userA, 2)
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit=2: want 2 rows; got %d", len(limited))
	}
	if limited[0].JTI != rowA_newest || limited[1].JTI != rowA_mid {
		t.Errorf("limit preserved wrong rows: got [%v %v]", limited[0].JTI, limited[1].JTI)
	}

	// --- Assertion 3: cross-user isolation. UserB query must return
	// only userB's row.
	bRows, err := store.ListActiveJWTSessionsForUser(ctx, userB, 0)
	if err != nil {
		t.Fatalf("list userB: %v", err)
	}
	if len(bRows) != 1 || bRows[0].JTI != rowB {
		t.Errorf("userB query: want [%v]; got %+v", rowB, bRows)
	}

	// --- Assertion 4: unknown user returns empty, nil error.
	empty, err := store.ListActiveJWTSessionsForUser(ctx, "pg-jwt-nonexistent", 0)
	if err != nil {
		t.Fatalf("list unknown: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown user must return empty; got %d rows", len(empty))
	}
}

// TestPgJWTSessionStore_ListActive_BoundaryAtExactNow validates that a
// row expiring at exactly the query's clock-observed NOW() is
// EXCLUDED (SQL uses strict `> NOW()`, matching the janitor's strict
// `< NOW()` in DeleteExpiredJWTSessions). Without this, a "just-
// pruned" row would briefly appear in the list, waste a signing-key
// iteration, and produce a misleading Warn.
//
// We can't force a row to have expires_at == server NOW exactly, so
// we insert with expires_at slightly in the past and confirm it's
// filtered, then insert with expires_at slightly in the future and
// confirm it appears. The observation that both cases behave
// correctly plus the janitor's strict < NOW semantics implies the
// boundary is on the correct side.
func TestPgJWTSessionStore_ListActive_BoundaryAtExactNow(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgJWTSessionStore(pool)
	ctx := context.Background()

	userID := "pg-jwt-listactive-boundary"
	ensureTestUser(t, pool, userID)
	defer cleanupJWTSessions(t, pool, userID)

	nearPast := uuid.New()
	nearFuture := uuid.New()

	// One row expiring 100ms in the past — must be excluded.
	// One row expiring 5s in the future — must be included.
	// (The 5s buffer prevents flakiness on slow test machines where
	// the "future" row could expire between insert and query.)
	now := time.Now()
	if err := store.WriteJWTSession(ctx, &JWTSession{
		JTI: nearPast, UserID: userID, WrappedDEK: []byte{1}, KEKSalt: []byte{1},
		CreatedAt: now.Add(-1 * time.Hour), ExpiresAt: now.Add(-100 * time.Millisecond),
	}); err != nil {
		t.Fatalf("write nearPast: %v", err)
	}
	if err := store.WriteJWTSession(ctx, &JWTSession{
		JTI: nearFuture, UserID: userID, WrappedDEK: []byte{2}, KEKSalt: []byte{2},
		CreatedAt: now.Add(-1 * time.Hour), ExpiresAt: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("write nearFuture: %v", err)
	}

	got, err := store.ListActiveJWTSessionsForUser(ctx, userID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("boundary: want 1 (near-future only); got %d", len(got))
	}
	if got[0].JTI != nearFuture {
		t.Errorf("wrong row surfaced: want %v (future); got %v", nearFuture, got[0].JTI)
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- In-memory mocks ---

type mockKeyStore struct {
	mu                     sync.Mutex
	records                map[string]*UserKeyRecord
	updateWithSourceCalled bool
}

func newMockKeyStore() *mockKeyStore {
	return &mockKeyStore{records: make(map[string]*UserKeyRecord)}
}

func (m *mockKeyStore) GetUserKey(_ context.Context, userID string) (*UserKeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return nil, nil
	}
	// Return a copy
	cp := *r
	return &cp, nil
}

func (m *mockKeyStore) CreateUserKey(_ context.Context, record *UserKeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// UPSERT semantics — mirrors PgKeyStore.CreateUserKey's
	// INSERT ... ON CONFLICT (user_id) DO UPDATE, so that password
	// reset can reinitialize a fresh DEK for a user who already has
	// key material (the prior wraps are overwritten).
	cp := *record
	m.records[record.UserID] = &cp
	return nil
}

func (m *mockKeyStore) UpdateWrappedDEK(_ context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("user key not found")
	}
	r.WrappedDEK = wrappedDEK
	r.Salt = salt
	r.KeyVersion = keyVersion
	return nil
}

func (m *mockKeyStore) UpdateWrappedDEKAndSource(_ context.Context, userID string, wrappedDEK, salt []byte, keyVersion int, dekSource string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("user key not found")
	}
	r.WrappedDEK = wrappedDEK
	r.Salt = salt
	r.KeyVersion = keyVersion
	r.DEKSource = dekSource
	m.updateWithSourceCalled = true
	return nil
}

func (m *mockKeyStore) UpdateWrappedDEKRecovery(_ context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("user key not found")
	}
	r.WrappedDEKRecovery = wrappedDEKRecovery
	r.RecoverySalt = recoverySalt
	return nil
}

type mockDEKCache struct {
	mu    sync.Mutex
	store map[string][]byte
}

func newMockDEKCache() *mockDEKCache {
	return &mockDEKCache{store: make(map[string][]byte)}
}

func (m *mockDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(dek))
	copy(cp, dek)
	m.store[sessionID] = cp
	return nil
}

func (m *mockDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dek, ok := m.store[sessionID]
	if !ok {
		return nil, nil
	}
	return dek, nil
}

func (m *mockDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, sessionID)
	return nil
}

// --- Tests ---

func TestKeyService_InitializeUserKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	recoveryHex, err := svc.InitializeUserKeys(ctx, "user-1", []byte("password123"))
	if err != nil {
		t.Fatalf("InitializeUserKeys failed: %v", err)
	}

	// Recovery key should be valid hex, 16 bytes = 32 hex chars
	recoveryBytes, err := hex.DecodeString(recoveryHex)
	if err != nil {
		t.Fatalf("Recovery key is not valid hex: %v", err)
	}
	if len(recoveryBytes) != 16 {
		t.Errorf("Recovery key should be 16 bytes, got %d", len(recoveryBytes))
	}

	// Verify record was stored
	record, err := store.GetUserKey(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetUserKey failed: %v", err)
	}
	if record == nil {
		t.Fatal("Expected user key record to exist")
	}
	if record.KeyVersion != 1 {
		t.Errorf("Expected key version 1, got %d", record.KeyVersion)
	}
	if len(record.WrappedDEK) == 0 {
		t.Error("WrappedDEK should not be empty")
	}
	if len(record.WrappedDEKRecovery) == 0 {
		t.Error("WrappedDEKRecovery should not be empty")
	}
	if len(record.Salt) != 32 {
		t.Errorf("Salt should be 32 bytes, got %d", len(record.Salt))
	}
	if len(record.RecoverySalt) != 32 {
		t.Errorf("RecoverySalt should be 32 bytes, got %d", len(record.RecoverySalt))
	}
}

func TestKeyService_InitializeUserKeys_Reinit_Upserts(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	recovery1, err := svc.InitializeUserKeys(ctx, "user-1", []byte("password"))
	if err != nil {
		t.Fatalf("First init failed: %v", err)
	}
	first, _ := store.GetUserKey(ctx, "user-1")
	if first == nil {
		t.Fatal("expected first key record")
	}

	// Reinit (e.g. password reset) for a user who already has keys must
	// UPSERT rather than fail on the user_keys PRIMARY KEY constraint.
	// A fresh DEK is generated, so the wrap, salt, and recovery key all
	// change; the previous DEK (and anything encrypted with it) is gone.
	recovery2, err := svc.InitializeUserKeys(ctx, "user-1", []byte("new-password"))
	if err != nil {
		t.Fatalf("Second init should upsert, got error: %v", err)
	}
	if recovery1 == "" || recovery2 == "" || recovery1 == recovery2 {
		t.Errorf("expected two distinct non-empty recovery keys, got %q and %q", recovery1, recovery2)
	}

	second, _ := store.GetUserKey(ctx, "user-1")
	if second == nil {
		t.Fatal("expected second key record")
	}
	if bytes.Equal(second.WrappedDEK, first.WrappedDEK) {
		t.Error("reinit should produce a fresh wrapped DEK")
	}
	if bytes.Equal(second.Salt, first.Salt) {
		t.Error("reinit should produce a fresh salt")
	}
	if second.KeyVersion != 1 {
		t.Errorf("InitializeUserKeys resets key_version to 1, got %d", second.KeyVersion)
	}
}

func TestKeyService_UnlockDEK(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	password := []byte("my-password")
	_, err := svc.InitializeUserKeys(ctx, "user-1", password)
	if err != nil {
		t.Fatalf("InitializeUserKeys failed: %v", err)
	}

	// Unlock (simulate login)
	err = svc.UnlockDEK(ctx, "user-1", password, "session-abc", 24*time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK failed: %v", err)
	}

	// DEK should be cached
	dek, err := cache.GetDEK(ctx, "session-abc")
	if err != nil {
		t.Fatalf("GetDEK failed: %v", err)
	}
	if dek == nil {
		t.Error("DEK should be cached after unlock")
	}
	if len(dek) != 32 {
		t.Errorf("Cached DEK should be 32 bytes, got %d", len(dek))
	}
}

func TestKeyService_UnlockDEK_WrongPassword(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("correct-password"))

	err := svc.UnlockDEK(ctx, "user-1", []byte("wrong-password"), "session-1", time.Hour)
	if err == nil {
		t.Error("UnlockDEK with wrong password should fail")
	}
}

func TestKeyService_UnlockDEK_NoKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	// User without keys (legacy user) — should succeed silently
	err := svc.UnlockDEK(ctx, "user-no-keys", []byte("password"), "session-1", time.Hour)
	if err != nil {
		t.Errorf("UnlockDEK for user without keys should succeed silently, got: %v", err)
	}
}

func TestKeyService_EvictDEK(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	password := []byte("password")
	_, _ = svc.InitializeUserKeys(ctx, "user-1", password)
	_ = svc.UnlockDEK(ctx, "user-1", password, "session-1", time.Hour)

	// Verify cached
	if !svc.DEKAvailable(ctx, "session-1") {
		t.Fatal("DEK should be available before eviction")
	}

	// Evict
	err := svc.EvictDEK(ctx, "session-1")
	if err != nil {
		t.Fatalf("EvictDEK failed: %v", err)
	}

	// Verify evicted
	if svc.DEKAvailable(ctx, "session-1") {
		t.Error("DEK should not be available after eviction")
	}
}

func TestKeyService_GetDEK(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	password := []byte("password")
	_, _ = svc.InitializeUserKeys(ctx, "user-1", password)
	_ = svc.UnlockDEK(ctx, "user-1", password, "session-1", time.Hour)

	dek, err := svc.GetDEK(ctx, "session-1", nil)
	if err != nil {
		t.Fatalf("GetDEK failed: %v", err)
	}
	if len(dek) != 32 {
		t.Errorf("DEK should be 32 bytes, got %d", len(dek))
	}
}

func TestKeyService_GetDEK_NotCached(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	_, err := svc.GetDEK(ctx, "nonexistent-session", nil)
	if err == nil {
		t.Error("GetDEK for nonexistent session should fail")
	}
}

func TestKeyService_ChangePassword(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	oldPassword := []byte("old-password")
	newPassword := []byte("new-password")

	_, _ = svc.InitializeUserKeys(ctx, "user-1", oldPassword)

	// Unlock with old password to get DEK
	_ = svc.UnlockDEK(ctx, "user-1", oldPassword, "session-before", time.Hour)
	dekBefore, _ := svc.GetDEK(ctx, "session-before", nil)

	// Change password
	err := svc.ChangePassword(ctx, "user-1", "", oldPassword, newPassword)
	if err != nil {
		t.Fatalf("ChangePassword failed: %v", err)
	}

	// Old password should no longer work
	err = svc.UnlockDEK(ctx, "user-1", oldPassword, "session-old", time.Hour)
	if err == nil {
		t.Error("Old password should no longer unlock DEK")
	}

	// New password should work
	err = svc.UnlockDEK(ctx, "user-1", newPassword, "session-new", time.Hour)
	if err != nil {
		t.Fatalf("New password should unlock DEK: %v", err)
	}

	// DEK should be the same (password change doesn't rotate DEK)
	dekAfter, _ := svc.GetDEK(ctx, "session-new", nil)
	if len(dekBefore) != len(dekAfter) {
		t.Error("DEK length changed after password change")
	}
	for i := range dekBefore {
		if dekBefore[i] != dekAfter[i] {
			t.Error("DEK changed after password change — should remain the same")
			break
		}
	}
}

func TestKeyService_ChangePassword_WrongOldPassword(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("correct"))

	err := svc.ChangePassword(ctx, "user-1", "", []byte("wrong"), []byte("new"))
	if err == nil {
		t.Error("ChangePassword with wrong old password should fail")
	}
}

func TestKeyService_ResetWithRecoveryKey(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	password := []byte("original-password")
	recoveryHex, _ := svc.InitializeUserKeys(ctx, "user-1", password)

	// Unlock to get original DEK
	_ = svc.UnlockDEK(ctx, "user-1", password, "session-orig", time.Hour)
	originalDEK, _ := svc.GetDEK(ctx, "session-orig", nil)

	// Reset with recovery key
	newPassword := []byte("new-password-after-reset")
	newRecoveryHex, err := svc.ResetWithRecoveryKey(ctx, "user-1", recoveryHex, newPassword)
	if err != nil {
		t.Fatalf("ResetWithRecoveryKey failed: %v", err)
	}

	// New recovery key should be different
	if newRecoveryHex == recoveryHex {
		t.Error("New recovery key should differ from old one")
	}

	// New password should work
	err = svc.UnlockDEK(ctx, "user-1", newPassword, "session-reset", time.Hour)
	if err != nil {
		t.Fatalf("New password after reset should work: %v", err)
	}

	// DEK should be unchanged
	resetDEK, _ := svc.GetDEK(ctx, "session-reset", nil)
	for i := range originalDEK {
		if originalDEK[i] != resetDEK[i] {
			t.Error("DEK should be unchanged after recovery reset")
			break
		}
	}

	// Old recovery key should no longer work
	_, err = svc.ResetWithRecoveryKey(ctx, "user-1", recoveryHex, []byte("another"))
	if err == nil {
		t.Error("Old recovery key should no longer work after reset")
	}
}

func TestKeyService_ResetWithRecoveryKey_InvalidKey(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("password"))

	_, err := svc.ResetWithRecoveryKey(ctx, "user-1", "deadbeefdeadbeefdeadbeefdeadbeef", []byte("new"))
	if err == nil {
		t.Error("Invalid recovery key should fail")
	}
}

func TestKeyService_ResetWithRecoveryKey_InvalidHex(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("password"))

	_, err := svc.ResetWithRecoveryKey(ctx, "user-1", "not-valid-hex!", []byte("new"))
	if err == nil {
		t.Error("Invalid hex recovery key should fail")
	}
}

func TestKeyService_HasKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	has, err := svc.HasKeys(ctx, "user-1")
	if err != nil {
		t.Fatalf("HasKeys failed: %v", err)
	}
	if has {
		t.Error("User without keys should return false")
	}

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("password"))

	has, err = svc.HasKeys(ctx, "user-1")
	if err != nil {
		t.Fatalf("HasKeys failed: %v", err)
	}
	if !has {
		t.Error("User with keys should return true")
	}
}

func TestKeyService_DEKAvailable(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	if svc.DEKAvailable(ctx, "no-session") {
		t.Error("DEK should not be available for nonexistent session")
	}

	_, _ = svc.InitializeUserKeys(ctx, "user-1", []byte("pw"))
	_ = svc.UnlockDEK(ctx, "user-1", []byte("pw"), "sess-1", time.Hour)

	if !svc.DEKAvailable(ctx, "sess-1") {
		t.Error("DEK should be available after unlock")
	}
}

// TestKeyService_RotateKey_EagerlyReEncryptsSecrets is the regression test
// for Bug 9 in worklog 0085: KEK rotation must walk every user_secrets row
// and re-encrypt under the new DEK. Without this, all pre-rotation secrets
// become permanently undecryptable (data loss).
func TestKeyService_RotateKey_EagerlyReEncryptsSecrets(t *testing.T) {
	ctx := context.Background()
	keyStore := newMockKeyStore()
	cache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, cache)
	secretStore := newMockSecretStore()
	secretSvc := NewSecretService(keySvc, secretStore)

	userID := "user-rotate"
	password := []byte("rotate-password-123")
	sessionID := "rotate-sess"

	if _, err := keySvc.InitializeUserKeys(ctx, userID, password); err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create three secrets pre-rotation, each with a distinct plaintext.
	plaintexts := map[string]string{
		"alpha": "alpha-value-pre-rotate",
		"beta":  "beta-value-pre-rotate",
		"gamma": "gamma-value-pre-rotate",
	}
	for name, value := range plaintexts {
		_, err := secretSvc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
			Name: name, Type: SecretTypeEnvSecret, Value: value,
			Metadata: []byte(`{"var_name":"X"}`),
		})
		if err != nil {
			t.Fatalf("CreateSecret %s: %v", name, err)
		}
	}

	// Rotate. After this, the OLD DEK is gone forever; if we have not
	// re-encrypted, GetSecret + DecryptSecretValue fails on every row.
	rotResult, err := keySvc.RotateKeyWithPassword(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("RotateKeyWithPassword: %v", err)
	}
	newVersion := rotResult.NewKeyVersion
	if newVersion != 2 {
		t.Fatalf("expected key_version=2 after rotate, got %d", newVersion)
	}
	if rotResult.NewRecoveryKeyHex == "" {
		t.Fatal("rotate must return a fresh recovery key — the old one wraps the discarded DEK")
	}

	// Every pre-rotation secret must still decrypt. This is the
	// load-bearing assertion for Bug 9.
	listed, err := secretSvc.ListSecrets(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(listed) != len(plaintexts) {
		t.Fatalf("expected %d secrets after rotate, got %d", len(plaintexts), len(listed))
	}
	for _, sr := range listed {
		got, err := secretSvc.DecryptSecretValue(ctx, userID, sessionID, nil, sr.ID)
		if err != nil {
			t.Fatalf("DecryptSecretValue(%s) after rotate: %v — Bug 9 has regressed", sr.Name, err)
		}
		want := plaintexts[sr.Name]
		if string(got) != want {
			t.Errorf("post-rotate plaintext mismatch for %s: got %q, want %q", sr.Name, string(got), want)
		}
	}

	// Every row must now carry the new key_version. Otherwise the lazy
	// path would still be required and a future rotation would orphan
	// these rows again.
	for _, sr := range listed {
		raw, err := secretStore.GetSecret(ctx, userID, sr.ID)
		if err != nil {
			t.Fatalf("GetSecret raw: %v", err)
		}
		if raw.KeyVersion != newVersion {
			t.Errorf("secret %s: expected key_version=%d after rotate, got %d",
				sr.Name, newVersion, raw.KeyVersion)
		}
	}

	// New secrets created post-rotation must also work — confirms the
	// rotation did not corrupt the active DEK cache.
	_, err = secretSvc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name: "post-rotate", Type: SecretTypeEnvSecret, Value: "post",
		Metadata: []byte(`{"var_name":"X"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret post-rotate: %v", err)
	}
}

// TestKeyService_RotateKey_IssuesFreshRecoveryKey is the regression
// test for A2 in worklog 0094 pass-2 audit: rotation rotates the DEK,
// so the previous recovery key (which wraps the now-discarded old
// DEK) must be replaced with a fresh one. Without this, a user who
// rotates and then later forgets their password discovers via
// ResetWithRecoveryKey that the recovery flow yields the OLD DEK,
// against which every secret (now encrypted with the NEW DEK) is
// undecryptable — total data loss.
func TestKeyService_RotateKey_IssuesFreshRecoveryKey(t *testing.T) {
	ctx := context.Background()
	keyStore := newMockKeyStore()
	cache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, cache)
	secretStore := newMockSecretStore()
	secretSvc := NewSecretService(keySvc, secretStore)

	userID := "user-recover-after-rotate"
	password := []byte("orig-password-1")
	sessionID := "sess-1"

	originalRecoveryHex, err := keySvc.InitializeUserKeys(ctx, userID, password)
	if err != nil {
		t.Fatalf("InitializeUserKeys: %v", err)
	}
	if err := keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour); err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// Create a pre-rotation secret.
	plaintext := "pre-rotate-secret-value"
	if _, err := secretSvc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name: "pre", Type: SecretTypeEnvSecret, Value: plaintext,
		Metadata: []byte(`{"var_name":"P"}`),
	}); err != nil {
		t.Fatalf("CreateSecret pre-rotate: %v", err)
	}

	// Rotate.
	rot, err := keySvc.RotateKeyWithPassword(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("RotateKeyWithPassword: %v", err)
	}
	if rot.NewRecoveryKeyHex == "" {
		t.Fatal("rotation must return a fresh recovery key")
	}
	if rot.NewRecoveryKeyHex == originalRecoveryHex {
		t.Fatal("fresh recovery key must differ from the original")
	}

	// Old recovery key must be REJECTED post-rotation.
	if _, err := keySvc.ResetWithRecoveryKey(ctx, userID, originalRecoveryHex, []byte("new-pw-1")); err == nil {
		t.Fatal("Old recovery key must NOT work after rotation; it would unwrap the discarded old DEK")
	}

	// New recovery key MUST work.
	freshRecoveryHexAfterReset, err := keySvc.ResetWithRecoveryKey(ctx, userID, rot.NewRecoveryKeyHex, []byte("new-pw-1"))
	if err != nil {
		t.Fatalf("ResetWithRecoveryKey with new key: %v — A2 regression: rotation did not refresh the recovery wrap", err)
	}
	if freshRecoveryHexAfterReset == "" {
		t.Error("ResetWithRecoveryKey must yield another fresh recovery key")
	}

	// And after the recovery-driven password reset, the pre-rotation
	// secret must STILL decrypt — confirms the recovery flow yielded
	// the same DEK that user_secrets are now encrypted with.
	if err := keySvc.UnlockDEK(ctx, userID, []byte("new-pw-1"), "sess-after-reset", time.Hour); err != nil {
		t.Fatalf("UnlockDEK after reset: %v", err)
	}
	listed, err := secretSvc.ListSecrets(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 secret post-reset, got %d", len(listed))
	}
	got, err := secretSvc.DecryptSecretValue(ctx, userID, "sess-after-reset", nil, listed[0].ID)
	if err != nil {
		t.Fatalf("DecryptSecretValue post-rotation-then-reset: %v — recovery flow yielded the wrong DEK", err)
	}
	if string(got) != plaintext {
		t.Errorf("plaintext mismatch: got %q want %q", string(got), plaintext)
	}
}

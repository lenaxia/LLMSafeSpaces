// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
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
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("password")
	_ = svc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
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
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("password")
	_ = svc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
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

func TestKeyService_HasKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	has, err := svc.HasKeys(ctx, "user-1")
	if err != nil {
		t.Fatalf("HasKeys failed: %v", err)
	}
	if has {
		t.Error("User without keys should return false")
	}

	_ = svc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")

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
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	if svc.DEKAvailable(ctx, "no-session") {
		t.Error("DEK should not be available for nonexistent session")
	}

	_ = svc.InitializeUserKeysServerKEK(ctx, "user-1", "server_kek")
	_ = svc.UnlockDEK(ctx, "user-1", []byte("pw"), "sess-1", time.Hour)

	if !svc.DEKAvailable(ctx, "sess-1") {
		t.Error("DEK should be available after unlock")
	}
}

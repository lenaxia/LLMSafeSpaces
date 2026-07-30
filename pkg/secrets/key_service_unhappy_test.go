// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- Failing store mocks ---

type failingKeyStore struct {
	failOn string
}

func (f *failingKeyStore) GetUserKey(ctx context.Context, userID string) (*UserKeyRecord, error) {
	if f.failOn == "get" {
		return nil, errors.New("db connection failed")
	}
	return nil, nil
}
func (f *failingKeyStore) CreateUserKey(ctx context.Context, record *UserKeyRecord) error {
	if f.failOn == "create" {
		return errors.New("db write failed")
	}
	return nil
}
func (f *failingKeyStore) UpdateWrappedDEK(ctx context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	if f.failOn == "update" {
		return errors.New("db update failed")
	}
	return nil
}
func (f *failingKeyStore) UpdateWrappedDEKAndSource(ctx context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int, dekSource string) error {
	if f.failOn == "update" {
		return errors.New("db update failed")
	}
	return nil
}
func (f *failingKeyStore) UpdateWrappedDEKRecovery(ctx context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	return nil
}

type failingDEKCache struct {
	failOn string
}

func (f *failingDEKCache) CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error {
	if f.failOn == "cache" {
		return errors.New("redis connection failed")
	}
	return nil
}
func (f *failingDEKCache) GetDEK(ctx context.Context, sessionID string) ([]byte, error) {
	if f.failOn == "get" {
		return nil, errors.New("redis read failed")
	}
	return nil, nil
}
func (f *failingDEKCache) EvictDEK(ctx context.Context, sessionID string) error {
	if f.failOn == "evict" {
		return errors.New("redis delete failed")
	}
	return nil
}

func TestKeyService_InitializeUserKeys_StoreFailure(t *testing.T) {
	store := &failingKeyStore{failOn: "create"}
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, err := svc.InitializeUserKeys(context.Background(), "user-1", []byte("pw"))
	if err == nil {
		t.Error("Should fail when store.CreateUserKey fails")
	}
}

func TestKeyService_UnlockDEK_StoreFailure(t *testing.T) {
	store := &failingKeyStore{failOn: "get"}
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	err := svc.UnlockDEK(context.Background(), "user-1", []byte("pw"), "sess", time.Hour)
	if err == nil {
		t.Error("Should fail when store.GetUserKey fails")
	}
}

func TestKeyService_UnlockDEK_CacheFailure(t *testing.T) {
	store := newMockKeyStore()
	cache := &failingDEKCache{failOn: "cache"}
	svc := NewKeyService(store, cache)
	ctx := context.Background()

	// Initialize with working store
	realCache := newMockDEKCache()
	realSvc := NewKeyService(store, realCache)
	realSvc.InitializeUserKeys(ctx, "user-1", []byte("pw"))

	// Now try to unlock with failing cache
	err := svc.UnlockDEK(ctx, "user-1", []byte("pw"), "sess", time.Hour)
	if err == nil {
		t.Error("Should fail when cache.CacheDEK fails")
	}
}

func TestKeyService_GetDEK_CacheFailure(t *testing.T) {
	store := newMockKeyStore()
	cache := &failingDEKCache{failOn: "get"}
	svc := NewKeyService(store, cache)

	_, err := svc.GetDEK(context.Background(), "sess", nil)
	if err == nil {
		t.Error("Should fail when cache.GetDEK fails")
	}
}

func TestKeyService_HasKeys_StoreFailure(t *testing.T) {
	store := &failingKeyStore{failOn: "get"}
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, err := svc.HasKeys(context.Background(), "user-1")
	if err == nil {
		t.Error("Should propagate store error")
	}
}

func TestKeyService_ChangePassword_NoKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	err := svc.ChangePassword(context.Background(), "nonexistent", "", []byte("old"), []byte("new"))
	if err == nil {
		t.Error("ChangePassword for user without keys should fail")
	}
}

func TestKeyService_ResetWithRecoveryKey_NoKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, err := svc.ResetWithRecoveryKey(context.Background(), "nonexistent", "aabbccdd", []byte("new"))
	if err == nil {
		t.Error("ResetWithRecoveryKey for user without keys should fail")
	}
}

func TestKeyService_RotateKeyWithPassword_NoKeys(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, err := svc.RotateKeyWithPassword(context.Background(), "nonexistent", []byte("pw"), "sess", time.Hour)
	if err == nil {
		t.Error("RotateKeyWithPassword for user without keys should fail")
	}
}

func TestKeyService_EvictDEK_NonexistentSession(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	// Should not error — evicting nonexistent is a no-op
	err := svc.EvictDEK(context.Background(), "nonexistent-session")
	if err != nil {
		t.Errorf("Evicting nonexistent session should not error: %v", err)
	}
}

func TestKeyService_DEKAvailable_CacheError(t *testing.T) {
	store := newMockKeyStore()
	cache := &failingDEKCache{failOn: "get"}
	svc := NewKeyService(store, cache)

	// Should return false on cache error (fail closed)
	if svc.DEKAvailable(context.Background(), "sess") {
		t.Error("DEKAvailable should return false on cache error")
	}
}

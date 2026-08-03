// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Epic 56 Step 6: UnlockDEK additionally writes a durable jwt_sessions
// row when a signing key is supplied. The KEK that wraps the durable
// DEK is derived from the active signing key + jti so the rehydrate
// path (already covered by TestKeyService_GetDEK_CacheMiss_Rehydrates*)
// can unwrap the same row using the matched signing key recovered
// from a presented JWT.

func TestKeyService_UnlockDEK_WritesDurableRow_WhenSigningKeySupplied(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	jwtStore := newMockJWTSessionStore()
	svc := NewKeyService(store, cache)
	svc.SetJWTSessionStore(jwtStore)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("correct horse battery staple")
	err := svc.InitializeUserKeysServerKEK(ctx, "u-1", "server_kek")
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	signingKey := []byte("server-jwt-signing-key-32-bytes!")
	jti := uuid.New().String()

	if err := svc.UnlockDEKWithSigningKey(ctx, "u-1", password, jti, time.Hour, signingKey); err != nil {
		t.Fatalf("UnlockDEKWithSigningKey: %v", err)
	}

	// Redis cache populated as before.
	cachedDEK, _ := cache.GetDEK(ctx, jti)
	if cachedDEK == nil {
		t.Errorf("DEK should be cached after unlock")
	}

	// Durable row written.
	jtiUUID, _ := uuid.Parse(jti)
	row, err := jwtStore.GetJWTSession(ctx, jtiUUID)
	if err != nil {
		t.Fatalf("get durable row: %v", err)
	}
	if row == nil {
		t.Fatalf("durable jwt_sessions row should exist after unlock")
	}
	if row.UserID != "u-1" {
		t.Errorf("UserID = %q, want u-1", row.UserID)
	}
	if len(row.KEKSalt) != 32 {
		t.Errorf("kek_salt should be 32 bytes, got %d", len(row.KEKSalt))
	}

	// Verify the wrapped DEK unwraps back to the cached DEK using the
	// same signingKey + jti the rehydrate path would derive from.
	keyMaterial := append([]byte{}, signingKey...)
	keyMaterial = append(keyMaterial, []byte(jti)...)
	kek, derr := DeriveKEKFromKey(keyMaterial, row.KEKSalt, JWTSessionKEKInfo)
	if derr != nil {
		t.Fatalf("derive KEK: %v", derr)
	}
	unwrapped, uerr := DecryptSecret(kek, row.WrappedDEK)
	if uerr != nil {
		t.Fatalf("unwrap DEK from durable row: %v", uerr)
	}
	if !bytes.Equal(unwrapped, cachedDEK) {
		t.Errorf("durable DEK does not match cached DEK")
	}
}

func TestKeyService_UnlockDEKWithSigningKey_DurableWriteFailureIsNonFatal(t *testing.T) {
	// Login durable write failure must NOT fail login — the Redis cache
	// is still valid for the JWT's lifetime. A warn-level log records
	// the failure; the user keeps logging in even when PG hiccups.
	store := newMockKeyStore()
	cache := newMockDEKCache()
	jwtStore := newMockJWTSessionStore()
	jwtStore.writeErr = errors.New("transient PG error")

	svc := NewKeyService(store, cache)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	svc.SetJWTSessionStore(jwtStore)
	ctx := context.Background()

	password := []byte("pw")
	_ = svc.InitializeUserKeysServerKEK(ctx, "u-2", "server_kek")

	if err := svc.UnlockDEKWithSigningKey(ctx, "u-2", password, uuid.New().String(), time.Hour, []byte("sk")); err != nil {
		t.Errorf("UnlockDEKWithSigningKey must not fail on durable-write error, got: %v", err)
	}
}

func TestKeyService_UnlockDEKWithSigningKey_NoStoreSkipsDurableWrite(t *testing.T) {
	// Pre-Epic-56 deploys or unit tests without SetJWTSessionStore must
	// still work; UnlockDEK falls back to Redis-only.
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("pw")
	_ = svc.InitializeUserKeysServerKEK(ctx, "u-3", "server_kek")

	if err := svc.UnlockDEKWithSigningKey(ctx, "u-3", password, uuid.New().String(), time.Hour, []byte("sk")); err != nil {
		t.Errorf("no store should still work: %v", err)
	}
}

func TestKeyService_UnlockDEKWithSigningKey_NilSigningKeySkipsDurableWrite(t *testing.T) {
	// API-key callers, tests, and the legacy UnlockDEK path all pass
	// nil — durable write is skipped; only Redis cache is populated.
	store := newMockKeyStore()
	cache := newMockDEKCache()
	jwtStore := newMockJWTSessionStore()
	svc := NewKeyService(store, cache)
	svc.SetJWTSessionStore(jwtStore)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("pw")
	_ = svc.InitializeUserKeysServerKEK(ctx, "u-4", "server_kek")

	if err := svc.UnlockDEKWithSigningKey(ctx, "u-4", password, uuid.New().String(), time.Hour, nil); err != nil {
		t.Fatalf("nil signing key should still succeed: %v", err)
	}
	// No durable row written
	if jwtStore.WriteCount != 0 {
		t.Errorf("nil signing key must not write durable row, WriteCount=%d", jwtStore.WriteCount)
	}
}

func TestKeyService_UnlockDEKWithSigningKey_NonUUIDSessionIDSkipsDurableWrite(t *testing.T) {
	// API-key sessionIDs like "apikey:hash" or legacy non-UUID strings
	// don't belong in jwt_sessions. UnlockDEKWithSigningKey skips the
	// durable write rather than panicking on uuid.Parse.
	store := newMockKeyStore()
	cache := newMockDEKCache()
	jwtStore := newMockJWTSessionStore()
	svc := NewKeyService(store, cache)
	svc.SetJWTSessionStore(jwtStore)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("pw")
	_ = svc.InitializeUserKeysServerKEK(ctx, "u-5", "server_kek")

	if err := svc.UnlockDEKWithSigningKey(ctx, "u-5", password, "apikey:hash", time.Hour, []byte("sk")); err != nil {
		t.Errorf("non-uuid sessionID should not fail unlock: %v", err)
	}
	if jwtStore.WriteCount != 0 {
		t.Errorf("non-uuid sessionID must not write durable row, WriteCount=%d", jwtStore.WriteCount)
	}
}

func TestKeyService_UnlockDEK_BackwardCompatible(t *testing.T) {
	// The old 5-arg UnlockDEK must keep working unchanged for non-JWT
	// callers (Register, tests). Equivalent to passing nil signing key.
	store := newMockKeyStore()
	cache := newMockDEKCache()
	jwtStore := newMockJWTSessionStore()
	svc := NewKeyService(store, cache)
	svc.SetJWTSessionStore(jwtStore)
	svc.SetAPIKeyStore(nil, &recordingProvider{})
	ctx := context.Background()

	password := []byte("pw")
	_ = svc.InitializeUserKeysServerKEK(ctx, "u-7", "server_kek")

	if err := svc.UnlockDEK(ctx, "u-7", password, uuid.New().String(), time.Hour); err != nil {
		t.Fatalf("legacy UnlockDEK: %v", err)
	}
	if jwtStore.WriteCount != 0 {
		t.Errorf("legacy UnlockDEK must not write durable row, WriteCount=%d", jwtStore.WriteCount)
	}
}

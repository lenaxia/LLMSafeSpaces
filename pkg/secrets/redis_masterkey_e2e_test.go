// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration
// +build integration

package secrets

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

func getTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6380"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Skipping Redis integration test: %v", err)
	}
	return client
}

// TestE2E_MasterKey_FullLifecycle proves that with a master key:
// 1. DEK is encrypted in Redis (not readable as plain hex)
// 2. The full secret lifecycle still works (create, decrypt, inject)
// 3. Without the master key, the DEK cannot be recovered from Redis
func TestE2E_MasterKey_FullLifecycle(t *testing.T) {
	client := getTestRedis(t)
	defer client.Close()
	ctx := context.Background()

	// Clean up test keys
	defer client.Del(ctx, "dek:e2e-master-key-session")

	masterKey := []byte("e2e-test-master-key-32-bytes!!")
	if len(masterKey) != 32 {
		masterKey = []byte("e2etestmasterkey0123456789abcdef") // exactly 32
	}

	// Create cache WITH master key
	cache := NewRedisDEKCache(client, masterKey)

	// Create key store and secret store (in-memory for this test)
	keyStore := newMockKeyStore()
	secretStore := newMockSecretStore()
	keySvc := NewKeyService(keyStore, cache)
	keySvc.SetAPIKeyStore(nil, &recordingProvider{})
	secretSvc := NewSecretService(keySvc, secretStore)

	userID := "e2e-mk-user"
	password := []byte("e2e-password")
	sessionID := "e2e-master-key-session"

	// === Phase 1: Initialize keys and unlock DEK ===
	err := keySvc.InitializeUserKeysServerKEK(ctx, userID, "server_kek")
	if err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}

	err = keySvc.UnlockDEK(ctx, userID, password, sessionID, time.Hour)
	if err != nil {
		t.Fatalf("UnlockDEK: %v", err)
	}

	// === Phase 2: Verify DEK is encrypted in Redis ===
	rawVal, err := client.Get(ctx, "dek:"+sessionID).Result()
	if err != nil {
		t.Fatalf("Redis GET: %v", err)
	}

	// Plain DEK would be 64 hex chars (32 bytes). Encrypted should be longer (nonce + tag).
	if len(rawVal) <= 64 {
		t.Fatalf("DEK in Redis should be encrypted (>64 chars), got %d chars", len(rawVal))
	}
	t.Logf("DEK in Redis: %d hex chars (encrypted, not plain 64)", len(rawVal))

	// Try to decode as plain hex — should NOT yield a valid 32-byte key
	rawBytes, _ := hex.DecodeString(rawVal)
	if len(rawBytes) == 32 {
		t.Error("Raw Redis value should NOT be a plain 32-byte DEK")
	}

	// === Phase 3: Create a secret (uses the cached DEK) ===
	created, err := secretSvc.CreateSecret(ctx, userID, sessionID, nil, CreateSecretRequest{
		Name:     "e2e-master-key-secret",
		Type:     SecretTypeAPIKey,
		Value:    "sk-super-secret-value-that-must-be-encrypted",
		Metadata: json.RawMessage(`{"kind":"test","slug":"test"}`),
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}

	// === Phase 4: Decrypt the secret (proves DEK retrieval works through master key) ===
	plaintext, err := secretSvc.DecryptSecretValue(ctx, userID, sessionID, nil, created.ID)
	if err != nil {
		t.Fatalf("DecryptSecretValue: %v", err)
	}
	if string(plaintext) != "sk-super-secret-value-that-must-be-encrypted" {
		t.Errorf("Decrypted value wrong: %s", string(plaintext))
	}

	// === Phase 5: Prepare injection (proves full flow works) ===
	_, _ = secretSvc.SetBindings(ctx, userID, "ws-mk-test", []string{created.ID})
	injData, err := secretSvc.InjectSecrets(ctx, userID, sessionID, nil, "ws-mk-test")
	if err != nil {
		t.Fatalf("InjectSecrets: %v", err)
	}
	var injected []InjectedSecret
	json.Unmarshal(injData, &injected)
	if len(injected) != 1 || injected[0].Plaintext != "sk-super-secret-value-that-must-be-encrypted" {
		t.Errorf("Injection wrong: %v", injected)
	}

	// === Phase 6: Wrong master key cannot read the DEK ===
	wrongCache := NewRedisDEKCache(client, []byte("wrong-master-key-32-bytes-xxxxxx"))
	wrongDEK, err := wrongCache.GetDEK(ctx, sessionID)
	if err == nil && wrongDEK != nil {
		t.Error("Wrong master key should NOT be able to read DEK")
	}

	// === Phase 7: No master key cannot read the DEK ===
	plainCache := NewRedisDEKCache(client)
	plainDEK, err := plainCache.GetDEK(ctx, sessionID)
	if err != nil {
		// hex.DecodeString will succeed but the result won't be a valid 32-byte DEK
		// (it'll be the encrypted blob decoded from hex)
		t.Logf("No master key read: err=%v (expected — encrypted value isn't valid plain hex DEK)", err)
	} else if len(plainDEK) == 32 {
		// If it happens to decode to 32 bytes, verify it's NOT the real DEK
		realDEK, _ := cache.GetDEK(ctx, sessionID)
		for i := range realDEK {
			if realDEK[i] != plainDEK[i] {
				goto different
			}
		}
		t.Error("Plain cache should NOT return the real DEK")
	different:
	}

	t.Log("E2E Master Key: all 7 phases passed — DEK encrypted in Redis, full lifecycle works")
}

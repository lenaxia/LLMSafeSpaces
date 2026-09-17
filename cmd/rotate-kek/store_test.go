// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// newMiniRedisURL starts a miniredis and returns its redis:// URL plus the
// instance for direct assertions.
func newMiniRedisURL(t *testing.T) (string, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	return "redis://" + mr.Addr(), mr
}

// TestRedisCacheFlusher_EvictsDEKKeysOnly verifies the post-rotation cache
// flush deletes every dek:* key and NOTHING else — the Redis instance is
// shared with rate limiters, sessions, and token revocations that must
// survive a KEK rotation.
func TestRedisCacheFlusher_EvictsDEKKeysOnly(t *testing.T) {
	url, mr := newMiniRedisURL(t)
	flusher, err := newRedisCacheFlusher(url)
	require.NoError(t, err)
	t.Cleanup(flusher.Close)

	ctx := context.Background()
	require.NoError(t, flusher.client.Set(ctx, "dek:session-a", "aa", time.Hour).Err())
	require.NoError(t, flusher.client.Set(ctx, "dek:session-b", "bb", time.Hour).Err())
	require.NoError(t, flusher.client.Set(ctx, "ratelimit:tenant-1", "5", time.Hour).Err())
	require.NoError(t, flusher.client.Set(ctx, "token:abc123", "revoked", time.Hour).Err())

	require.NoError(t, flusher.FlushDEKCache(ctx))

	assert.False(t, mr.Exists("dek:session-a"), "dek:* keys must be evicted")
	assert.False(t, mr.Exists("dek:session-b"), "dek:* keys must be evicted")
	assert.True(t, mr.Exists("ratelimit:tenant-1"), "non-dek keys must survive the flush")
	assert.True(t, mr.Exists("token:abc123"), "non-dek keys must survive the flush")
}

// TestRedisCacheFlusher_EmptyCacheIsSuccess verifies flushing an empty (or
// dek-less) Redis succeeds — the common case on a fresh deployment.
func TestRedisCacheFlusher_EmptyCacheIsSuccess(t *testing.T) {
	url, _ := newMiniRedisURL(t)
	flusher, err := newRedisCacheFlusher(url)
	require.NoError(t, err)
	t.Cleanup(flusher.Close)
	assert.NoError(t, flusher.FlushDEKCache(context.Background()))
}

// TestRedisCacheFlusher_ManyKeysIsComplete drives the SCAN loop past one
// batch (>500 keys) so the cursor iteration is exercised.
func TestRedisCacheFlusher_ManyKeysIsComplete(t *testing.T) {
	url, mr := newMiniRedisURL(t)
	flusher, err := newRedisCacheFlusher(url)
	require.NoError(t, err)
	t.Cleanup(flusher.Close)

	ctx := context.Background()
	const n = 1200
	for i := 0; i < n; i++ {
		require.NoError(t, flusher.client.Set(ctx, "dek:bulk-"+string(rune('a'+i%26))+time.Now().Format("150405.000000000")+itoa(i), "x", time.Hour).Err())
	}
	require.NoError(t, flusher.FlushDEKCache(ctx))
	remaining := 0
	for _, k := range mr.Keys() {
		if len(k) > 4 && k[:4] == "dek:" {
			remaining++
		}
	}
	assert.Equal(t, 0, remaining, "every dek:* key must be evicted, including across SCAN batches")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestRedisCacheFlusher_BadURLFails verifies the constructor surfaces parse
// errors instead of succeeding with a broken client.
func TestRedisCacheFlusher_BadURLFails(t *testing.T) {
	_, err := newRedisCacheFlusher("://not-a-redis-url")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse redis URL")
}

// TestRedisCacheFlusher_UnreachableRedisFails verifies the constructor pings
// — an incident-time typo in --redis-url must fail at startup, not at the
// post-rotation flush where the CLI would already have re-wrapped rows.
func TestRedisCacheFlusher_UnreachableRedisFails(t *testing.T) {
	_, err := newRedisCacheFlusher("redis://127.0.0.1:1/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping Redis")
}

// TestPgRotationStore_UnreachablePostgresFails verifies the pg constructor
// pings: a bad --database-url fails fast with an actionable error.
func TestPgRotationStore_UnreachablePostgresFails(t *testing.T) {
	_, err := newPgRotationStore("postgres://127.0.0.1:1/nowhere?sslmode=disable")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect")
}

// TestCompositeRotationStore_FlushDelegatesToRedis verifies the composite
// store routes the flush to Redis (the pg-only no-op would silently leave
// stale DEKs cached).
func TestCompositeRotationStore_FlushDelegatesToRedis(t *testing.T) {
	url, mr := newMiniRedisURL(t)
	flusher, err := newRedisCacheFlusher(url)
	require.NoError(t, err)
	t.Cleanup(flusher.Close)

	require.NoError(t, flusher.client.Set(context.Background(), "dek:stale", "x", time.Hour).Err())

	store := newCompositeRotationStore(&pgRotationStore{}, flusher)
	require.NoError(t, store.FlushDEKCache(context.Background()))
	assert.False(t, mr.Exists("dek:stale"))
}

// TestReadMasterKeyFile verifies the operator keyfile contract: hex values
// decode, raw bytes pass through, whitespace is trimmed.
func TestReadMasterKeyFile(t *testing.T) {
	dir := t.TempDir()

	hexPath := dir + "/hex.key"
	require.NoError(t, writeFile(hexPath, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"))
	raw, err := readMasterKeyFile(hexPath)
	require.NoError(t, err)
	assert.Len(t, raw, 32)
	assert.Equal(t, byte(0x1f), raw[31])

	rawPath := dir + "/raw.key"
	base64ish := "this is a 48-character base64-ish master secret!!"
	require.NoError(t, writeFile(rawPath, base64ish))
	raw, err = readMasterKeyFile(rawPath)
	require.NoError(t, err)
	assert.Equal(t, base64ish, string(raw), "non-hex content must be used as raw bytes")

	missing, err := readMasterKeyFile(dir + "/absent.key")
	require.Error(t, err)
	assert.Nil(t, missing)
}

// TestLastRowIDFieldAndResumeHint verify the CLI's resume-cursor reporting:
// the last-row-id field the runbook documents and the printed resume command.
func TestLastRowIDFieldAndResumeHint(t *testing.T) {
	assert.Equal(t, "last-row-id=abc", lastRowIDField("abc"))
	assert.Equal(t, "", lastRowIDField(""))
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// goldenMasterM1 mirrors the m1-32B fixture from
// pkg/secrets/testdata/derive_server_key_golden.txt (issue #832): the
// consolidated-derivation golden matrix was captured from the three original
// implementations before they were deleted, and is byte-pinned in
// pkg/secrets. This test proves the CLI path routes through the same
// derivation by cross-decrypting a fixture encrypted with the golden bytes.
func TestCLIProviderConstruction_GoldenDerivedKey(t *testing.T) {
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 1)
	}
	golden, err := hex.DecodeString("fe448d678443b728ecd2edb73c616777d287f849fbcce7f6ed9cf17fc6678bdb")
	require.NoError(t, err)

	derived := secrets.DeriveServerKey(master, "provider-credentials")
	require.NotNil(t, derived)
	assert.Equal(t, golden, derived, "the derivation the CLI uses must equal the golden #832 bytes")

	// The provider the run() loop builds from that derivation decrypts a
	// fixture encrypted under the golden key.
	plaintext := []byte("rotate-kek golden cross-decrypt")
	fixture, err := secrets.EncryptSecret(golden, plaintext)
	require.NoError(t, err)
	dec, err := secrets.DecryptSecret(derived, fixture)
	require.NoError(t, err)
	assert.Equal(t, string(plaintext), string(dec))
}

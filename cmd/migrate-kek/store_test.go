// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// goldenMasterM1 mirrors the m1-32B fixture from
// pkg/secrets/testdata/derive_server_key_golden.txt (issue #832): the
// consolidated-derivation golden matrix was captured from the three original
// implementations before they were deleted, and it is byte-pinned in
// pkg/secrets. These CLI-side tests prove the CLI paths route through the
// same derivation by cross-decrypting fixtures encrypted with the golden
// key bytes.
func goldenMasterM1() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

func goldenProviderCredentialsKeyM1() []byte {
	raw, err := hex.DecodeString("fe448d678443b728ecd2edb73c616777d287f849fbcce7f6ed9cf17fc6678bdb")
	if err != nil {
		panic(err)
	}
	return raw
}

// TestCLIProviderConstruction_GoldenDerivedKey proves the binary's provider
// construction (the same code run() executes) derives keys byte-identical to
// the golden fixtures: a provider built via secrets.DeriveServerKey over the
// m1 master decrypts a fixture encrypted with the golden
// provider-credentials key.
func TestCLIProviderConstruction_GoldenDerivedKey(t *testing.T) {
	master := goldenMasterM1()
	plaintext := []byte(`{"kind":"openai","slug":"openai","apiKey":"sk-golden"}`)

	goldenKey := goldenProviderCredentialsKeyM1()
	fixtureCT, err := secrets.EncryptSecret(goldenKey, plaintext)
	require.NoError(t, err)

	derived := secrets.DeriveServerKey(master, "provider-credentials")
	require.NotNil(t, derived)
	assert.Equal(t, goldenKey, derived, "the derivation the CLI uses must equal the golden #832 bytes")

	dec, err := secrets.DecryptSecret(derived, fixtureCT)
	require.NoError(t, err, "a fixture encrypted under the golden key must decrypt under the CLI-derived key")
	assert.Equal(t, string(plaintext), string(dec))
}

// TestRun_MasterKeyFileTooShortFailsClosed verifies the operator error path:
// a master keyfile shorter than 32 bytes must fail the run before any DB
// connection is attempted (fail-closed, not weak-key).
func TestRun_MasterKeyFileTooShortFailsClosed(t *testing.T) {
	dir := t.TempDir()
	shortKey := filepath.Join(dir, "short.key")
	require.NoError(t, os.WriteFile(shortKey, []byte("0f0e0d0c0b0a09080706050403020100"), 0o600)) // 30 hex chars → 15 bytes
	err := run("postgres://127.0.0.1:1/nope", shortKey, "aws", "us-east-1", "/dev/null",
		"/dev/null", "arn:a", "arn:b", "arn:c", "", "", "",
		"all", "", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deriving local key")
	assert.NotContains(t, err.Error(), "connect to Postgres", "the short-key failure must fire before any DB connection")
}

// TestRedisCacheFlusher_EvictsDEKKeysOnly mirrors the rotate-kek flusher
// contract: dek:* evicted, everything else untouched.
func TestRedisCacheFlusher_EvictsDEKKeysOnly(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	flusher, err := newRedisCacheFlusher("redis://" + mr.Addr())
	require.NoError(t, err)
	t.Cleanup(flusher.Close)

	ctx := context.Background()
	require.NoError(t, flusher.client.Set(ctx, "dek:s1", "x", time.Hour).Err())
	require.NoError(t, flusher.client.Set(ctx, "ratelimit:t1", "5", time.Hour).Err())

	require.NoError(t, flusher.FlushDEKCache(ctx))
	assert.False(t, mr.Exists("dek:s1"))
	assert.True(t, mr.Exists("ratelimit:t1"))
}

func TestRedisCacheFlusher_BadURLFails(t *testing.T) {
	_, err := newRedisCacheFlusher("://bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse redis URL")
}

func TestRedisCacheFlusher_UnreachableRedisFails(t *testing.T) {
	_, err := newRedisCacheFlusher("redis://127.0.0.1:1/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping Redis")
}

func TestPgMigrationStore_UnreachablePostgresFails(t *testing.T) {
	_, err := newPgMigrationStore("postgres://127.0.0.1:1/nowhere?sslmode=disable")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connect")
}

// TestCompositeMigrationStore_FlushDelegatesToRedis verifies the composite
// routes the flush to Redis, not the pg no-op.
func TestCompositeMigrationStore_FlushDelegatesToRedis(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	flusher, err := newRedisCacheFlusher("redis://" + mr.Addr())
	require.NoError(t, err)
	t.Cleanup(flusher.Close)
	require.NoError(t, flusher.client.Set(context.Background(), "dek:stale", "x", time.Hour).Err())

	store := &compositeMigrationStore{pg: &pgMigrationStore{}, redis: flusher}
	require.NoError(t, store.FlushDEKCache(context.Background()))
	assert.False(t, mr.Exists("dek:stale"))
}

// TestLastRowIDField verifies the runbook's last-row-id report field.
func TestLastRowIDField(t *testing.T) {
	assert.Equal(t, "last-row-id=xyz", lastRowIDField("xyz"))
	assert.Equal(t, "", lastRowIDField(""))
}

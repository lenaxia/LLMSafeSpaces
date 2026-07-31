// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration
// +build integration

package passkey

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func cleanupPasskeyTables(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	pool.Exec(context.Background(), "DELETE FROM user_passkeys WHERE user_id = $1", userID)
	pool.Exec(context.Background(), "DELETE FROM user_recovery_codes WHERE user_id = $1", userID)
	pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", userID)
}

// ensureTestUser inserts a minimal users row so the FK constraint on
// user_passkeys.user_id is satisfied.
func ensureTestUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email, password_hash, active, role)
		 VALUES ($1, $2, $3, '$2a$12$dummy', true, 'user')
		 ON CONFLICT (id) DO NOTHING`,
		userID, "test-"+userID[:8], userID+"@test.local")
	require.NoError(t, err, "failed to create test user")
}

func TestPgStore_CreateAndGetCredential(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-pk-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	cred := &Credential{
		ID:              uuid.New(),
		UserID:          userID,
		CredentialID:    []byte("cred-id-1"),
		PublicKey:       []byte("pub-key-1"),
		AttestationType: "none",
		SignCount:       0,
		CreatedAt:       time.Now(),
	}
	require.NoError(t, store.CreateCredential(ctx, cred))

	creds, err := store.ListCredentials(ctx, userID)
	require.NoError(t, err)
	require.Len(t, creds, 1)
	assert.Equal(t, []byte("cred-id-1"), creds[0].CredentialID)
	assert.Equal(t, []byte("pub-key-1"), creds[0].PublicKey)
}

func TestPgStore_CreateCredential_DuplicateCredentialID(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-dup-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	credID := []byte("shared-cred-id")
	c1 := &Credential{ID: uuid.New(), UserID: userID, CredentialID: credID, PublicKey: []byte("k1"), AttestationType: "none", CreatedAt: time.Now()}
	c2 := &Credential{ID: uuid.New(), UserID: userID, CredentialID: credID, PublicKey: []byte("k2"), AttestationType: "none", CreatedAt: time.Now()}

	require.NoError(t, store.CreateCredential(ctx, c1))
	err := store.CreateCredential(ctx, c2)
	assert.Error(t, err, "duplicate credential_id must be rejected")
}

func TestPgStore_DeleteCredential_LastCredentialRefused(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-last-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	c := &Credential{ID: uuid.New(), UserID: userID, CredentialID: []byte("only-cred"), PublicKey: []byte("k"), AttestationType: "none", CreatedAt: time.Now()}
	require.NoError(t, store.CreateCredential(ctx, c))

	err := store.DeleteCredential(ctx, userID, c.ID)
	assert.ErrorIs(t, err, ErrLastCredential, "deleting the only credential must be refused")
}

func TestPgStore_CreateCredentialAndRecoveryCodes_Atomic(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-atomic-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	cred := &Credential{ID: uuid.New(), UserID: userID, CredentialID: []byte("atomic-cred"), PublicKey: []byte("k"), AttestationType: "none", CreatedAt: time.Now()}
	hashes := []string{"hash1", "hash2", "hash3"}

	require.NoError(t, store.CreateCredentialAndRecoveryCodes(ctx, cred, hashes))

	// Both credential and recovery codes must be present.
	creds, _ := store.ListCredentials(ctx, userID)
	assert.Len(t, creds, 1)

	codes, _ := store.ListAvailableRecoveryCodes(ctx, userID)
	assert.Len(t, codes, 3)
}

func TestPgStore_ConsumeRecoveryCode(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-rc-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	require.NoError(t, store.CreateRecoveryCodes(ctx, userID, []string{"hash-a", "hash-b"}))

	// Consume one.
	require.NoError(t, store.ConsumeRecoveryCode(ctx, userID, "hash-a"))

	codes, _ := store.ListAvailableRecoveryCodes(ctx, userID)
	assert.Len(t, codes, 1, "one code consumed, one remaining")

	// Consuming the same hash again must fail.
	err := store.ConsumeRecoveryCode(ctx, userID, "hash-a")
	assert.ErrorIs(t, err, ErrRecoveryCodeNotFound, "consumed code cannot be reused")
}

func TestPgStore_UpdateCredentialAfterLogin(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	store := NewPgStore(pool)
	ctx := context.Background()
	userID := "test-signcount-" + uuid.NewString()

	cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)
	defer cleanupPasskeyTables(t, pool, userID)
	ensureTestUser(t, pool, userID)

	c := &Credential{ID: uuid.New(), UserID: userID, CredentialID: []byte("sc-cred"), PublicKey: []byte("k"), AttestationType: "none", SignCount: 0, CreatedAt: time.Now()}
	require.NoError(t, store.CreateCredential(ctx, c))

	require.NoError(t, store.UpdateCredentialAfterLogin(ctx, c.ID, 42, time.Now()))

	creds, _ := store.ListCredentials(ctx, userID)
	require.Len(t, creds, 1)
	assert.Equal(t, uint32(42), creds[0].SignCount)
	assert.NotNil(t, creds[0].LastUsedAt)
}

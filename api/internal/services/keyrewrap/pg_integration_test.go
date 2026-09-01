// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package keyrewrap

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
	"github.com/lenaxia/llmsafespaces/api/migrations"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// PG integration for the US-70.4 store layer (ReconcileKeyStore on
// PgKeyStore) and the migration. Follows the testharness conventions:
// skips without TEST_DATABASE_URL; migrations applied by New(); unique
// per-test user IDs for parallel isolation.

func integrationService(t *testing.T) (*Service, *secrets.PgKeyStore, *secrets.StaticKeyProvider, *testharness.Harness, string) {
	t.Helper()
	h := testharness.New(t)
	store := secrets.NewPgKeyStore(h.Pool())
	provider, err := secrets.NewStaticKeyProvider([]byte("integration-master-key-32-bytes!"))
	require.NoError(t, err)
	s := New(store, nil, provider, nil, nil, nil, Config{BatchSize: 10, HaltOnVerifyFailures: 3})
	return s, store, provider, h, h.ID()
}

func ensureKeyUser(t *testing.T, ctx context.Context, h *testharness.Harness, userID string) {
	t.Helper()
	_, err := h.Pool().Exec(ctx,
		`INSERT INTO users (id, username, email, password_hash, active, role)
		 VALUES ($1, $2, $3, 'hash', true, 'user') ON CONFLICT DO NOTHING`,
		userID, "krewrap-"+userID, userID+"@test.example")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupKeyUser(h, userID) }) //nolint:contextcheck // teardown must outlive the test-scoped ctx (its deadline may have passed)
}

// cleanupKeyUser is a named helper (not a closure) so contextcheck does
// not demand the test's scoped ctx — teardown intentionally runs on a
// fresh background context after the test's deadline may have passed.
func cleanupKeyUser(h *testharness.Harness, userID string) {
	_, _ = h.Pool().Exec(context.Background(), "DELETE FROM user_keys WHERE user_id = $1", userID)
	_, _ = h.Pool().Exec(context.Background(), "DELETE FROM secret_audit_log WHERE user_id = $1", userID)
	_, _ = h.Pool().Exec(context.Background(), "DELETE FROM users WHERE id = $1", userID)
}

func insertKeyRow(t *testing.T, ctx context.Context, h *testharness.Harness, userID string, wrapped []byte, version int, updatedAgo time.Duration) {
	t.Helper()
	_, err := h.Pool().Exec(ctx,
		`INSERT INTO user_keys (user_id, key_version, wrapped_dek, created_at, updated_at)
		 VALUES ($1, $2, $3, NOW() - $4::interval, NOW() - $4::interval)`,
		userID, version, wrapped, fmt.Sprintf("%d seconds", int(updatedAgo.Seconds())))
	require.NoError(t, err)
}

// Migration idempotency: applying 000030's SQL a second time (the
// columns already exist via testharness.New's migrate-up) is a no-op,
// and the NOT NULL / DEFAULT constraints hold.
func TestMigration000030_Idempotent(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()

	sqlBytes, err := migrations.FS.ReadFile("000030_key_rewrap_retention.up.sql")
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, string(sqlBytes))
	require.NoError(t, err, "re-applying 000030 must be a no-op (IF NOT EXISTS / WHERE IS NULL)")

	var nullable string
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		  WHERE table_name = 'user_keys' AND column_name = 'updated_at'`).Scan(&nullable))
	assert.Equal(t, "NO", nullable, "updated_at must be NOT NULL after backfill")
}

// Walk ordering: real SQL returns rows oldest-first (highest risk =
// longest untouched), with the deterministic user_id tiebreak.
func TestListUserKeysForReconcile_OldestFirst(t *testing.T) {
	_, store, provider, h, id := integrationService(t)
	ctx := context.Background()

	old := "krewrap-old-" + id
	new := "krewrap-new-" + id
	ensureKeyUser(t, ctx, h, old)
	ensureKeyUser(t, ctx, h, new)
	wrapped, err := provider.Encrypt(ctx, testDEK("i"))
	require.NoError(t, err)
	insertKeyRow(t, ctx, h, old, wrapped, 1, 90*24*time.Hour)
	insertKeyRow(t, ctx, h, new, wrapped, 1, time.Hour)

	rows, err := store.ListUserKeysForReconcile(ctx, 100, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2)
	// Relative order (the DB is shared; other rows may interleave): the
	// 90-day-old row must walk before the 1-hour-old row.
	idxOld, idxNew := -1, -1
	for i, r := range rows {
		switch r.UserID {
		case old:
			idxOld = i
		case new:
			idxNew = i
		}
	}
	require.NotEqual(t, -1, idxOld, "old row listed")
	require.NotEqual(t, -1, idxNew, "new row listed")
	assert.Less(t, idxOld, idxNew, "oldest updated_at walks first")
}

// The CAS race: two concurrent heals of the same row (as two API
// replicas would produce) — exactly one wins; the loser sees won=false
// and the row carries the winner's wrap + retention.
func TestCompareAndSwapWrappedDEK_ConcurrentRaceExactlyOneWins(t *testing.T) {
	_, store, provider, h, id := integrationService(t)
	ctx := context.Background()
	userID := "krewrap-race-" + id
	ensureKeyUser(t, ctx, h, userID)

	dek := testDEK("r")
	legacyProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	legacyWrap, err := legacyProvider.Encrypt(ctx, dek)
	require.NoError(t, err)
	insertKeyRow(t, ctx, h, userID, legacyWrap, 1, 120*24*time.Hour)

	// Both contenders derived the same newWrap from the same recovered DEK
	// but produce independent Encrypt outputs (fresh nonces) — the CAS must
	// still admit exactly one.
	newWrapA, err := provider.Encrypt(ctx, dek)
	require.NoError(t, err)
	newWrapB, err := provider.Encrypt(ctx, dek)
	require.NoError(t, err)
	prevA, err := provider.Encrypt(ctx, legacyWrap)
	require.NoError(t, err)
	prevB, err := provider.Encrypt(ctx, legacyWrap)
	require.NoError(t, err)

	var wins atomic.Int64
	var wg sync.WaitGroup
	for _, contender := range []struct {
		newWrap []byte
		prev    []byte
	}{
		{newWrapA, prevA},
		{newWrapB, prevB},
	} {
		wg.Add(1)
		go func(c struct {
			newWrap []byte
			prev    []byte
		}) {
			defer wg.Done()
			won, cerr := store.CompareAndSwapWrappedDEK(ctx, userID, legacyWrap, c.newWrap,
				secrets.ActiveVersionOf(provider),
				&secrets.RetainedWrap{Ciphertext: c.prev, KEKVersion: secrets.ActiveVersionOf(provider), Until: time.Now().Add(30 * 24 * time.Hour)})
			require.NoError(t, cerr)
			if won {
				wins.Add(1)
			}
		}(contender)
	}
	wg.Wait()

	assert.Equal(t, int64(1), wins.Load(), "exactly one concurrent heal wins")

	var gotWrap, gotPrev []byte
	var gotVersion int
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT wrapped_dek, wrapped_dek_previous, key_version FROM user_keys WHERE user_id = $1`, userID).
		Scan(&gotWrap, &gotPrev, &gotVersion))
	assert.Equal(t, 1, gotVersion, "static provider active version")
	assert.True(t, bytesEqualAny(gotWrap, newWrapA, newWrapB), "row carries the winner's wrap")
	assert.True(t, bytesEqualAny(gotPrev, prevA, prevB), "row carries the winner's retained wrap")
}

// CAS staleness: an expected value that no longer matches the row
// (concurrent legitimate rotation) loses cleanly without error.
func TestCompareAndSwapWrappedDEK_StaleExpectedLoses(t *testing.T) {
	_, store, provider, h, id := integrationService(t)
	ctx := context.Background()
	userID := "krewrap-stale-" + id
	ensureKeyUser(t, ctx, h, userID)

	dek := testDEK("s")
	legacyProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	legacyWrap, err := legacyProvider.Encrypt(ctx, dek)
	require.NoError(t, err)
	insertKeyRow(t, ctx, h, userID, legacyWrap, 1, 100*24*time.Hour)

	// Legitimate rotation rewrote the row before the reconciler's CAS.
	legitWrap, err := provider.Encrypt(ctx, dek)
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx, `UPDATE user_keys SET wrapped_dek = $2, key_version = 7, updated_at = now() WHERE user_id = $1`, userID, legitWrap)
	require.NoError(t, err)

	newWrap, _ := provider.Encrypt(ctx, dek)
	prev, _ := provider.Encrypt(ctx, legacyWrap)
	won, err := store.CompareAndSwapWrappedDEK(ctx, userID, legacyWrap, newWrap, 1,
		&secrets.RetainedWrap{Ciphertext: prev, KEKVersion: 1, Until: time.Now().Add(30 * 24 * time.Hour)})
	require.NoError(t, err)
	assert.False(t, won, "stale expected wrap must lose the CAS")

	var gotPrev []byte
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT wrapped_dek_previous FROM user_keys WHERE user_id = $1`, userID).Scan(&gotPrev))
	assert.Nil(t, gotPrev, "loser must not write retention columns")
}

// Real retained-wrap round-trip (W10): after a full service heal, the
// previous column decrypts under the CURRENT provider to exactly the
// old wrap bytes, and the new active wrap decrypts to the recovered DEK.
func TestServiceHeal_RetainedWrapRoundTrip(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	userID := "krewrap-heal-" + h.ID()
	ensureKeyUser(t, ctx, h, userID)

	provider, err := secrets.NewStaticKeyProvider([]byte("integration-master-key-32-bytes!"))
	require.NoError(t, err)
	dek := testDEK("h")

	// June-era row under a lost key + one secret encrypted under the DEK
	// (the W9 agreement source).
	legacyProvider, err := secrets.NewStaticKeyProvider([]byte("fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	legacyWrap, err := legacyProvider.Encrypt(ctx, dek)
	require.NoError(t, err)
	insertKeyRow(t, ctx, h, userID, legacyWrap, 1, 90*24*time.Hour)

	secretCT, err := secrets.EncryptSecret(dek, []byte("agreement-value"))
	require.NoError(t, err)
	_, err = h.Pool().Exec(ctx,
		`INSERT INTO user_secrets (user_id, name, type, ciphertext, key_version, metadata)
		 VALUES ($1, 'gh-token', 'env-secret', $2, 1, '{"var_name":"GH_TOKEN"}')`,
		userID, secretCT)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = h.Pool().Exec(context.Background(), "DELETE FROM user_secrets WHERE user_id = $1", userID)
	})

	store := secrets.NewPgKeyStore(h.Pool())
	rec := &fakeRecoverer{dek: dek, jti: "integration-jti"}
	// The production secret lister (PgSecretStore) — the integration test
	// exercises the real W9 agreement path, not a fake.
	lister := secrets.NewPgSecretStore(h.Pool())
	audit := &fakeAudit{}
	s := New(store, rec, provider, lister, audit, nil, Config{BatchSize: 10, HaltOnVerifyFailures: 3})

	stats := s.runPass(ctx)
	assert.Equal(t, 1, stats.rows[outcomeHealed], "row heals end-to-end against real Postgres")

	var gotWrap, gotPrev []byte
	var gotPrevVer *int
	var gotUntil *time.Time
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT wrapped_dek, wrapped_dek_previous, wrapped_dek_previous_kek_version, wrapped_dek_retained_until
		  FROM user_keys WHERE user_id = $1`, userID).
		Scan(&gotWrap, &gotPrev, &gotPrevVer, &gotUntil))

	gotDEK, derr := provider.Decrypt(ctx, gotWrap)
	require.NoError(t, derr)
	assert.Equal(t, dek, gotDEK, "new active wrap decrypts to the recovered DEK")

	recoveredOld, perr := provider.Decrypt(ctx, gotPrev)
	require.NoError(t, perr, "retained wrap decrypts under the CURRENT provider (W10)")
	assert.Equal(t, legacyWrap, recoveredOld, "retained wrap hides the exact old wrap bytes")
	require.NotNil(t, gotPrevVer)
	assert.Equal(t, secrets.ActiveVersionOf(provider), *gotPrevVer)
	require.NotNil(t, gotUntil)

	// The heal is audited.
	assert.Len(t, audit.byAction(auditActionHeal), 1)
}

// Retention cleanup against real rows: expired previous columns NULLed;
// unexpired and healthy rows untouched.
func TestDeleteExpiredRetainedWraps_RealRows(t *testing.T) {
	_, store, provider, h, id := integrationService(t)
	ctx := context.Background()
	expired := "krewrap-exp-" + id
	fresh := "krewrap-fr-" + id
	ensureKeyUser(t, ctx, h, expired)
	ensureKeyUser(t, ctx, h, fresh)

	wrap, _ := provider.Encrypt(ctx, testDEK("x"))
	prev, _ := provider.Encrypt(ctx, wrap)

	_, err := h.Pool().Exec(ctx,
		`INSERT INTO user_keys (user_id, key_version, wrapped_dek, wrapped_dek_previous, wrapped_dek_previous_kek_version, wrapped_dek_retained_until, created_at, updated_at)
		 VALUES ($1, 1, $2, $3, 1, NOW() - INTERVAL '1 day', NOW() - INTERVAL '40 days', NOW() - INTERVAL '31 days')`,
		expired, wrap, prev)
	require.NoError(t, err)
	future := time.Now().Add(29 * 24 * time.Hour)
	_, err = h.Pool().Exec(ctx,
		`INSERT INTO user_keys (user_id, key_version, wrapped_dek, wrapped_dek_previous, wrapped_dek_previous_kek_version, wrapped_dek_retained_until, created_at, updated_at)
		 VALUES ($1, 1, $2, $3, 1, $4, NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day')`,
		fresh, wrap, prev, future)
	require.NoError(t, err)

	n, err := store.DeleteExpiredRetainedWraps(ctx, time.Now())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	var expiredPrev, freshPrev []byte
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT wrapped_dek_previous FROM user_keys WHERE user_id = $1`, expired).Scan(&expiredPrev))
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT wrapped_dek_previous FROM user_keys WHERE user_id = $1`, fresh).Scan(&freshPrev))
	assert.Nil(t, expiredPrev, "expired retention cleaned")
	assert.NotNil(t, freshPrev, "unexpired retention kept")
}

func bytesEqualAny(b []byte, candidates ...[]byte) bool {
	for _, c := range candidates {
		if bytesEqual(b, c) {
			return true
		}
	}
	return false
}

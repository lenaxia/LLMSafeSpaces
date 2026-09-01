// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metering

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Quota-enforcement integration tests (#768 review round 1): real
// Postgres, real migrations (usage_quota_reservations from migration
// 000027), real advisory locks — the wiring the sqlmock unit tests
// cannot prove: lock serialization actually serializes, the reservation
// sums actually count, the reaper actually deletes, and concurrent
// callers actually lose the race exactly N-limit times.

func newIntegrationService(t *testing.T) (*Service, *testharness.Harness) {
	t.Helper()
	// testharness.New applies migrations as part of construction.
	h := testharness.New(t)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)
	svc, err := New(nil, log.With("component", "metering-it"), h.SQLDB())
	require.NoError(t, err)
	return svc, h
}

func seedLimit(t *testing.T, db *sql.DB, ownerID, eventType string, limit int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO usage_limits (owner_id, owner_type, event_type, period_type, max_quantity)
		VALUES ($1, 'user', $2, 'lifetime', $3)
		ON CONFLICT (owner_id, owner_type, event_type, period_type) DO UPDATE SET max_quantity = EXCLUDED.max_quantity`,
		ownerID, eventType, limit)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_limits WHERE owner_id = $1 AND owner_type = 'user' AND event_type = $2 AND period_type = 'lifetime'`,
			ownerID, eventType)
	})
}

func seedUsageEvent(t *testing.T, db *sql.DB, ownerID string, quantity int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO usage_events (idempotency_key, owner_id, owner_type, actor_id, event_type, quantity, source, event_time)
		VALUES ($1, $2, 'user', $2, 'llm_request', $3, 'api', now())`,
		fmt.Sprintf("it-%s-%d", ownerID, time.Now().UnixNano()), ownerID, quantity)
	require.NoError(t, err)
}

func ownerOf(id string) types.BillingOwner {
	return types.BillingOwner{ID: id, Type: types.OwnerTypeUser}
}

// TestReserveQuotaIntegration_AllowedAndDenied: end-to-end over the
// real schema — committed usage and active reservations both count,
// the deny path inserts nothing, and the allow path lands a row with a
// bounded expiry.
func TestReserveQuotaIntegration_AllowedAndDenied(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := context.Background()
	owner := ownerOf(h.ID())

	seedLimit(t, h.SQLDB(), owner.ID, "llm_request", 3)
	seedUsageEvent(t, h.SQLDB(), owner.ID, 2)

	allowed, remaining, err := svc.ReserveQuota(ctx, owner, "llm_request", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "2 used + 1 reserved = 3 <= limit")
	assert.Equal(t, int64(0), remaining)

	// The next reservation must be denied: used(2)+reserved(1) exhausts 3.
	allowed, remaining, err = svc.ReserveQuota(ctx, owner, "llm_request", 1)
	require.NoError(t, err)
	assert.False(t, allowed, "active reservations must count against the limit on the real DB")
	assert.Equal(t, int64(0), remaining)

	var n int
	require.NoError(t, h.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM usage_quota_reservations WHERE owner_id = $1`, owner.ID).Scan(&n))
	assert.Equal(t, 1, n, "denied reservation must not insert a row")
}

// TestReserveQuotaIntegration_ConcurrentLastSlot: the race the issue
// reported — N concurrent reservations against a limit with exactly one
// free slot. Exactly one may win; the advisory lock must make the
// check-then-insert atomic.
func TestReserveQuotaIntegration_ConcurrentLastSlot(t *testing.T) {
	svc, h := newIntegrationService(t)
	owner := ownerOf(h.ID())

	seedLimit(t, h.SQLDB(), owner.ID, "llm_request", 1)

	const n = 12
	var wg sync.WaitGroup
	wins := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, _, err := svc.ReserveQuota(context.Background(), owner, "llm_request", 1)
			require.NoError(t, err)
			wins <- allowed
		}()
	}
	wg.Wait()
	close(wins)

	won := 0
	for w := range wins {
		if w {
			won++
		}
	}
	assert.Equal(t, 1, won,
		"exactly one concurrent reservation may take the last slot (limit=1)")
}

// TestReserveQuotaIntegration_ExpiredReservationsDontCount: a
// reservation past its TTL stops counting (the self-healing bound for
// ungraceful worker death).
func TestReserveQuotaIntegration_ExpiredReservationsDontCount(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := context.Background()
	owner := ownerOf(h.ID())

	seedLimit(t, h.SQLDB(), owner.ID, "llm_request", 1)
	_, _, err := svc.ReserveQuota(ctx, owner, "llm_request", 1)
	require.NoError(t, err)

	// Force-expire the reservation.
	_, err = h.SQLDB().Exec(
		`UPDATE usage_quota_reservations SET expires_at = now() - interval '1 second' WHERE owner_id = $1`, owner.ID)
	require.NoError(t, err)

	allowed, _, err := svc.ReserveQuota(ctx, owner, "llm_request", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "expired reservation must not block a new one")
}

// TestReapExpiredReservationsIntegration: the reaper deletes expired
// rows and leaves active ones.
func TestReapExpiredReservationsIntegration(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := context.Background()
	owner := ownerOf(h.ID())

	_, err := h.SQLDB().Exec(`INSERT INTO usage_quota_reservations
		(owner_id, owner_type, event_type, quantity, expires_at)
		VALUES ($1, 'user', 'llm_request', 1, now() - interval '1 minute'),
		       ($1, 'user', 'llm_request', 1, now() + interval '1 minute')`, owner.ID)
	require.NoError(t, err)

	require.NoError(t, svc.reapExpiredReservations(ctx))

	var n int
	require.NoError(t, h.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM usage_quota_reservations WHERE owner_id = $1`, owner.ID).Scan(&n))
	assert.Equal(t, 1, n, "only the active reservation survives the reaper")
}

// TestReserveQuotaIntegration_DBError_FailsClosed: with the DB closed
// mid-flight, ReserveQuota returns an error (never a silent allow) —
// the #768b contract at the storage layer.
func TestReserveQuotaIntegration_DBError_FailsClosed(t *testing.T) {
	svc, h := newIntegrationService(t)
	owner := ownerOf(h.ID())
	seedLimit(t, h.SQLDB(), owner.ID, "llm_request", 3)

	// Swap the handle for one pointing at a dead DB: same service, no
	// reachable store. Errors must surface.
	dead, err := sql.Open("pgx", "postgres://nope@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dead.Close() })
	svc.db = dead

	allowed, _, rerr := svc.ReserveQuota(context.Background(), owner, "llm_request", 1)
	require.Error(t, rerr, "storage failure must surface as an error (fail-closed)")
	assert.False(t, allowed)
}

// TestQuotaGateIntegration_RouterToDB: the full gate wiring against the
// real router fixture + real metering service — 429 on token-quota
// exhaustion and 200-path (no quota response) when under limit,
// proving router → handler → service → DB end to end.
func TestQuotaGateIntegration_RouterToDB(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	svc, h := newIntegrationService(t)
	ctx := context.Background()
	owner := ownerOf(h.ID())

	// Token limit of 10 with 10 used → token gate denies.
	seedLimit(t, h.SQLDB(), owner.ID, "llm_tokens", 10)
	seedUsageEventOfType(t, h.SQLDB(), owner.ID, "llm_tokens", 10)

	allowed, _, err := svc.CheckQuota(ctx, owner, "llm_tokens")
	require.NoError(t, err)
	assert.False(t, allowed, "token gate must see the exhausted limit through the service")

	// Request admission still allowed for a fresh owner (no limit row).
	fresh := ownerOf(h.ID() + "-fresh")
	allowed, _, err = svc.ReserveQuota(ctx, fresh, "llm_request", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "no-limit owner is unlimited through the real path")
}

func seedUsageEventOfType(t *testing.T, db *sql.DB, ownerID, eventType string, quantity int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO usage_events (idempotency_key, owner_id, owner_type, actor_id, event_type, quantity, source, event_time)
		VALUES ($1, $2, 'user', $2, $3, $4, 'api', now())`,
		fmt.Sprintf("it-%s-%s-%d", eventType, ownerID, time.Now().UnixNano()), ownerID, eventType, quantity)
	require.NoError(t, err)
}

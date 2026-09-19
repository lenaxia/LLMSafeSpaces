// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsstate

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1340: Redis-backed session-index reconciliation miss counters.
// Whole-map JSON replace with TTL decay; empty map deletes the key.

func newReconcileTestStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisStore(client, testActiveSessTTL), mr
}

func TestRedisStore_ReconcileMisses_RoundTrip(t *testing.T) {
	s, _ := newReconcileTestStore(t)
	ctx := context.Background()

	require.Nil(t, s.GetReconcileMisses(ctx, "ws-1"), "absent key reads nil, not an error")

	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_ghost": 1, "ses_other": 2})
	got := s.GetReconcileMisses(ctx, "ws-1")
	assert.Equal(t, map[string]int{"ses_ghost": 1, "ses_other": 2}, got)

	// Whole-map replace drops absent keys.
	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_ghost": 2})
	got = s.GetReconcileMisses(ctx, "ws-1")
	assert.Equal(t, map[string]int{"ses_ghost": 2}, got)

	// Empty map deletes the key entirely.
	s.SetReconcileMisses(ctx, "ws-1", nil)
	assert.Nil(t, s.GetReconcileMisses(ctx, "ws-1"))
}

func TestRedisStore_ReconcileMisses_TTL(t *testing.T) {
	s, mr := newReconcileTestStore(t)
	ctx := context.Background()

	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_ghost": 1})
	require.NotNil(t, s.GetReconcileMisses(ctx, "ws-1"))

	// Decay: the TTL means abandoned counters expire rather than
	// accumulating forever.
	mr.FastForward(DefaultReconcileMissesTTL + time.Minute)
	assert.Nil(t, s.GetReconcileMisses(ctx, "ws-1"))
}

func TestRedisStore_ReconcileMisses_CorruptPayloadReadsNil(t *testing.T) {
	s, _ := newReconcileTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.client.Set(ctx, reconcileMissesKey("ws-1"), "not-json", time.Minute).Err())
	// The store answers nil (treated as "no misses"), never an error —
	// a corrupt counter must not break the reconciliation pass.
	assert.Nil(t, s.GetReconcileMisses(ctx, "ws-1"))
}

// The cross-replica turn claim — the r2 centerpiece: contention,
// expiry re-arm, and fail-open on outage.
func TestRedisStore_ClaimReconcileTurn_Contention(t *testing.T) {
	s, _ := newReconcileTestStore(t)
	ctx := context.Background()

	// Contention: exactly one claimant wins the window.
	first := s.ClaimReconcileTurn(ctx, "ws-1", time.Minute)
	second := s.ClaimReconcileTurn(ctx, "ws-1", time.Minute)
	assert.True(t, first)
	assert.False(t, second, "a second claimant in the same window must lose")

	// Per-workspace independence.
	assert.True(t, s.ClaimReconcileTurn(ctx, "ws-2", time.Minute))
}

func TestRedisStore_ClaimReconcileTurn_ExpiryRearms(t *testing.T) {
	s, mr := newReconcileTestStore(t)
	ctx := context.Background()

	require.True(t, s.ClaimReconcileTurn(ctx, "ws-1", time.Minute))
	require.False(t, s.ClaimReconcileTurn(ctx, "ws-1", time.Minute))
	mr.FastForward(2 * time.Minute)
	assert.True(t, s.ClaimReconcileTurn(ctx, "ws-1", time.Minute),
		"an expired window must be claimable again")
}

func TestRedisStore_ClaimReconcileTurn_FailOpenOnOutage(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := NewRedisStore(client, testActiveSessTTL)

	// Outage: the cache being DOWN must not block convergence —
	// fail-open (documented in-code).
	mr.Close()
	assert.True(t, s.ClaimReconcileTurn(context.Background(), "ws-1", time.Minute),
		"Redis outage must fail OPEN (the pass is idempotent; blocking convergence inverts the S5b priority)")
}

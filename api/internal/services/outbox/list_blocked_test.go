// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1019 option D: surface frozen-queue state in List ---

// TestList_BlockedByInFlight: pending entries under a lock held by
// another worker carry blockedByInFlight=true and a hold age — the
// "messages silently not sending" signal from the 2026-08-21 incident.
func TestList_BlockedByInFlight(t *testing.T) {
	s, mr := newTestService(t)
	ctx := context.Background()

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello", nil)
	require.NoError(t, err)

	// Simulate another worker holding the session lock for ~5 minutes of
	// its 12-minute TTL (7 minutes remaining).
	require.NoError(t, mr.Set(lockKey("ws", "ses"), "lk_other"))
	mr.SetTTL(lockKey("ws", "ses"), 7*time.Minute)

	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].BlockedByInFlight,
		"pending entry under a foreign lock must surface blockedByInFlight")
	assert.InDelta(t, (LockTTL - 7*time.Minute).Seconds(), entries[0].InFlightFor.Seconds(), 1,
		"hold age = LockTTL - remaining TTL")
}

// TestList_NotBlockedWhenLockFree: no lock → plain pending, no blocked
// flag (the common healthy case).
func TestList_NotBlockedWhenLockFree(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello", nil)
	require.NoError(t, err)

	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].BlockedByInFlight)
	assert.Zero(t, entries[0].InFlightFor)
}

// TestList_DeliveringEntryNotBlocked: entries actively delivering (moved
// to the delivering list by THIS worker) are the healthy in-flight
// case — never marked blocked.
func TestList_DeliveringEntryNotBlocked(t *testing.T) {
	s, mr := newTestService(t)
	ctx := context.Background()

	e, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello", nil)
	require.NoError(t, err)

	// Move the entry to the delivering list with this worker's lock held.
	moveToDelivering(t, s, mr, *e)

	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, StatusDelivering, entries[0].Status)
	assert.False(t, entries[0].BlockedByInFlight,
		"the actively-delivering entry is the turn itself, not a blocked one")
}

// TestList_StaleLockLongHold: a lock held nearly to its TTL expiry (the
// residual SIGKILL window) reports a long hold age — the frozen-queue
// signal operators triage the incident by.
func TestList_StaleLockLongHold(t *testing.T) {
	s, mr := newTestService(t)
	ctx := context.Background()

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello", nil)
	require.NoError(t, err)

	// Stale lock: 30s of TTL remaining = ~11.5 minutes held.
	require.NoError(t, mr.Set(lockKey("ws", "ses"), "lk_dead"))
	mr.SetTTL(lockKey("ws", "ses"), 30*time.Second)

	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].BlockedByInFlight)
	assert.InDelta(t, (LockTTL - 30*time.Second).Seconds(), entries[0].InFlightFor.Seconds(), 1)
}

// moveToDelivering stages an entry the way deliverOne does: push onto
// the delivering list, remove from the main list, hold the lock.
func moveToDelivering(t *testing.T, s *Service, mr *miniredis.Miniredis, e Entry) {
	t.Helper()
	ctx := context.Background()
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(ctx, dKey("ws", "ses"), raw).Err())
	require.NoError(t, s.client.LRem(ctx, qKey("ws", "ses"), 1, raw).Err())
	require.NoError(t, mr.Set(lockKey("ws", "ses"), "lk_self"))
	mr.SetTTL(lockKey("ws", "ses"), LockTTL)
}

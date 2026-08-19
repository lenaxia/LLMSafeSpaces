// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	return New(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

func TestAccept_Dedupe(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	e1, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello", nil)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, e1.Status)

	_, err = s.Accept(ctx, "ws", "ses", "u-1", "cmid-1", "hello again", nil)
	var dup *Duplicate
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, "cmid-1", e1.ClientMessageID)

	// A different clientMessageID is accepted.
	_, err = s.Accept(ctx, "ws", "ses", "u-1", "cmid-2", "second", nil)
	require.NoError(t, err)
}

func TestAccept_EmptyClientMessageID_NoDedupe(t *testing.T) {
	// Legacy clients send no clientMessageID: every accept is distinct
	// (no accidental dedupe), capped by the session cap.
	s, _ := newTestService(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.Accept(ctx, "ws", "ses", "u-1", "", "m", nil)
		require.NoError(t, err)
	}
	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	assert.Len(t, entries, 3)
}

func TestAccept_Cap(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	for i := 0; i < Cap; i++ {
		_, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-cap-"+string(rune('a'+i)), "m", nil)
		require.NoError(t, err)
	}
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "cmid-overflow", "m", nil)
	assert.ErrorIs(t, err, ErrCapped)
}

func TestDeliver_Success(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "hello", nil)
	require.NoError(t, err)

	var got Entry
	done := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, e Entry) error {
		got = e
		return nil
	})
	require.True(t, done)
	assert.Equal(t, "hello", got.Text)

	entries, err := s.List(ctx, "ws", "ses")
	require.NoError(t, err)
	assert.Empty(t, entries, "delivered entry is removed from the outbox")
}

func TestDeliver_RetryThenTerminal(t *testing.T) {
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "will fail", nil)
	require.NoError(t, err)

	fail := errors.New("adapter down")
	for attempt := 1; attempt < MaxAttempts; attempt++ {
		ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return fail })
		require.True(t, ok)
		time.Sleep(2 * time.Millisecond) // let the backoff gate elapse
		entries, _ := s.List(ctx, "ws", "ses")
		require.Len(t, entries, 1)
		assert.Equal(t, StatusPending, entries[0].Status, "attempt %d retries, not terminal", attempt)
		assert.Equal(t, attempt, entries[0].Attempts)
	}

	// Final attempt parks as error.
	time.Sleep(2 * time.Millisecond)
	ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return fail })
	require.True(t, ok)
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
	assert.Contains(t, entries[0].LastError, "adapter down")
}

func TestDeliver_BackoffGateSkipsFutureEntries(t *testing.T) {
	// r1 finding 6: retries must not exhaust in tick-time — a failed
	// entry is SKIPPED (not consumed) until NextAttemptAt elapses, so
	// attempts span opencode restart windows instead of 5 seconds.
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = 10*time.Second, 10*time.Second
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "failing", nil)
	require.NoError(t, err)

	calls := 0
	ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		calls++
		return errors.New("down")
	})
	require.True(t, ok)
	require.Equal(t, 1, calls)

	// Within the backoff window: the entry is skipped, no delivery call.
	ok = s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		calls++
		return errors.New("down")
	})
	assert.False(t, ok, "backoff-gated entry must be skipped, not retried instantly")
	assert.Equal(t, 1, calls)
}

func TestDeliver_ErrorParksInPlace_OrderFlows(t *testing.T) {
	// An error entry must not block later entries (the incident's
	// head-of-line stall, in outbox form).
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "first-will-fail", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws", "ses", "u-1", "c2", "second", nil)
	require.NoError(t, err)

	// Drive first to terminal error (tiny backoff so the test is fast).
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	for i := 0; i < MaxAttempts; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, e Entry) error {
			if e.Text == "first-will-fail" {
				return errors.New("nope")
			}
			return nil
		})
		time.Sleep(2 * time.Millisecond)
	}

	// Second still delivers.
	var delivered []string
	for i := 0; i < 3; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, e Entry) error {
			delivered = append(delivered, e.Text)
			return nil
		})
	}
	assert.Contains(t, delivered, "second")

	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
	assert.Equal(t, "first-will-fail", entries[0].Text)
}

func TestDeliver_SessionLock(t *testing.T) {
	// The per-session lock must prevent a second concurrent worker from
	// double-delivering the same entry.
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "hello", nil)
	require.NoError(t, err)

	calls := 0
	d := func(_ context.Context, _, _ string, _ Entry) error { calls++; return nil }
	done1 := s.deliverOne(ctx, "ws", "ses", d)
	require.True(t, done1)
	// Lock was released on completion: a second pass finds no work.
	done2 := s.deliverOne(ctx, "ws", "ses", d)
	assert.False(t, done2, "no double delivery")
	assert.Equal(t, 1, calls)
}

func TestRecover_StagedEntriesRequeued(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "staged", nil)
	require.NoError(t, err)

	// Simulate a crash mid-delivery: entry moved to staging, then the
	// process died before delivery or removal.
	vals, err := s.client.LRange(ctx, qKey("ws", "ses"), 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, vals, 1)
	s.client.LRem(ctx, qKey("ws", "ses"), 1, vals[0])
	s.client.RPush(ctx, dKey("ws", "ses"), vals[0])

	n := s.Recover(ctx)
	assert.Equal(t, 1, n)
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusPending, entries[0].Status, "recovered entry is deliverable again")
}

func TestDismissAndRetry(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	e, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "x", nil)
	require.NoError(t, err)
	// Park it as error (tiny backoff).
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	for i := 0; i < MaxAttempts; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return errors.New("nope") })
		time.Sleep(2 * time.Millisecond)
	}
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)

	// Retry resets it.
	assert.True(t, s.Retry(ctx, "ws", "ses", e.ID))
	entries, _ = s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusPending, entries[0].Status)
	assert.Zero(t, entries[0].Attempts)

	// Dismiss removes it.
	assert.True(t, s.Dismiss(ctx, "ws", "ses", e.ID))
	entries, _ = s.List(ctx, "ws", "ses")
	assert.Empty(t, entries)

	assert.False(t, s.Dismiss(ctx, "ws", "ses", "ob_missing"), "unknown id is a no-op")
}

func TestRun_EndToEnd(t *testing.T) {
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	s, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "hello", json.RawMessage(`{"modelID":"m","providerID":"p"}`))
	require.NoError(t, err)

	got := make(chan Entry, 1)
	go s.Run(ctx, func(_ context.Context, _, _ string, e Entry) error {
		got <- e
		return nil
	}, 5*time.Millisecond)

	select {
	case e := <-got:
		assert.Equal(t, "hello", e.Text)
		assert.Equal(t, "c1", e.ClientMessageID)
		assert.JSONEq(t, `{"modelID":"m","providerID":"p"}`, string(e.Model))
	case <-time.After(3 * time.Second):
		t.Fatal("worker never delivered the accepted entry")
	}

	require.Eventually(t, func() bool {
		entries, _ := s.List(ctx, "ws", "ses")
		return len(entries) == 0
	}, 2*time.Second, 10*time.Millisecond, "delivered entry leaves the outbox")
}

// TestAccept_CappedWritesNoDedupeMarker (r1 finding 3): a failed accept
// (cap, push error) must leave NO marker — a stale marker turns every
// retry into a false "duplicate" and silently drops the message.
func TestAccept_CappedWritesNoDedupeMarker(t *testing.T) {
	origCap := Cap
	Cap = 1
	t.Cleanup(func() { Cap = origCap })
	s, _ := newTestService(t)
	ctx := context.Background()

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "cm-1", "first", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws", "ses", "u-1", "cm-2", "second", nil)
	require.ErrorIs(t, err, ErrCapped)

	// No marker for the capped accept: after draining, the retry succeeds.
	ok := s.Dismiss(ctx, "ws", "ses", mustFirstID(t, s))
	require.True(t, ok)
	e, err := s.Accept(ctx, "ws", "ses", "u-1", "cm-2", "second", nil)
	require.NoError(t, err, "retry after cap-drain must accept, not false-duplicate")
	assert.NotEmpty(t, e.ID)
}

func mustFirstID(t *testing.T, s *Service) string {
	t.Helper()
	entries, err := s.List(context.Background(), "ws", "ses")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	return entries[0].ID
}

// TestAccept_DuplicateCarriesOriginalID (r1 finding 7): the duplicate
// return must carry the ORIGINAL entry ID so the client can correlate.
func TestAccept_DuplicateCarriesOriginalID(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	e1, err := s.Accept(ctx, "ws", "ses", "u-1", "cm-1", "first", nil)
	require.NoError(t, err)

	_, err = s.Accept(ctx, "ws", "ses", "u-1", "cm-1", "retry", nil)
	var dup *Duplicate
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, e1.ID, dup.AcceptedID, "duplicate carries the original messageID")
}

// TestDeliver_CrashWindowBothLists_NoLoss (r1 finding 4): the stage-out
// ordering (RPUSH staging BEFORE LREM main) means a crash between the two
// leaves the entry in BOTH — Recover must dedupe by ID, never lose it.
func TestDeliver_CrashWindowBothLists_NoLoss(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	e, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "crash-window", nil)
	require.NoError(t, err)

	// Simulate the crash: entry duplicated into staging AND left in main.
	vals, _ := s.client.LRange(ctx, qKey("ws", "ses"), 0, -1).Result()
	require.Len(t, vals, 1)
	s.client.RPush(ctx, dKey("ws", "ses"), vals[0]) // staging copy, main NOT removed

	n := s.Recover(ctx)
	assert.Equal(t, 0, n, "recover must NOT duplicate an entry already in main")

	// The entry survives exactly once and delivers.
	var got []string
	for i := 0; i < 3; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, de Entry) error {
			got = append(got, de.Text)
			return nil
		})
	}
	assert.Equal(t, []string{"crash-window"}, got, "delivered exactly once (dedupe by ID), never lost")
	entries, _ := s.List(ctx, "ws", "ses")
	assert.Empty(t, entries)
	_ = e
}

// TestDeliver_LockOutlivesLongTurn (r1 finding 5): LockTTL must exceed
// DeliveryTimeout — a long turn must not expire its own lock.
func TestDeliver_LockOutlivesLongTurn(t *testing.T) {
	require.Greater(t, LockTTL, DeliveryTimeout,
		"lock TTL must exceed the delivery timeout or a long turn lets a second worker in")
}

// TestRun_ConcurrentSessionsNoHeadOfLine (r1 finding 11): one slow
// session must not block another session's delivery — Run delivers
// sessions concurrently.
func TestRun_ConcurrentSessionsNoHeadOfLine(t *testing.T) {
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	s, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := s.Accept(ctx, "ws", "slow", "u-1", "c-slow", "slow", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws", "fast", "u-1", "c-fast", "fast", nil)
	require.NoError(t, err)

	fastDone := make(chan time.Time, 1)
	start := time.Now()
	go s.Run(ctx, func(ctx context.Context, _, ses string, _ Entry) error {
		if ses == "slow" {
			select {
			case <-ctx.Done():
			case <-time.After(300 * time.Millisecond): // long turn
			}
			return nil
		}
		fastDone <- time.Now()
		return nil
	}, 5*time.Millisecond)

	select {
	case at := <-fastDone:
		assert.Less(t, at.Sub(start).Milliseconds(), int64(300),
			"fast session delivered while the slow turn was still in flight — no head-of-line starvation")
	case <-time.After(3 * time.Second):
		t.Fatal("fast session starved behind the slow session's long turn")
	}
}

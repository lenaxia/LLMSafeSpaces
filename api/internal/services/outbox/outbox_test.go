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

	e1, err := s.Accept(ctx, "ws", "ses", "cmid-1", "hello", nil)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, e1.Status)

	_, err = s.Accept(ctx, "ws", "ses", "cmid-1", "hello again", nil)
	var dup *Duplicate
	require.ErrorAs(t, err, &dup)
	assert.Equal(t, "cmid-1", e1.ClientMessageID)

	// A different clientMessageID is accepted.
	_, err = s.Accept(ctx, "ws", "ses", "cmid-2", "second", nil)
	require.NoError(t, err)
}

func TestAccept_EmptyClientMessageID_NoDedupe(t *testing.T) {
	// Legacy clients send no clientMessageID: every accept is distinct
	// (no accidental dedupe), capped by the session cap.
	s, _ := newTestService(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.Accept(ctx, "ws", "ses", "", "m", nil)
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
		_, err := s.Accept(ctx, "ws", "ses", "cmid-cap-"+string(rune('a'+i)), "m", nil)
		require.NoError(t, err)
	}
	_, err := s.Accept(ctx, "ws", "ses", "cmid-overflow", "m", nil)
	assert.ErrorIs(t, err, ErrCapped)
}

func TestDeliver_Success(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "c1", "hello", nil)
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
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "c1", "will fail", nil)
	require.NoError(t, err)

	fail := errors.New("adapter down")
	for attempt := 1; attempt < MaxAttempts; attempt++ {
		ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return fail })
		require.True(t, ok)
		entries, _ := s.List(ctx, "ws", "ses")
		require.Len(t, entries, 1)
		assert.Equal(t, StatusPending, entries[0].Status, "attempt %d retries, not terminal", attempt)
		assert.Equal(t, attempt, entries[0].Attempts)
	}

	// Final attempt parks as error.
	ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return fail })
	require.True(t, ok)
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
	assert.Contains(t, entries[0].LastError, "adapter down")
}

func TestDeliver_ErrorParksInPlace_OrderFlows(t *testing.T) {
	// An error entry must not block later entries (the incident's
	// head-of-line stall, in outbox form).
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "c1", "first-will-fail", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws", "ses", "c2", "second", nil)
	require.NoError(t, err)

	// Drive first to terminal error.
	for i := 0; i < MaxAttempts; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, e Entry) error {
			if e.Text == "first-will-fail" {
				return errors.New("nope")
			}
			return nil
		})
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
	_, err := s.Accept(ctx, "ws", "ses", "c1", "hello", nil)
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
	_, err := s.Accept(ctx, "ws", "ses", "c1", "staged", nil)
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
	e, err := s.Accept(ctx, "ws", "ses", "c1", "x", nil)
	require.NoError(t, err)
	// Park it as error.
	for i := 0; i < MaxAttempts; i++ {
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return errors.New("nope") })
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
	origBackoff := RetryBackoff
	RetryBackoff = time.Millisecond
	t.Cleanup(func() { RetryBackoff = origBackoff })
	s, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := s.Accept(ctx, "ws", "ses", "c1", "hello", json.RawMessage(`{"modelID":"m","providerID":"p"}`))
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

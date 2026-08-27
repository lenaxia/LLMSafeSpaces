// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
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
	assert.Equal(t, StatusVerifying, entries[0].Status, "recovered entry outcome is unknown — verify, never blind re-send")
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

	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "hello", json.RawMessage(`{"modelID":"m","providerID":"p"}`))
	require.NoError(t, err)

	got := make(chan Entry, 1)
	deliveriesDone := make(chan struct{}, 64) // buffered: the deferred signal must never block delivery
	var deliveries int64
	var runWG sync.WaitGroup
	// Stop the worker AND any in-flight delivery BEFORE this test
	// returns (var-mutating tests may follow): cancel, wait for Run,
	// then wait for the last observed delivery goroutine to return.
	defer func() {
		cancel()
		runWG.Wait()
		for i := atomic.LoadInt64(&deliveries); i > 0; i-- {
			<-deliveriesDone
		}
	}()
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		s.Run(ctx, func(_ context.Context, _, _ string, e Entry) error {
			atomic.AddInt64(&deliveries, 1)
			defer func() { deliveriesDone <- struct{}{} }()
			got <- e
			return nil
		}, 5*time.Millisecond)
	}()

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

// --- Ambiguity / verification state machine (incident 2026-08-20, #987) ---

// shortVerifyVars compresses the verify timers for tests.
func shortVerifyVars(t *testing.T) {
	t.Helper()
	origDelay, origVB, origMaxVB, origMVA := VerifyDelay, VerifyBackoff, MaxVerifyBackoff, MaxVerifyAttempts
	VerifyDelay, VerifyBackoff, MaxVerifyBackoff = time.Millisecond, time.Millisecond, time.Millisecond
	MaxVerifyAttempts = 3
	t.Cleanup(func() {
		VerifyDelay, VerifyBackoff, MaxVerifyBackoff, MaxVerifyAttempts = origDelay, origVB, origMaxVB, origMVA
	})
}

// TestDeliver_AmbiguousMovesToVerifying: a deliverer outcome of unknown
// result (timeout mid-turn, transport cut mid-flight) must NOT be retried
// as a fresh send — the send most likely persisted (opencode persists
// before the turn starts). The entry moves to verifying and the delivery
// attempt does not count toward MaxAttempts.
func TestDeliver_AmbiguousMovesToVerifying(t *testing.T) {
	shortVerifyVars(t)
	s, _ := newTestService(t)
	s.SetVerifier(func(context.Context, string, string, Entry) Verdict { return VerdictInconclusive })
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "long turn", nil)
	require.NoError(t, err)

	ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		return Ambiguous(errors.New("context deadline exceeded"))
	})
	require.True(t, ok)

	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusVerifying, entries[0].Status, "ambiguous outcome moves the entry to verifying")
	assert.Zero(t, entries[0].Attempts, "ambiguous attempts must not count toward MaxAttempts")
	assert.False(t, entries[0].LastAttemptAt.IsZero(), "the send window start is recorded for the verifier")

	// A second pass while verifying must NOT re-send (verifier inconclusive keeps it verifying).
	sent := 0
	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		sent++
		return nil
	})
	assert.Zero(t, sent, "verifying entries are verified, never re-sent blindly")
}

// TestVerifying_DeliveredRemovesAndNotifies: a verifier verdict of
// delivered confirms the outbox entry's removal — the ONLY completion
// path besides the synchronous 2xx — and fires the OnDelivered hook once
// (SSE queue.update/sent + metering ride that hook in the bridge).
func TestVerifying_DeliveredRemovesAndNotifies(t *testing.T) {
	shortVerifyVars(t)
	s, _ := newTestService(t)
	var notified []Entry
	s.SetOnDelivered(func(_ string, _ string, e Entry) { notified = append(notified, e) })
	s.SetVerifier(func(context.Context, string, string, Entry) Verdict { return VerdictDelivered })
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "confirmed by history", nil)
	require.NoError(t, err)

	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		return Ambiguous(errors.New("context deadline exceeded"))
	})
	time.Sleep(3 * time.Millisecond) // verify delay
	ok := s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		t.Fatal("re-send after ambiguous outcome")
		return nil
	})
	require.True(t, ok)

	entries, _ := s.List(ctx, "ws", "ses")
	assert.Empty(t, entries, "verified-delivered entry leaves the outbox")
	require.Len(t, notified, 1)
	assert.Equal(t, "confirmed by history", notified[0].Text)
}

// TestVerifying_AbsentRestoresPending: verifier proves the message never
// reached the transcript — a definitive not-delivered. The entry returns
// to pending and the attempt counts toward MaxAttempts normally.
func TestVerifying_AbsentRestoresPending(t *testing.T) {
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	shortVerifyVars(t)
	s, _ := newTestService(t)
	s.SetVerifier(func(context.Context, string, string, Entry) Verdict { return VerdictAbsent })
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "never landed", nil)
	require.NoError(t, err)

	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		return Ambiguous(errors.New("connection reset"))
	})
	time.Sleep(3 * time.Millisecond)
	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		t.Fatal("must not re-send in the same pass that verified absent")
		return nil
	})

	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusPending, entries[0].Status, "proven-absent returns to pending for a fresh send")
	assert.Equal(t, 1, entries[0].Attempts, "absent is a definitive failure and counts")
}

// TestVerifying_InconclusiveParksAfterBound: an unreachable verifier
// (agent down, suspended workspace) keeps the entry verifying with
// backoff, bounded by MaxVerifyAttempts, then parks as error — visible,
// dismissable, retryable. It must NEVER be silently re-sent.
func TestVerifying_InconclusiveParksAfterBound(t *testing.T) {
	shortVerifyVars(t)
	s, _ := newTestService(t)
	var sends int
	s.SetVerifier(func(context.Context, string, string, Entry) Verdict { return VerdictInconclusive })
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "agent unreachable", nil)
	require.NoError(t, err)

	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		sends++
		return Ambiguous(errors.New("context deadline exceeded"))
	})

	for i := 0; i < MaxVerifyAttempts+2; i++ {
		time.Sleep(3 * time.Millisecond)
		s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
			sends++
			return nil
		})
	}

	assert.Equal(t, 1, sends, "exactly one send ever — never a blind retry")
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status, "unverifiable entry parks as error after the bound")
	assert.NotEmpty(t, entries[0].LastError)

	// Retry (the queue UI action) resets it for a fresh send.
	assert.True(t, s.Retry(ctx, "ws", "ses", entries[0].ID))
	entries, _ = s.List(ctx, "ws", "ses")
	assert.Equal(t, StatusPending, entries[0].Status)
	assert.Zero(t, entries[0].VerifyAttempts)
}

// TestDeliver_SuccessFiresOnDelivered: the synchronous 2xx path fires the
// same OnDelivered hook as the verified path — one completion seam.
func TestDeliver_SuccessFiresOnDelivered(t *testing.T) {
	s, _ := newTestService(t)
	var notified int
	s.SetOnDelivered(func(string, string, Entry) { notified++ })
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "fast turn", nil)
	require.NoError(t, err)

	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error { return nil })
	assert.Equal(t, 1, notified)
}

// TestDeliver_AmbiguousWithoutVerifierFallsBack: legacy wiring (no
// verifier) degrades ambiguous outcomes to the pre-#987 retry path
// rather than stranding entries in verifying forever.
func TestDeliver_AmbiguousWithoutVerifierFallsBack(t *testing.T) {
	origBackoff, origMax := RetryBackoff, MaxBackoff
	RetryBackoff, MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryBackoff, MaxBackoff = origBackoff, origMax })
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws", "ses", "u-1", "c1", "no verifier", nil)
	require.NoError(t, err)

	s.deliverOne(ctx, "ws", "ses", func(_ context.Context, _, _ string, _ Entry) error {
		return Ambiguous(errors.New("context deadline exceeded"))
	})
	entries, _ := s.List(ctx, "ws", "ses")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusPending, entries[0].Status, "no verifier wired: legacy retry semantics")
	assert.Equal(t, 1, entries[0].Attempts)
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
	slowDone := make(chan struct{}, 1)
	// slowStarted fires when the slow session's DELIVERER is entered.
	// Load-bearing for teardown: a slow-session worker spawn is LIKELY,
	// not guaranteed — the scheduler may cancel Run before that tick's
	// worker runs, in which case slowDone never fires (the entry simply
	// stays pending — correct outbox semantics; the next Run delivers
	// it). The join below waits for slowDone ONLY when the delivery
	// actually started. Waiting unconditionally produced phantom 5s
	// stalls (4MB goroutine dumps: join blocked on <-slowDone, every
	// worker already gone, nothing left to wake it).
	slowStarted := make(chan struct{}, 1)
	// testDone wakes the slow-fn on TEST teardown. Load-bearing: the
	// deliverer receives a DETACHED context (deliverDetached — the D3
	// disconnect-immunity contract), so waiting on ITS ctx.Done() can
	// never observe the test's cancel(); under scheduler contention the
	// 300ms timer alone made this test's cleanup stall past the package
	// timeout (release-run flakes, 2× on 2026-08-20).
	testDone := make(chan struct{})
	start := time.Now()
	var runWG sync.WaitGroup
	// Cancel, join Run, then join the in-flight slow delivery — no
	// goroutine of this test outlives it (var-mutating tests follow).
	// Bounded: if any join stalls, fail FAST with a diagnostic instead of
	// hanging the package for 10 minutes.
	defer func() {
		cancel()
		close(testDone)
		joined := make(chan struct{})
		go func() {
			runWG.Wait()
			select {
			case <-slowStarted:
				<-slowDone // delivery began; it must complete (testDone wakes it)
			default:
				// never spawned — nothing pending, nothing to join
			}
			close(joined)
		}()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			// Both-ready select race guard: joined may have closed in the
			// same instant the timer fired (4MB stack dumps of such events
			// showed every worker ALREADY gone — a phantom stall). Re-check
			// non-blockingly before declaring failure.
			select {
			case <-joined:
				return
			default:
			}
			// Diagnose, don't just fail: dump goroutine stacks so the
			// stalling frame is named in the test output.
			buf := make([]byte, 4<<20)
			n := runtime.Stack(buf, true)
			t.Errorf("teardown stalled >5s: Run or the slow delivery did not observe cancellation. Goroutine dump:\n%s", buf[:n])
		}
	}()
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		s.Run(ctx, func(ctx context.Context, _, ses string, _ Entry) error {
			if ses == "slow" {
				slowStarted <- struct{}{}
				defer func() { slowDone <- struct{}{} }()
				select {
				case <-testDone: // test teardown — independent of the detached ctx
				case <-time.After(300 * time.Millisecond): // long turn
				}
				return nil
			}
			fastDone <- time.Now()
			return nil
		}, 5*time.Millisecond)
	}()

	select {
	case at := <-fastDone:
		assert.Less(t, at.Sub(start).Milliseconds(), int64(300),
			"fast session delivered while the slow turn was still in flight — no head-of-line starvation")
	case <-time.After(3 * time.Second):
		t.Fatal("fast session starved behind the slow session's long turn")
	}
}

func TestSweepWorkspaceUnverifiable_OnlyParkedUnverifiable(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	// Three entries on the target workspace's session:
	//  1. parked unverifiable (the suspend/resume casualty) → must sweep
	//  2. parked with a DIFFERENT error → must stay
	//  3. pending → untouched
	_, err := s.Accept(ctx, "ws-a", "ses-1", "u-1", "cm-1", "m1", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws-a", "ses-1", "u-1", "cm-2", "m2", nil)
	require.NoError(t, err)
	_, err = s.Accept(ctx, "ws-a", "ses-1", "u-1", "cm-3", "m3", nil)
	require.NoError(t, err)
	// An unverifiable entry on ANOTHER workspace → must not be touched.
	_, err = s.Accept(ctx, "ws-b", "ses-9", "u-2", "cm-4", "m4", nil)
	require.NoError(t, err)

	// Mutate entries in place via the service client (same primitive the worker uses).
	fix := func(ws, ses, cmid, status, lastErr string) {
		qk := qKey(ws, ses)
		vals, _ := s.client.LRange(ctx, qk, 0, -1).Result()
		for i, v := range vals {
			var cand Entry
			if json.Unmarshal([]byte(v), &cand) == nil && cand.ClientMessageID == cmid {
				cand.Status = status
				cand.LastError = lastErr
				cand.VerifyAttempts = 5
				cand.NextAttemptAt = time.Now().UTC().Add(time.Hour)
				raw, _ := json.Marshal(cand)
				s.client.LSet(ctx, qk, int64(i), string(raw))
			}
		}
	}
	fix("ws-a", "ses-1", "cm-1", StatusError, lastErrUnverifiable)
	fix("ws-a", "ses-1", "cm-2", StatusError, "delivery confirmed absent; retry bound reached")
	fix("ws-b", "ses-9", "cm-4", StatusError, lastErrUnverifiable)
	n, err := s.SweepWorkspaceUnverifiable(ctx, "ws-a")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	entries, err := s.List(ctx, "ws-a", "ses-1")
	require.NoError(t, err)
	byCMID := map[string]Entry{}
	for _, e := range entries {
		byCMID[e.ClientMessageID] = e
	}
	assert.Equal(t, StatusVerifying, byCMID["cm-1"].Status)
	assert.Zero(t, byCMID["cm-1"].VerifyAttempts, "verify attempts must reset so the sweep grants a full fresh window")
	assert.True(t, !byCMID["cm-1"].NextAttemptAt.After(time.Now().UTC().Add(time.Minute)), "swept entry must be due immediately, not backoff-gated an hour out")
	assert.Equal(t, StatusError, byCMID["cm-2"].Status, "non-unverifiable errors must stay parked")
	assert.Equal(t, StatusPending, byCMID["cm-3"].Status)

	foreignEntries, err := s.List(ctx, "ws-b", "ses-9")
	require.NoError(t, err)
	assert.Equal(t, StatusError, foreignEntries[0].Status, "other workspaces must not be swept")
}

func TestDeliverOne_FiresOnStaged(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws-a", "ses-1", "u-1", "cm-1", "m1", nil)
	require.NoError(t, err)

	var mu sync.Mutex
	var stagedIDs []string
	s.SetOnStaged(func(ws, ses string, e Entry) {
		mu.Lock()
		defer mu.Unlock()
		if ws == "ws-a" && ses == "ses-1" {
			stagedIDs = append(stagedIDs, e.ID)
		}
	})

	ok := s.DeliverOnce(ctx, "ws-a", "ses-1", func(ctx context.Context, ws, ses string, e Entry) error {
		return nil
	})
	require.True(t, ok)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, stagedIDs, 1, "onStaged must fire exactly once when the entry is picked up")
}

func TestDeliverOnce_OnStagedFiresBeforeDelivererReturns(t *testing.T) {
	// The signal's whole purpose is clearing the pill DURING a long
	// turn: the hook must fire at staging (before the deliverer blocks),
	// not at completion alongside onDelivered.
	s, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Accept(ctx, "ws-a", "ses-1", "u-1", "cm-1", "m1", nil)
	require.NoError(t, err)

	staged := make(chan struct{}, 1)
	release := make(chan struct{})
	s.SetOnStaged(func(ws, ses string, e Entry) { staged <- struct{}{} })

	done := make(chan bool, 1)
	go func() {
		done <- s.DeliverOnce(ctx, "ws-a", "ses-1", func(ctx context.Context, ws, ses string, e Entry) error {
			// Hold the "turn" open until the test has observed the
			// staged signal — simulates the multi-minute sync send.
			<-release
			return nil
		})
	}()

	select {
	case <-staged:
		close(release)
		require.True(t, <-done)
	case <-time.After(2 * time.Second):
		t.Fatal("onStaged did not fire while the deliverer was still running")
	}
}

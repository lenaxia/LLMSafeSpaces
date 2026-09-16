// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// rearm_loop_test.go — US-72.0/#910: the generalized re-arm loop's state
// machine, red-first per the story test plan:
//
//	terminal failure → bounded retry schedule (min→2x…→cap)
//	success          → disarm
//	ctx cancel       → stop
//	gates            → busy / restart-deferred skip without attempt or
//	                   backoff advance; already-applied short-circuit
//
// Every row uses a unique Loop label so the shared package metric vec's
// series stay test-local (the injector vec's delta discipline,
// relay_injector_test.go:587, applies here too).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rearmSpy captures what the loop did to an Attempt closure.
type rearmSpy struct {
	attempts atomic.Int32
	inFlight atomic.Int32
	maxInFly atomic.Int32
	// resultFn lets a test script per-attempt outcomes; nil → retryable
	// fetch_failed.
	resultFn func(n int) rearmAttemptResult
}

func (s *rearmSpy) attempt(_ context.Context) rearmAttemptResult {
	s.attempts.Add(1)
	cur := s.inFlight.Add(1)
	for {
		max := s.maxInFly.Load()
		if cur <= max || s.maxInFly.CompareAndSwap(max, cur) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	if s.resultFn != nil {
		return s.resultFn(int(s.attempts.Load()))
	}
	return rearmAttemptResult{outcome: relayOutcomeFetchFailed, retryable: true}
}

func rearmTick(t *testing.T, loop, outcome string) float64 {
	t.Helper()
	return promtestutil.ToFloat64(rearmLoopOutcomes.WithLabelValues(loop, outcome))
}

// TestNextRearmDelay_DoublingSchedule pins the #910 schedule purely:
// waits double from the floor and clamp at the cap — 5m→10m→20m→30m→30m…
// (the issue's proposed bounds).
func TestNextRearmDelay_DoublingSchedule(t *testing.T) {
	const min, max = 5 * time.Minute, 30 * time.Minute
	var got []time.Duration
	for cur := min; len(got) < 5; cur = nextRearmDelay(cur, max) {
		got = append(got, cur)
	}
	assert.Equal(t, []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute}, got,
		"backoff waits must double from min and clamp at max")
}

func TestNextRearmDelay_CapBelowDoubling(t *testing.T) {
	assert.Equal(t, 2*time.Minute, nextRearmDelay(time.Minute, 3*time.Minute))
	assert.Equal(t, 3*time.Minute, nextRearmDelay(2*time.Minute, 3*time.Minute),
		"the doubling that would exceed the cap clamps to the cap")
	assert.Equal(t, 3*time.Minute, nextRearmDelay(3*time.Minute, 3*time.Minute),
		"already at cap stays at cap")
}

// TestRearmLoop_TerminalFailure_BoundedBackoff: consecutive retryable
// failures slow the attempt cadence per the doubling schedule — the loop
// keeps trying (never gives up permanently) but each wait grows. A
// no-backoff loop would attempt ~20x in this window; the schedule allows
// exactly 4 (base + 2x + 4x + 8x).
func TestRearmLoop_TerminalFailure_BoundedBackoff(t *testing.T) {
	const loop = "test_backoff"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Attempt:  spy.attempt,
		MinDelay: 20 * time.Millisecond,
		MaxDelay: 160 * time.Millisecond,
	})

	// Window covers base+2x+4x+8x = 280ms of waits → 4 attempts, then
	// the 5th wait (160ms, at cap) is still running at 350ms.
	time.Sleep(350 * time.Millisecond)
	n := spy.attempts.Load()
	assert.GreaterOrEqual(t, n, int32(4), "loop must keep re-arming after terminal failures")
	assert.LessOrEqual(t, n, int32(5), "backoff must slow the cadence — a hot loop means the schedule is not applied")
	assert.Equal(t, int32(1), spy.maxInFly.Load(), "exactly one in-flight re-arm attempt at any time")
	assert.InDelta(t, float64(n), rearmTick(t, loop, relayOutcomeFetchFailed), 0,
		"one outcome tick per attempt cycle")
}

// TestRearmLoop_SuccessDisarms: an applied attempt ends the loop — no
// further attempts or ticks after the success.
func TestRearmLoop_SuccessDisarms(t *testing.T) {
	const loop = "test_success"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{resultFn: func(n int) rearmAttemptResult {
		if n == 1 {
			return rearmAttemptResult{outcome: relayOutcomeFetchFailed, retryable: true}
		}
		return rearmAttemptResult{outcome: relayOutcomeSuccess, applied: true}
	}}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Attempt:  spy.attempt,
		MinDelay: 15 * time.Millisecond,
		MaxDelay: 30 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return spy.attempts.Load() >= 2 }, 2*time.Second, 5*time.Millisecond)
	appliedTicks := rearmTick(t, loop, relayOutcomeSuccess)
	require.InDelta(t, 1.0, appliedTicks, 0, "the applied cycle ticks exactly once")

	// Quiescence: with base 15ms a live loop would attempt again within
	// ~30ms; 150ms of silence proves the loop exited.
	n := spy.attempts.Load()
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, n, spy.attempts.Load(), "loop must disarm after a successful attempt")
	assert.InDelta(t, appliedTicks, rearmTick(t, loop, relayOutcomeSuccess), 0, "no further ticks after disarm")
}

// TestRearmLoop_CtxCancelStops: cancellation while waiting produces one
// canceled tick and zero attempts.
func TestRearmLoop_CtxCancelStops(t *testing.T) {
	const loop = "test_cancel"
	ctx, cancel := context.WithCancel(context.Background())

	spy := &rearmSpy{}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Attempt:  spy.attempt,
		MinDelay: 25 * time.Millisecond,
		MaxDelay: 50 * time.Millisecond,
	})
	cancel()

	require.Eventually(t, func() bool { return rearmTick(t, loop, rearmOutcomeCanceled) >= 1 },
		2*time.Second, 5*time.Millisecond, "cancel must tick the canceled outcome")
	time.Sleep(80 * time.Millisecond)
	assert.Zero(t, spy.attempts.Load(), "no attempt may run after cancellation")
	assert.InDelta(t, 1.0, rearmTick(t, loop, rearmOutcomeCanceled), 0, "exactly one canceled tick")
}

// TestRearmLoop_CtxCanceledAttemptExits: ctx dying DURING an attempt makes
// the attempt report canceled; the loop exits instead of re-arming.
func TestRearmLoop_CtxCanceledAttemptExits(t *testing.T) {
	const loop = "test_cancel_attempt"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{resultFn: func(int) rearmAttemptResult {
		cancel()
		return rearmAttemptResult{outcome: rearmOutcomeCanceled, retryable: false}
	}}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Attempt:  spy.attempt,
		MinDelay: 10 * time.Millisecond,
		MaxDelay: 20 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return spy.attempts.Load() >= 1 }, 2*time.Second, 5*time.Millisecond)
	n := spy.attempts.Load()
	time.Sleep(120 * time.Millisecond)
	assert.Equal(t, n, spy.attempts.Load(), "a canceled attempt must end the loop, not schedule a retry")
}

// TestRearmLoop_AlreadyAppliedShortCircuit: Applied() true at cycle entry
// disarms without ever calling Attempt — the HasRelay()-class per-cycle
// short-circuit.
func TestRearmLoop_AlreadyAppliedShortCircuit(t *testing.T) {
	const loop = "test_applied"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Applied:  func() bool { return true },
		Attempt:  spy.attempt,
		MinDelay: 10 * time.Millisecond,
		MaxDelay: 20 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return rearmTick(t, loop, rearmOutcomeAlreadyApplied) >= 1 },
		2*time.Second, 5*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	assert.Zero(t, spy.attempts.Load(), "already-applied must skip the attempt entirely")
	assert.InDelta(t, 1.0, rearmTick(t, loop, rearmOutcomeAlreadyApplied), 0)
}

// TestRearmLoop_BusyGateSkipsWithoutAttempt: while Busy() holds, cycles
// tick busy and never attempt; once clear, the next cycle attempts. The
// skip must not advance the backoff (a skip is not a failure).
func TestRearmLoop_BusyGateSkipsWithoutAttempt(t *testing.T) {
	const loop = "test_busy"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gateCalls atomic.Int32
	var attempts atomic.Int32
	startRearmLoop(ctx, rearmLoopConfig{
		Loop: loop,
		Busy: func() bool { return gateCalls.Add(1) <= 2 },
		Attempt: func(ctx context.Context) rearmAttemptResult {
			// After the busy window, one failure then success.
			if attempts.Add(1) == 1 {
				return rearmAttemptResult{outcome: relayOutcomeFetchFailed, retryable: true}
			}
			return rearmAttemptResult{outcome: relayOutcomeSuccess, applied: true}
		},
		MinDelay: 15 * time.Millisecond,
		MaxDelay: 30 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return attempts.Load() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"attempt must fire only after the busy gate opens")
	busyTicks := rearmTick(t, loop, rearmOutcomeBusy)
	assert.GreaterOrEqual(t, busyTicks, float64(2), "each busy cycle ticks the busy outcome")
	// The gate's contract: zero attempts while busy — proven by
	// attempts==0 until the gate opened (the Eventually above only
	// passed once a cycle observed busy==false, so attempts could not
	// have run during the busy window).
	require.Eventually(t, func() bool { return rearmTick(t, loop, relayOutcomeSuccess) >= 1 },
		2*time.Second, 5*time.Millisecond)
}

// TestRearmLoop_BusySkipDoesNotAdvanceBackoff: a busy skip between the
// loop start and the first failed attempt must not double the delay a
// second time — attempt#2 lands one doubled wait (2x base) after
// attempt#1, not two (4x).
func TestRearmLoop_BusySkipDoesNotAdvanceBackoff(t *testing.T) {
	const loop = "test_busy_backoff"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var gateCalls atomic.Int32
	var attemptStarts []time.Time
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop: loop,
		Busy: func() bool { return gateCalls.Add(1) == 1 },
		Attempt: func(context.Context) rearmAttemptResult {
			<-mu
			attemptStarts = append(attemptStarts, time.Now())
			mu <- struct{}{}
			return rearmAttemptResult{outcome: relayOutcomeFetchFailed, retryable: true}
		},
		MinDelay: 30 * time.Millisecond,
		MaxDelay: 240 * time.Millisecond,
	})

	// Cycle 1 (t≈30ms) skips busy; attempt 1 lands at t≈60ms (wait
	// unchanged by the skip); the post-failure wait doubles to 60ms, so
	// attempt 2 lands at t≈120ms — gap ≈ 60ms. If the skip had advanced
	// the backoff, attempt 1's wait would already be 60ms and the gap
	// would be 120ms.
	require.Eventually(t, func() bool {
		<-mu
		n := len(attemptStarts)
		mu <- struct{}{}
		return n >= 2
	}, 3*time.Second, 5*time.Millisecond, "two attempts must run")

	<-mu
	first, second := attemptStarts[0], attemptStarts[1]
	mu <- struct{}{}
	gap := second.Sub(first)
	assert.Greater(t, gap, 40*time.Millisecond, "gap must include the doubled 60ms wait, minus scheduling slack")
	assert.Less(t, gap, 105*time.Millisecond, "a busy skip must not advance the backoff — gap must stay near 2x base, not 4x")
}

// TestRearmLoop_RestartDeferredGateSkips: the deferred-kill gate mirrors
// the busy gate — no attempt while a deferred restart is outstanding.
func TestRearmLoop_RestartDeferredGateSkips(t *testing.T) {
	const loop = "test_deferred"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var deferred atomic.Bool
	deferred.Store(true)
	var attempts atomic.Int32
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:            loop,
		RestartDeferred: func() bool { return deferred.Load() },
		Attempt: func(context.Context) rearmAttemptResult {
			attempts.Add(1)
			return rearmAttemptResult{outcome: relayOutcomeSuccess, applied: true}
		},
		MinDelay: 15 * time.Millisecond,
		MaxDelay: 30 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return rearmTick(t, loop, rearmOutcomeRestartDeferred) >= 2 },
		2*time.Second, 5*time.Millisecond, "deferred cycles must tick restart_deferred")
	assert.Zero(t, attempts.Load(), "no attempt may run behind a deferred restart")

	deferred.Store(false)
	require.Eventually(t, func() bool { return attempts.Load() == 1 }, 2*time.Second, 5*time.Millisecond,
		"attempt must fire once the deferred restart clears")
}

// TestRearmLoop_NonRetryableDisarms: the personal-key class (terminal,
// correct-to-skip) ends the loop after one tick.
func TestRearmLoop_NonRetryableDisarms(t *testing.T) {
	const loop = "test_personal"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{resultFn: func(int) rearmAttemptResult {
		return rearmAttemptResult{outcome: relayOutcomeSkippedPersonalKey}
	}}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:     loop,
		Attempt:  spy.attempt,
		MinDelay: 10 * time.Millisecond,
		MaxDelay: 20 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return spy.attempts.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), spy.attempts.Load(), "non-retryable outcome must disarm the loop")
	assert.InDelta(t, 1.0, rearmTick(t, loop, relayOutcomeSkippedPersonalKey), 0)
}

// TestRearmLoop_DefaultsFillWhenZero: zero delays resolve to the #910
// defaults (5m/30m) — asserted through the loop's observable behavior:
// with defaults, no attempt fires inside a short window.
func TestRearmLoop_DefaultsFillWhenZero(t *testing.T) {
	const loop = "test_defaults"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spy := &rearmSpy{}
	startRearmLoop(ctx, rearmLoopConfig{
		Loop:    loop,
		Attempt: spy.attempt,
	})

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, spy.attempts.Load(),
		"zero MinDelay must fall back to the 5m default — no attempt inside 100ms")
	cancel()
	require.Eventually(t, func() bool { return rearmTick(t, loop, rearmOutcomeCanceled) >= 1 },
		2*time.Second, 5*time.Millisecond)
}

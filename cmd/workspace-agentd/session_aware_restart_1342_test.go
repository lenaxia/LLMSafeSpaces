// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_aware_restart_1342_test.go — #1342: interrupt-before-force-
// kill + progress-keyed defer (the fixed 15-minute maxDefer is retired).
//
// Decision matrix under test:
//
//	idle                       → restart immediately (unchanged)
//	busy + recent activity     → defer, unbounded (40-min build rule)
//	busy + all sessions stalled→ interrupt every busy session → grace → restart
//
// S12: no platform-initiated harness restart terminates a turn without
// terminal part state — the force path is interrupt-first, the orphan
// sweep (sessionstate) is the backstop.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
)

// orderRecordingProc records the restart call order relative to interrupts.
type orderRecordingProc struct {
	restarts atomic.Int32
	events   chan string // buffered; receives "restart"
}

func newOrderRecordingProc() *orderRecordingProc {
	return &orderRecordingProc{events: make(chan string, 16)}
}

func (p *orderRecordingProc) restart() {
	p.restarts.Add(1)
	p.events <- "restart"
}

func (p *orderRecordingProc) restartCount() int { return int(p.restarts.Load()) }

// recordingInterrupter records interrupt calls with timestamps.
type recordingInterrupter struct {
	calls atomic.Int32
	fail  bool
	// cancelAfterCall, when set, is invoked after the first interrupt
	// lands (the grace-window cancellation shape).
	cancelAfterCall context.CancelFunc
	events          chan string
	seenIDs         []string
	mu              sync.Mutex
}

func newRecordingInterrupter() *recordingInterrupter {
	return &recordingInterrupter{events: make(chan string, 16)}
}

func (r *recordingInterrupter) interrupt(ctx context.Context, sessionID string) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.seenIDs = append(r.seenIDs, sessionID)
	cancel := r.cancelAfterCall
	r.mu.Unlock()
	r.events <- "interrupt:" + sessionID
	if cancel != nil {
		cancel()
	}
	if r.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func (r *recordingInterrupter) callCount() int { return int(r.calls.Load()) }
func (r *recordingInterrupter) interruptedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.seenIDs...)
}

// stallSession marks a session busy with a busy-mark older than bound
// and no event activity — the wedged-turn shape.
func stallSession(tracker *sessionStatusTracker, id string) {
	tracker.set(id, "busy")
	tracker.mu.Lock()
	tracker.busySince[id] = time.Now().Add(-time.Hour)
	tracker.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Item 1: interrupt → grace → restart (order)
// ---------------------------------------------------------------------------

func TestRestart1342_Stalled_IssuesInterruptThenGraceThenRestart(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_wedged")

	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()

	begin := time.Now()
	decided := makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  150 * time.Millisecond,
		Interrupter:  intr.interrupt,
	})
	assert.False(t, decided, "busy session defers the initial decision")

	// Drain ordered events: interrupt must precede restart, and the
	// restart must land after the grace window.
	var order []string
	deadline := time.After(3 * time.Second)
	for len(order) < 2 {
		select {
		case ev := <-proc.events:
			order = append(order, ev)
		case ev := <-intr.events:
			order = append(order, ev)
		case <-deadline:
			t.Fatalf("timed out; events so far: %v", order)
		}
	}
	require.Len(t, order, 2)
	assert.Equal(t, "interrupt:ses_wedged", order[0], "interrupt MUST be issued before the restart (S12)")
	assert.Equal(t, "restart", order[1])
	assert.GreaterOrEqual(t, time.Since(begin).Nanoseconds(), int64(150*time.Millisecond),
		"restart must follow the grace window, not fire immediately after the interrupt")
	require.Equal(t, 1, proc.restartCount())
}

func TestRestart1342_Stalled_InterruptsEveryBusySession(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_a")
	stallSession(tracker, "ses_b")

	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()
	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  40 * time.Millisecond,
		Interrupter:  intr.interrupt,
	})

	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		3*time.Second, 10*time.Millisecond, "force path must restart")
	assert.ElementsMatch(t, []string{"ses_a", "ses_b"}, intr.interruptedIDs(),
		"the force path interrupts EACH busy session before restarting")
}

func TestRestart1342_InterrupterErrorStillRestarts(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_wedged")

	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()
	intr.fail = true // harness ignores / errors the interrupt

	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  40 * time.Millisecond,
		Interrupter:  intr.interrupt,
	})

	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		3*time.Second, 10*time.Millisecond,
		"a failing interrupt must not block the restart — grace expires, restart fires, orphan sweep restores honest state")
	require.Equal(t, 1, intr.callCount())
}

func TestRestart1342_NilInterrupterStillRestarts(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_wedged")

	proc := newOrderRecordingProc()
	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  40 * time.Millisecond,
	})

	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		3*time.Second, 10*time.Millisecond,
		"a nil interrupter (unwired seam) degrades to the pre-1342 force restart, never a stuck defer")
}

// ---------------------------------------------------------------------------
// Item 3: progress-keyed defer — the 40-minute build rule
// ---------------------------------------------------------------------------

func TestRestart1342_ProgressingSessionDefersUnbounded(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_build", "busy")

	proc := newOrderRecordingProc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // the defer goroutine must die with the test
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   100 * time.Millisecond,
		GraceWindow:  30 * time.Millisecond,
		Interrupter:  func(context.Context, string) error { return nil },
	})

	// Simulate a 40-min build: part output arrives continuously, faster
	// than the stall bound. Over 6x the stall bound the restart must
	// never fire.
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_build","part":{"id":"prt_out","type":"text","text":"build output chunk"}}}`)
			}
		}
	}()
	time.Sleep(600 * time.Millisecond)
	close(stop)

	assert.Equal(t, 0, proc.restartCount(),
		"a session streaming part output must NEVER be force-restarted, no matter how long the turn runs")
}

func TestRestart1342_MixedProgressAndStalled_DefersWhileAnyProgress(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_build", "busy")
	stallSession(tracker, "ses_wedged")

	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // the defer goroutine must die with the test
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   80 * time.Millisecond,
		GraceWindow:  30 * time.Millisecond,
		Interrupter:  intr.interrupt,
	})

	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_build","part":{"id":"prt_out","type":"text","text":"chunk"}}}`)
			}
		}
	}()
	time.Sleep(400 * time.Millisecond)
	close(stop)

	assert.Equal(t, 0, proc.restartCount(),
		"a progressing session shields the pod from the force path — its turn must not die for a sibling's wedge")
	assert.Equal(t, 0, intr.callCount(), "no interrupt may be issued while any busy session progresses")
}

func TestRestart1342_ProgressStops_ForcePathFires(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_build", "busy")

	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()
	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   100 * time.Millisecond,
		GraceWindow:  30 * time.Millisecond,
		Interrupter:  intr.interrupt,
	})

	// Stream for a while (deferred)…
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_build","part":{"id":"prt_out","type":"text","text":"chunk"}}}`)
			}
		}
	}()
	time.Sleep(250 * time.Millisecond)
	close(stop)
	assert.Equal(t, 0, proc.restartCount(), "still streaming — deferred")

	// …then the stream goes silent: the session crosses the stall bound
	// and the interrupt-first force path fires.
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		3*time.Second, 10*time.Millisecond,
		"once activity stops past the stall bound, the force path must fire so the credential applies")
	assert.Equal(t, 1, intr.callCount())
}

func TestRestart1342_IdleRestartsImmediately(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_1", "idle")

	proc := newOrderRecordingProc()
	decided := makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
	})
	assert.True(t, decided)
	assert.Equal(t, 1, proc.restartCount())
}

// TestRestart1342_ZeroStallBoundFallsBackToDefault: StallBound=0 must
// fall back to the #1312-family default (30s), NOT mean "everything is
// instantly stalled" — a fresh busy session must still defer.
func TestRestart1342_ZeroStallBoundFallsBackToDefault(t *testing.T) {
	require.Equal(t, sessionstate.LeaseConvergenceBound, restartStallBound,
		"the restart stall bound must be derived from the #1312 lease-clock family (LeaseConvergenceBound)")

	tracker := newSessionStatusTracker()
	tracker.set("ses_fresh", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_fresh","part":{"id":"p","type":"text","text":"x"}}}`)

	proc := newOrderRecordingProc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // the defer goroutine must die with the test
	decided := makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   0, // fallback
	})
	assert.False(t, decided, "a fresh busy session must defer even with StallBound=0 (fallback active)")
	assert.Equal(t, 0, proc.restartCount(),
		"StallBound=0 must not mean 'force now' — the default bound governs")
}

// ---------------------------------------------------------------------------
// H1a preserved: cancellation during the grace window aborts the restart
// ---------------------------------------------------------------------------

func TestRestart1342_ContextCancelDuringGrace_NoRestart(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_wedged")

	ctx, cancel := context.WithCancel(context.Background())
	proc := newOrderRecordingProc()
	intr := newRecordingInterrupter()
	intr.cancelAfterCall = cancel // cancel as soon as the interrupt lands (grace begins)
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  time.Hour, // would hang forever without ctx select
		Interrupter:  intr.interrupt,
	})

	require.Eventually(t, func() bool { return intr.callCount() == 1 },
		3*time.Second, 10*time.Millisecond, "interrupt must have been issued")
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 0, proc.restartCount(),
		"shutdown during the grace window must cancel the force restart")
}

// ---------------------------------------------------------------------------
// Item 4: pending credential apply surfaces while deferred
// ---------------------------------------------------------------------------

func TestRestart1342_PendingApplyLifecycle(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_busy", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_busy","part":{"id":"p","type":"text","text":"x"}}}`)

	pending := newPendingApplyTracker()
	proc := newOrderRecordingProc()
	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  30 * time.Millisecond,
		PendingApply: pending,
	})

	// Deferred → pending surfaces with the busy count.
	require.Eventually(t, func() bool {
		snap := pending.snapshot()
		return snap != nil && snap.BusySessions == 1
	}, 2*time.Second, 10*time.Millisecond, "deferred apply must surface on the pending tracker")
	snap := pending.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, pendingApplyReasonCredentialChange, snap.Reason)

	// Session goes idle → restart fires → pending clears.
	tracker.set("ses_busy", "idle")
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		2*time.Second, 10*time.Millisecond, "idle transition must apply the deferred restart")
	require.Eventually(t, func() bool { return pending.snapshot() == nil },
		2*time.Second, 10*time.Millisecond, "applied restart must clear the pending surface")
}

func TestRestart1342_PendingApplyClearedByForcePath(t *testing.T) {
	tracker := newSessionStatusTracker()
	stallSession(tracker, "ses_wedged")

	pending := newPendingApplyTracker()
	proc := newOrderRecordingProc()
	_ = makeSessionAwareRestartDecision(context.Background(), proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   50 * time.Millisecond,
		GraceWindow:  30 * time.Millisecond,
		PendingApply: pending,
	})

	require.Eventually(t, func() bool { return pending.snapshot() != nil },
		2*time.Second, 10*time.Millisecond, "deferred apply surfaces")
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		3*time.Second, 10*time.Millisecond, "force path fires")
	require.Eventually(t, func() bool { return pending.snapshot() == nil },
		2*time.Second, 10*time.Millisecond, "force-path restart clears the pending surface")
}

func TestRestart1342_PendingApplyClearedOnCancel(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_busy", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_busy","part":{"id":"p","type":"text","text":"x"}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	pending := newPendingApplyTracker()
	proc := newOrderRecordingProc()
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   time.Hour, // never stalls in this test
		PendingApply: pending,
	})

	require.Eventually(t, func() bool { return pending.snapshot() != nil },
		2*time.Second, 10*time.Millisecond, "deferred apply surfaces")
	cancel()
	require.Eventually(t, func() bool { return pending.snapshot() == nil },
		2*time.Second, 10*time.Millisecond, "canceled defer must clear the pending surface")
	assert.Equal(t, 0, proc.restartCount())
}

// TestRestart1342_NilPendingApplyTrackerIsSafe: the relay path passes a
// nil tracker — every method must be nil-safe.
func TestRestart1342_NilPendingApplyTrackerIsSafe(t *testing.T) {
	var pending *pendingApplyTracker
	assert.NotPanics(t, func() {
		pending.begin(1)
		pending.refreshBusy(2)
		pending.clear()
		assert.Nil(t, pending.snapshot())
	})
}

// ---------------------------------------------------------------------------
// WaitGroup tracking preserved (H1c)
// ---------------------------------------------------------------------------

func TestRestart1342_BgWgTracked(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_busy", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_busy","part":{"id":"p","type":"text","text":"x"}}}`)

	bgWg := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	proc := newOrderRecordingProc()
	_ = makeSessionAwareRestartDecision(ctx, proc, tracker, restartDecisionConfig{
		PollInterval: 20 * time.Millisecond,
		StallBound:   time.Hour,
		BgWg:         bgWg,
	})

	cancel()
	waitDone := make(chan struct{})
	go func() {
		bgWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("bgWg.Wait() did not return after cancel — deferred goroutine not tracked")
	}
}

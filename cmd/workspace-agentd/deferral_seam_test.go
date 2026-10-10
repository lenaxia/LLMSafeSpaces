// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// deferral_seam_test.go — the #1576 design steer (TDD: authored before
// the seam exists).
//
// Restart deferral is on probation: the owner leans toward removing it
// or replacing busy-poll deferral with turn-boundary event-driven
// application (the #1507 suspend precedent is the same disease). This
// PR adds NO new deferral machinery — it puts the busy reads the
// restart decision consumes behind a SWAPPABLE SEAM (defer-until-idle
// is one implementation), and surfaces the decision numbers (firing
// count, stall duration — with the orphaned-flag datum already on
// workspace_tracker_busy_resets_total) so the keep/replace/remove call
// is made with data.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// stallObservations reads the histogram's cumulative sample count via
// the default gatherer (testutil.CollectAndCount counts METRICS, not
// observations — a histogram is always "1").
func stallObservations(t *testing.T) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "llmsafespaces_agentd_restart_defer_stall_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// fakeDeferSource is a scriptable deferBusySource: the answers the
// restart decision will see, in order.
type fakeDeferSource struct {
	mu     sync.Mutex
	script []fakeSourceAnswer
}

type fakeSourceAnswer struct {
	busy  []string // busy session IDs (empty = idle → apply)
	force bool     // marker: the force path's partition answer
}

func (f *fakeDeferSource) anyBusyOrUnknown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return false
	}
	return len(f.script[0].busy) > 0
}

func (f *fakeDeferSource) listBusy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return nil
	}
	return append([]string(nil), f.script[0].busy...)
}

func (f *fakeDeferSource) busyPartitions(_ time.Duration) (progressing, stalled []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 || len(f.script[0].busy) == 0 {
		return nil, nil
	}
	if f.script[0].force {
		return nil, append([]string(nil), f.script[0].busy...)
	}
	return append([]string(nil), f.script[0].busy...), nil
}

// advance pops one scripted answer.
func (f *fakeDeferSource) advance() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) > 0 {
		f.script = f.script[1:]
	}
}

// seamProc is the minimal restartableProcess the seam test needs.
type seamProc struct{ restarts atomic.Int64 }

func (p *seamProc) restart() { p.restarts.Add(1) }

// startScriptAdvancer pops one scripted answer per interval on a
// background ticker until the test ends (#1532 de-flake: the script
// used to be advanced from INSIDE an Eventually condition — a
// side-effecting poll that raced the decision goroutine's own reads
// and destroyed an answer per 5ms tick). A slow cadence empties the
// script to its terminal answer and LEAVES it there, so the decision
// goroutine cannot miss a state no matter how the runner schedules it.
func startScriptAdvancer(t *testing.T, src *fakeDeferSource, interval time.Duration) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				src.advance()
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// TestDeferSeam_PolicyIsSwappable: the restart decision runs against a
// NON-tracker source — busy then idle applies the deferred restart by
// the SOURCE's answers alone (the seam is real; defer-until-idle is one
// implementation of it).
func TestDeferSeam_PolicyIsSwappable(t *testing.T) {
	withTestLogger(t)
	proc := &seamProc{}
	src := &fakeDeferSource{script: []fakeSourceAnswer{
		{busy: []string{"ses-a"}}, // defer
		{busy: []string{"ses-a"}}, // still busy
		{},                        // idle → apply
	}}

	deferralsBefore := testutil.ToFloat64(restartDeferralsFired)
	decided := makeSessionAwareRestartDecision(context.Background(), proc, src, restartDecisionConfig{
		PollInterval: 5 * time.Millisecond,
		StallBound:   time.Hour,
	})
	require.False(t, decided, "busy → deferred to the background goroutine (the documented contract: false = deferred)")
	// >= not ==: the counter is package-global; a leaked deferred
	// goroutine from an earlier test can legitimately bump it between
	// the read and this assert — OUR deferral is what must be counted.
	require.GreaterOrEqual(t, testutil.ToFloat64(restartDeferralsFired), deferralsBefore+1.0,
		"each deferral counts — the how-often datum")

	// The deferred goroutine applies when the source turns idle
	// (non-mutating condition; the 15s bound absorbs runner stalls).
	startScriptAdvancer(t, src, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		return proc.restarts.Load() > 0
	}, 15*time.Second, 5*time.Millisecond, "the deferred restart must apply once the source turns idle")
}

// TestDeferSeam_TrackerRemainsTheDefaultSource: the production wiring
// passes the SSE tracker (the current defer-until-idle policy).
func TestDeferSeam_TrackerRemainsTheDefaultSource(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	var _ deferBusySource = tr // compile-time: the tracker satisfies the seam
	require.False(t, tr.anyBusyOrUnknown())
	tr.set("ses-a", "busy")
	require.True(t, tr.anyBusyOrUnknown())
}

// TestDeferSeam_ForceLegObservesStall: the stall histogram's contract
// is applying/forcing/canceling — the FORCE leg is the longest-stall
// case and the datum the owner's keep/replace/remove decision most
// needs (a deferral that never found an idle window). A source stuck
// busy-and-stalled defers, then forces, and the observation lands.
func TestDeferSeam_ForceLegObservesStall(t *testing.T) {
	withTestLogger(t)
	proc := &seamProc{}
	src := &fakeDeferSource{script: []fakeSourceAnswer{
		{busy: []string{"ses-stuck"}},              // defer
		{busy: []string{"ses-stuck"}, force: true}, // stalled → force
	}}

	stallObsBefore := stallObservations(t)
	decided := makeSessionAwareRestartDecision(context.Background(), proc, src, restartDecisionConfig{
		PollInterval: 5 * time.Millisecond,
		StallBound:   time.Hour,
		GraceWindow:  time.Millisecond,
	})
	require.False(t, decided, "busy → deferred")

	startScriptAdvancer(t, src, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		return proc.restarts.Load() > 0
	}, 15*time.Second, 5*time.Millisecond, "the stalled-only path forces the restart")

	// The restart fires inside forceInterruptRestart; the observation
	// lands on the line AFTER — wait for the datum, not just the
	// restart (they are deliberately separate events). >= not ==:
	// the histogram is package-global; another test's deferred
	// goroutine can observe concurrently — OUR observation landing is
	// the datum, and it cannot be subtracted by noise.
	require.Eventually(t, func() bool {
		return stallObservations(t) >= stallObsBefore+1
	}, 15*time.Second, 5*time.Millisecond,
		"the force leg observes its stall duration — deferred-then-forced is the case the decision data exists for")
}

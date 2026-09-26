// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// generation_signal_test.go — #1576 layer 1 wiring + #1573 ask 3 (TDD:
// authored before the implementation).
//
// #1573's live finding: the D2 orphan reset fired on NEITHER
// boot-restore NOR respawn, because in split mode the sidecar never
// learns the supervisor respawned opencode (the supervisor holds no
// tracker by design; onChildStarted = nil). The fix rides observable
// truth that already flows: the sidecar's status poller fetches the
// supervisor's ChildPID — a PID change IS a generation boundary, and
// the FIRST observation is the boot boundary (tracker flags held at
// boot are unverified SSE cache, orphaned by definition).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestSupervisorGenerationSignal_PIDChangeFires: the poll step detects
// the boot boundary (first observation) and every respawn (PID change).
func TestSupervisorGenerationSignal_PIDChangeFires(t *testing.T) {
	gen := newGenerationSignal()

	require.Equal(t, 0, gen.lastKnown(), "no observation yet")
	require.True(t, gen.observe(24), "first observation is the boot boundary")
	require.Equal(t, 24, gen.lastKnown())
	require.False(t, gen.observe(24), "same PID: no generation change")
	require.True(t, gen.observe(1304), "respawn: new PID is a new generation")
	require.Equal(t, 1304, gen.lastKnown())
	require.False(t, gen.observe(1304))
}

// TestD2Reset_TheUnifiedTrackerHook: both topologies share the
// tracker's generation hook — busy → idle, tokens survive, and the
// clear lands in workspace_tracker_busy_resets_total (the
// orphaned-flag-rate datum for the owner's deferral decision).
func TestD2Reset_TheUnifiedTrackerHook(t *testing.T) {
	withTestLogger(t)
	tr := newSessionStatusTracker()
	tr.set("ses-a", "busy")
	tr.set("ses-b", "busy")
	tr.set("ses-c", "idle")
	tr.setPromptTokens("ses-a", 1234)

	before := testutil.ToFloat64(trackerBusyResetsMetricForTest())
	tr.onOpencodeGenerationStart()

	require.Equal(t, "idle", tr.get("ses-a"))
	require.Equal(t, "idle", tr.get("ses-b"))
	require.Equal(t, "idle", tr.get("ses-c"))
	require.Equal(t, int64(1234), tr.getPromptTokens("ses-a"), "context meters survive the reset")
	require.False(t, tr.hasAnyBusy(), "restart deferral is not blocked by ghosts")
	require.InDelta(t, before+2.0, testutil.ToFloat64(trackerBusyResetsMetricForTest()), 0.001,
		"each cleared flag counts into the orphaned-flag datum")
}

// TestGenerationSignalPollerWiring: the REAL poller + socket — the
// generation callback fires on the boot observation and on a respawn
// (the r-lessons: wiring must be revert-proof; a nil callback or a
// broken observe step must fail this test through the production path).
func TestGenerationSignalPollerWiring(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	proc.overrideState.Store(&procStateOverride{pid: 24, state: "running"})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve() // the helper listens; serving is the caller's job
	cc := newControlClient(srv.addr())

	seen := make(chan int, 4)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	startSupervisorStatusPollerWithInterval(ctx, &wg, cc, &supervisorStatusStore{}, func(genPID int) { seen <- genPID }, 5*time.Millisecond)
	defer func() { cancel(); wg.Wait() }()

	// Boot boundary observed.
	require.Eventually(t, func() bool { return len(seen) > 0 }, 5*time.Second, 2*time.Millisecond)
	require.Equal(t, 24, <-seen)

	// Respawn: the PID changes under a live poller.
	proc.overrideState.Store(&procStateOverride{pid: 1304, state: "running"})
	require.Eventually(t, func() bool { return len(seen) > 0 }, 5*time.Second, 2*time.Millisecond)
	require.Equal(t, 1304, <-seen)
}

// trackerBusyResetsMetricForTest exposes the existing D2 datum counter
// (single source with opsMetrics — no duplicate metric).
func trackerBusyResetsMetricForTest() prometheus.Counter {
	return pkgOpsMetrics.trackerBusyResets.WithLabelValues(workspaceIDFromEnv())
}

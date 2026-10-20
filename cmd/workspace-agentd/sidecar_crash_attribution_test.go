// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_crash_attribution_test.go — #1776: sidecar-topology crash
// restarts reach the agent-restart surfacing chain. The supervisor
// (supervise-opencode) classifies the crash at the managedProcess
// decision point (reason crash/oom — the same strings the restart-
// reason marker writes), counts it LIFETIME in the adapter, and serves
// it on the control socket's status; the sidecar's status poller
// derives deltas into agentRestartLog. The A11 asymmetry (agentRestarts
// reading 0 on a sidecar workspace that crashed-respawned repeatedly)
// is closed at the source.

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagedProcAdapter_ObserveCrashRestart_LifetimeCount: the
// supervisor-side counter accumulates for the supervisor's whole
// lifetime (it is NOT the resetting backoff counter) and the report
// carries the latest crash's classification.
func TestManagedProcAdapter_ObserveCrashRestart_LifetimeCount(t *testing.T) {
	a := &managedProcAdapter{}
	assert.Equal(t, crashRestartReport{}, a.CrashRestartReport(),
		"a fresh supervisor reports zero crashes and no attribution")

	a.observeCrashRestart("crash")
	a.observeCrashRestart("oom")

	rep := a.CrashRestartReport()
	assert.Equal(t, 2, rep.CrashRestarts, "the count is lifetime — it accumulates across crash and oom")
	assert.Equal(t, "oom", rep.LastCrashReason, "the latest crash's classification wins")
	assert.False(t, rep.LastCrashAt.IsZero(), "the latest crash's time rides along")
}

// TestControlSocketStatus_CrashAttribution: the status result carries
// the supervisor's crash attribution, and omits the attribution fields
// entirely when no crash was ever classified (no evidence is not a
// crash).
func TestControlSocketStatus_CrashAttribution(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	at := time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	proc.overrideCrash.Store(&crashRestartReport{CrashRestarts: 2, LastCrashReason: "oom", LastCrashAt: at})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	cc := newControlClient(srv.addr())
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, st.CrashRestarts)
	assert.Equal(t, "oom", st.LastCrashReason)
	assert.Equal(t, at, st.LastCrashAt)

	proc.overrideCrash.Store(&crashRestartReport{})
	res, err := cc.call(context.Background(), "status", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, float64(0), res["crash_restarts"], "a zero-crash supervisor still reports the counter")
	for _, key := range []string{"last_crash_reason", "last_crash_at"} {
		_, ok := res[key]
		assert.False(t, ok, "a zero-crash supervisor must omit %q (no evidence is not a crash)", key)
	}
}

// TestControlSocketStatus_CrashAttributionAbsenceOnLegacySupervisor:
// mixed fleet — a supervisor binary predating #1776 does not implement
// the capability; the status result must carry NO crash keys at all and
// the client decode must read no evidence (never a miscount).
func TestControlSocketStatus_CrashAttributionAbsenceOnLegacySupervisor(t *testing.T) {
	withTestLogger(t)
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &legacyStatusProc{})
	go srv.serve()

	cc := newControlClient(srv.addr())
	res, err := cc.call(context.Background(), "status", map[string]any{})
	require.NoError(t, err)
	for _, key := range []string{"crash_restarts", "last_crash_reason", "last_crash_at"} {
		_, ok := res[key]
		assert.False(t, ok, "legacy supervisor status must not carry %q", key)
	}
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	assert.Zero(t, st.CrashRestarts)
	assert.Empty(t, st.LastCrashReason)
	assert.True(t, st.LastCrashAt.IsZero())
}

// legacyStatusProc implements supervisedProcIface WITHOUT the
// crashReporter capability — the wire shape of a pre-#1776 supervisor
// binary.
type legacyStatusProc struct{}

func (legacyStatusProc) Restart(string, int) (bool, bool)     { return true, false }
func (legacyStatusProc) State() (int, string, int, time.Time) { return 42, "running", 0, time.Time{} }
func (legacyStatusProc) SetSpawnEnv(map[string]string)        {}
func (legacyStatusProc) SpawnEnvState() spawnEnvStateReport   { return spawnEnvStateReport{} }
func (legacyStatusProc) RefreshFiles() (string, string)       { return "", "" }

// TestDecodeControlStatus_CrashFields: the client-side decode table
// carries the crash-attribution wire tags (pinned here so a typo in
// the table cannot pass silently), and absent fields decode to
// no-evidence.
func TestDecodeControlStatus_CrashFields(t *testing.T) {
	const at = "2026-10-20T09:30:00Z"
	st, err := decodeControlStatus(map[string]any{
		"child_pid":         11,
		"crash_restarts":    3,
		"last_crash_reason": "crash",
		"last_crash_at":     at,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, st.CrashRestarts)
	assert.Equal(t, "crash", st.LastCrashReason)
	want, err := time.Parse(time.RFC3339, at)
	require.NoError(t, err)
	assert.Equal(t, want, st.LastCrashAt)

	st, err = decodeControlStatus(map[string]any{"child_pid": 11})
	require.NoError(t, err)
	assert.Zero(t, st.CrashRestarts)
	assert.Empty(t, st.LastCrashReason)
	assert.True(t, st.LastCrashAt.IsZero(), "absent fields decode to no evidence")
}

// TestCrashRestartTracker_DeltaRebaseFirstSighting: the sidecar's
// delta policy — first sighting reports the supervisor's lifetime
// count in full (a late-booting sidecar must not read 0, the exact
// asymmetry #1776 closes), increases report the delta, and a decrease
// (the supervisor container restarted; the counter is in-process)
// rebases silently.
func TestCrashRestartTracker_DeltaRebaseFirstSighting(t *testing.T) {
	tr := &crashRestartTracker{}
	assert.Equal(t, 3, tr.observe(3), "first sighting reports the lifetime count in full")
	assert.Equal(t, 0, tr.observe(3), "no change is no record")
	assert.Equal(t, 2, tr.observe(5), "an increase reports the delta")
	assert.Equal(t, 0, tr.observe(4), "a decrease rebases silently — never a negative or replayed record")
	assert.Equal(t, 1, tr.observe(5), "post-rebase increases record normally")
}

// TestSupervisorStatusPoller_RecordsSidecarCrashRestarts: the
// attribution e2e leg — a real control socket, a real poller, and the
// sidecar's restart log. First-sighting, delta (two crashes inside one
// poll window record both), rebase-on-decrease, and post-rebase delta
// all land with crash semantics: reason crash/oom, EMPTY verdict,
// supervisor-side attribution time.
func TestSupervisorStatusPoller_RecordsSidecarCrashRestarts(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	at := time.Now().UTC().Truncate(time.Second)
	proc.overrideCrash.Store(&crashRestartReport{CrashRestarts: 1, LastCrashReason: "crash", LastCrashAt: at})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	log := newAgentRestartLog()
	store := &supervisorStatusStore{}
	wg := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startSupervisorStatusPollerWithInterval(ctx, wg, newControlClient(srv.addr()), store, nil, log, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		snap := log.snapshot()
		return snap != nil && snap.AgentRestarts == 1
	}, 5*time.Second, 20*time.Millisecond, "the first-sighting crash must record")
	snap := log.snapshot()
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "crash", snap.LastAgentRestart.Reason)
	assert.Empty(t, snap.LastAgentRestart.Verdict, "crash attribution claims no watchdog verdict")
	assert.Equal(t, at, snap.LastAgentRestart.At, "attribution time comes from the supervisor's crash clock")

	later := at.Add(30 * time.Second)
	proc.overrideCrash.Store(&crashRestartReport{CrashRestarts: 3, LastCrashReason: "oom", LastCrashAt: later})
	require.Eventually(t, func() bool {
		snap := log.snapshot()
		return snap != nil && snap.AgentRestarts == 3 && snap.LastAgentRestart.Reason == "oom"
	}, 5*time.Second, 20*time.Millisecond, "two crashes inside one poll window must both record; the latest attribution wins")
	snap = log.snapshot()
	assert.Equal(t, later, snap.LastAgentRestart.At)

	proc.overrideCrash.Store(&crashRestartReport{CrashRestarts: 1, LastCrashReason: "crash", LastCrashAt: later.Add(time.Second)})
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 3, log.snapshot().AgentRestarts, "a supervisor restart rebase must not record or replay")

	proc.overrideCrash.Store(&crashRestartReport{CrashRestarts: 2, LastCrashReason: "crash", LastCrashAt: later.Add(2 * time.Second)})
	require.Eventually(t, func() bool {
		return log.snapshot().AgentRestarts == 4
	}, 5*time.Second, 20*time.Millisecond, "post-rebase increases record against the new baseline")

	cancel()
	wg.Wait()
}

// TestSupervisorProcess_CrashRespawnAttributedInAdapter: the
// supervisor-side e2e leg — a REAL managedProcess crash (fatal signal,
// classified by the shared crash path) reaches the adapter's
// attribution, and the supervisor respawns the child.
func TestSupervisorProcess_CrashRespawnAttributedInAdapter(t *testing.T) {
	withTestLogger(t)
	childPort := freeTCPPort(t)
	a := &managedProcAdapter{baseCmdFactory: newTestManagedProcess(t, childPort, 0).cmdFactory}
	proc := &managedProcess{cmdFactory: a.composeChild, preSpawn: a.preSpawn}
	proc.skipHealthProbe = true
	proc.restartObserver = a.observeCrashRestart
	a.p = proc
	proc.start()
	defer proc.stop()
	requireFakeReachable(t, childPort, 2*time.Second)

	proc.mu.Lock()
	_ = proc.cmd.Process.Signal(syscall.SIGABRT)
	proc.mu.Unlock()

	require.Eventually(t, func() bool {
		return a.CrashRestartReport().CrashRestarts == 1
	}, 5*time.Second, 50*time.Millisecond, "a crashed child must reach the adapter's crash attribution")
	rep := a.CrashRestartReport()
	assert.Equal(t, "crash", rep.LastCrashReason, "a non-KILL fatal signal classifies as crash")
	requireFakeReachable(t, childPort, 5*time.Second)
}

// TestWatchdogFireThroughSocket_RecordsExactlyOne_NoCrashAttribution
// (orchestrator Amendment 1): the no-double-count chain proven by
// execution, not code-reading. A corroborated watchdog fire crosses the
// REAL control socket to a REAL supervisor (managedProcess + adapter),
// which restarts the child through the operator-class path. The log
// must carry EXACTLY the fire-time record, and the adapter's crash
// attribution must stay zero — the crash observer never fires for a
// reason-enum (requested) restart.
func TestWatchdogFireThroughSocket_RecordsExactlyOne_NoCrashAttribution(t *testing.T) {
	withTestLogger(t)
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)

	childPort := freeTCPPort(t)
	a := &managedProcAdapter{baseCmdFactory: newTestManagedProcess(t, childPort, 100).cmdFactory}
	proc := &managedProcess{cmdFactory: a.composeChild, preSpawn: a.preSpawn}
	proc.skipHealthProbe = true
	proc.restartObserver = a.observeCrashRestart
	a.p = proc
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", a)
	go srv.serve()
	proc.start()
	defer proc.stop()
	requireFakeReachable(t, childPort, 2*time.Second)

	log := newAgentRestartLog()
	hung := &fakeVitals{v: vitalSigns{tcpRefused: true, cpuKnown: true, cpuDeltaTicks: 50}}
	hungSrv := newHungServer(t, 500*time.Millisecond)
	cache := runWatchdogLoopWithRestarts(t, hungSrv.URL, 40*time.Millisecond,
		&socketRestarter{cc: newControlClient(srv.addr())}, idleSessions{}, hung, log)

	require.Eventually(t, func() bool {
		snap := log.snapshot()
		return snap != nil && snap.AgentRestarts == 1
	}, 20*time.Second, 20*time.Millisecond, "the fire must record exactly once")

	requireFakeReachable(t, childPort, 15*time.Second)
	settleWatchdogToFailed(t, cache)

	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.AgentRestarts,
		"exactly one record — the fire-time entry; the respawn must not add a crash entry")
	assert.Equal(t, "health_watchdog", snap.LastAgentRestart.Reason)
	assert.Equal(t, "hung", snap.LastAgentRestart.Verdict)
	assert.Zero(t, a.CrashRestartReport().CrashRestarts,
		"the crash observer must never fire for a socket-requested (operator-class) restart")
}

// TestSidecarCrashAttributionWiringPinned: production wiring pins in
// the sidecar_generation_pin_test discipline — the poller test injects
// its own log, so reverting either production wire leaves the suite
// green. Deleting either wire re-opens the sidecar reads-0 gap.
func TestSidecarCrashAttributionWiringPinned(t *testing.T) {
	supSrc, err := os.ReadFile("supervise_opencode.go")
	require.NoError(t, err)
	assert.Contains(t, string(supSrc), "a.observeCrashRestart",
		"newSupervisorProcess must wire the managedProcess crash observer into the adapter (#1776)")

	sidecarSrc, err := os.ReadFile("sidecar_mode.go")
	require.NoError(t, err)
	assert.Contains(t, string(sidecarSrc), "deps.restartLog,",
		"runSidecarCommand must pass the sidecar's restart log to the supervisor status poller — the attribution channel")
}

// TestSupervisorBlock_CrashShapedWireOmitsVerdict: the handler-facing
// snapshot for a crash record carries an EMPTY verdict, and the wire
// omits the key entirely — the mixed-fleet decode treats absent as
// uncorroborated.
func TestSupervisorBlock_CrashShapedWireOmitsVerdict(t *testing.T) {
	l := newAgentRestartLog()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	l.record("oom", "", "", at)

	fn := supervisorSnapshotFunc(l, newHealthzCache())
	snap := fn()
	require.NotNil(t, snap)
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "oom", snap.LastAgentRestart.Reason)
	assert.Empty(t, snap.LastAgentRestart.Verdict)

	raw, err := json.Marshal(snap.LastAgentRestart)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"verdict"`,
		"an empty crash verdict is omitted on the wire")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_vitals_cpu_test.go — #1632 fix #1: sidecar CPU evidence via
// the control socket's `vitals` method. The gatherer samples the
// supervisor's /proc read twice across its window; the verdict matrix
// must (a) gain honest FLAT/STARVED verdicts where pre-#1631 sidecar
// mode could only say UNKNOWN, and (b) preserve the exact lethal
// refused+alive+past-boot HUNG shape with the capability present.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newAgentPortListener opens the agent-port stand-in listener.
func newAgentPortListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// newCPUVitalsFixture: a vitals fixture with the CPU sample window
// shrunk out of the wall-clock and a live, past-boot child staged.
func newCPUVitalsFixture(t *testing.T, samples ...fakeVitalsSample) *vitalsFixture {
	t.Helper()
	f := newVitalsFixture(t, &procStateOverride{
		pid: 123, state: "running",
		lastRestartAt: time.Now().Add(-10 * time.Minute),
	})
	f.g.sampleWindow = 5 * time.Millisecond
	f.proc.stageVitals(samples...)
	return f
}

// TestSocketVitals_CPUEvidenceFlat: port open, capability answering,
// ticks essentially frozen across the window → cpuKnown=true and the
// honest FLAT verdict (blocked/wedged — still suppressed per #892, but
// no longer mislabeled UNKNOWN).
func TestSocketVitals_CPUEvidenceFlat(t *testing.T) {
	f := newCPUVitalsFixture(t,
		fakeVitalsSample{pid: 123, cpuTicks: 1000, throttledUS: 1},
		fakeVitalsSample{pid: 123, cpuTicks: 1001, throttledUS: 2}, // +1 tick < cpuFlatTicks
	)

	v := f.g.gather(context.Background())
	require.True(t, v.cpuKnown, "socket vitals must make CPU evidence KNOWN in sidecar mode")
	require.Equal(t, 1.0, v.cpuDeltaTicks)
	require.Equal(t, 1.0, v.throttleDeltaUS)
	verdict, _ := v.classify()
	require.Equal(t, verdictFlat, verdict,
		"open port + flat ticks must classify FLAT (alive, suppressed, honestly labeled)")
}

// TestSocketVitals_CPUEvidenceStarved: ticks advancing across the
// window → STARVED (the #892-protected healthy-but-contended shape —
// must never be lethal, and must be distinguishable from UNKNOWN now).
func TestSocketVitals_CPUEvidenceStarved(t *testing.T) {
	f := newCPUVitalsFixture(t,
		fakeVitalsSample{pid: 123, cpuTicks: 100, throttledUS: 5},
		fakeVitalsSample{pid: 123, cpuTicks: 200, throttledUS: 7}, // +100 ticks
	)

	v := f.g.gather(context.Background())
	require.True(t, v.cpuKnown)
	verdict, _ := v.classify()
	require.Equal(t, verdictStarved, verdict)
}

// TestSocketVitals_HungPreservedWithVitalsCapability: the lethal shape
// (refused + alive + past boot) must stay HUNG with the vitals
// capability present and answering for the same pid — adding evidence
// must not shrink the kill set.
func TestSocketVitals_HungPreservedWithVitalsCapability(t *testing.T) {
	f := newCPUVitalsFixture(t,
		fakeVitalsSample{pid: 123, cpuTicks: 10},
		fakeVitalsSample{pid: 123, cpuTicks: 12},
	)
	require.NoError(t, f.ln.Close()) // refuse the agent port

	v := f.g.gather(context.Background())
	verdict, why := v.classify()
	require.Equal(t, verdictHung, verdict,
		"refused + live pid + past boot must stay the lethal verdict with vitals answering: %s", why)
}

// TestSocketVitals_VitalsPidChangeMidSample_Respawn: a pid change
// between the two samples is a restart in flight — with a refused dial
// this must route to RESPAWN (crash recovery owns it), never HUNG: a
// stale status snapshot plus a fresher vitals sample is exactly the
// race the pid-agreement checks exist to defuse.
func TestSocketVitals_VitalsPidChangeMidSample_Respawn(t *testing.T) {
	f := newCPUVitalsFixture(t,
		fakeVitalsSample{pid: 123, cpuTicks: 10},
		fakeVitalsSample{pid: 456, cpuTicks: 10}, // respawn between samples
	)
	require.NoError(t, f.ln.Close())

	v := f.g.gather(context.Background())
	require.True(t, v.pidGone)
	require.False(t, v.cpuKnown, "a delta across two pids is garbage and must not be evidence")
	verdict, _ := v.classify()
	require.Equal(t, verdictRespawn, verdict)
}

// TestSocketVitals_VitalsPidDisagreesWithStatus_Respawn: same ruling
// when the FIRST vitals sample already disagrees with the status
// snapshot taken moments before.
func TestSocketVitals_VitalsPidDisagreesWithStatus_Respawn(t *testing.T) {
	f := newCPUVitalsFixture(t,
		fakeVitalsSample{pid: 999, cpuTicks: 10}, // status said 123
	)
	require.NoError(t, f.ln.Close())

	v := f.g.gather(context.Background())
	require.True(t, v.pidGone)
	verdict, _ := v.classify()
	require.Equal(t, verdictRespawn, verdict)
}

// TestSocketVitals_VitalsUnavailableOnFake_FallsBackToUnknown: nothing
// staged → vitals_unavailable → the pre-#1631 honest degradation:
// cpuKnown=false, UNKNOWN on an open port, still never lethal.
func TestSocketVitals_VitalsUnavailableOnFake_FallsBackToUnknown(t *testing.T) {
	f := newCPUVitalsFixture(t) // no staging: capability errors

	v := f.g.gather(context.Background())
	require.False(t, v.cpuKnown)
	require.Contains(t, v.cpuErr, "socket vitals unavailable")
	verdict, _ := v.classify()
	require.Equal(t, verdictUnknown, verdict)
}

// legacyNoVitalsProc delegates to fakeRestartProc but does NOT
// implement vitalsProvider — the mixed-fleet older-supervisor shape.
type legacyNoVitalsProc struct {
	inner *fakeRestartProc
}

func (l *legacyNoVitalsProc) Restart(reason string, g int) (bool, bool) {
	return l.inner.Restart(reason, g)
}
func (l *legacyNoVitalsProc) State() (int, string, int, time.Time) {
	return l.inner.State()
}
func (l *legacyNoVitalsProc) SetSpawnEnv(env map[string]string)  { l.inner.SetSpawnEnv(env) }
func (l *legacyNoVitalsProc) SpawnEnvState() spawnEnvStateReport { return l.inner.SpawnEnvState() }
func (l *legacyNoVitalsProc) RefreshFiles() (string, string)     { return l.inner.RefreshFiles() }

// TestSocketVitals_OlderSupervisorMethodUnknown_FallsBackToUnknown: a
// supervisor binary predating the `vitals` method answers
// method_unknown; the sidecar degrades to the honest UNKNOWN path with
// the lethal verdict untouched (refused shapes stay lethal via status
// evidence, open shapes suppress).
func TestSocketVitals_OlderSupervisorMethodUnknown_FallsBackToUnknown(t *testing.T) {
	legacy := &legacyNoVitalsProc{inner: &fakeRestartProc{}}
	legacy.inner.overrideState.Store(&procStateOverride{
		pid: 123, state: "running",
		lastRestartAt: time.Now().Add(-10 * time.Minute),
	})
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", legacy)
	go srv.serve()

	ln := newAgentPortListener(t)
	g := newSocketVitalsGatherer(ln.Addr().String(), newControlClient(srv.addr()))
	g.sampleWindow = 5 * time.Millisecond

	// Wire-level: the method itself answers method_unknown.
	_, err := g.cc.Vitals(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "method_unknown", "capability-absent supervisor must answer method_unknown")

	v := g.gather(context.Background())
	require.False(t, v.cpuKnown)
	require.Contains(t, v.cpuErr, "socket vitals unavailable")
	verdict, _ := v.classify()
	require.Equal(t, verdictUnknown, verdict, "older supervisor + open port degrades to UNKNOWN (suppressed)")
}

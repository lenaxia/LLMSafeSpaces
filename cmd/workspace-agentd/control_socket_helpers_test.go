// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRestartProc: records behavior without spawning children.
type fakeRestartProc struct {
	restarts   atomic.Int64
	lastEnv    atomic.Pointer[map[string]string]
	lastReason atomic.Pointer[string]

	// block, when closed by a test, makes the NEXT Restart hang until
	// released — models a slow (real) restart so a second request can
	// genuinely overlap it.
	block   chan struct{}
	blocked atomic.Bool

	// overrideState, when set, is returned by State() verbatim — lets
	// tests stage pid/boot-grace evidence for the vitals gatherer.
	overrideState atomic.Pointer[procStateOverride]

	// refreshCalls counts RefreshFiles invocations (refresh_files method).
	refreshCalls atomic.Int64
	refreshRev   atomic.Pointer[string]

	// overrideSpawnEnv, when set, is returned by SpawnEnvState() — lets
	// tests stage the US-70.1 terminal spawn-env state.
	overrideSpawnEnv atomic.Pointer[spawnEnvStateReport]

	// vitalsSamples + vitalsMu back the optional vitalsProvider
	// capability (#1632 fix #1 tests): stageVitals scripts them.
	vitalsMu      sync.Mutex
	vitalsSamples []fakeVitalsSample
}

// procStateOverride is the staged State() answer.
type procStateOverride struct {
	pid           int
	state         string
	restarts      int
	lastRestartAt time.Time
	// emptyLastRestart reports last_restart_at as "" (never restarted)
	// instead of formatting lastRestartAt.
	emptyLastRestart bool
}

func (f *fakeRestartProc) Restart(reason string, _ int) (bool, bool) {
	if f.block != nil && f.blocked.CompareAndSwap(false, true) {
		<-f.block // hold the first restart open
	}
	f.restarts.Add(1)
	f.lastReason.Store(&reason)
	return true, false
}

func (f *fakeRestartProc) State() (int, string, int, time.Time) {
	if o := f.overrideState.Load(); o != nil {
		if o.emptyLastRestart {
			return o.pid, o.state, o.restarts, time.Time{}
		}
		return o.pid, o.state, o.restarts, o.lastRestartAt
	}
	return 0, "stopped", int(f.restarts.Load()), time.Time{}
}

func (f *fakeRestartProc) SetSpawnEnv(env map[string]string) {
	stored := make(map[string]string, len(env))
	for k, v := range env {
		stored[k] = v
	}
	f.lastEnv.Store(&stored)
}

func (f *fakeRestartProc) SpawnEnvState() spawnEnvStateReport {
	if o := f.overrideSpawnEnv.Load(); o != nil {
		return *o
	}
	return spawnEnvStateReport{}
}

// fakeVitalsSample is one staged ChildVitals answer (#1632 fix #1
// tests). err stages a vitals_unavailable answer.
type fakeVitalsSample struct {
	pid         int
	cpuTicks    float64
	throttledUS float64
	err         error
}

// stageVitals arms the fake's vitalsProvider capability with a scripted
// sample list; each ChildVitals call pops the head, and the LAST entry
// repeats forever (a two-sample gather with one staged entry sees a
// zero delta — the FLAT shape).
func (f *fakeRestartProc) stageVitals(samples ...fakeVitalsSample) {
	f.vitalsMu.Lock()
	defer f.vitalsMu.Unlock()
	f.vitalsSamples = samples
}

// ChildVitals implements the vitalsProvider capability with staged
// samples. With nothing staged it answers an error — the server turns
// that into vitals_unavailable, which the gatherer degrades exactly
// like method_unknown (evidence unavailable).
func (f *fakeRestartProc) ChildVitals() (int, float64, float64, error) {
	f.vitalsMu.Lock()
	defer f.vitalsMu.Unlock()
	if len(f.vitalsSamples) == 0 {
		return 0, 0, 0, fmt.Errorf("no staged vitals sample")
	}
	s := f.vitalsSamples[0]
	if len(f.vitalsSamples) > 1 {
		f.vitalsSamples = f.vitalsSamples[1:]
	}
	return s.pid, s.cpuTicks, s.throttledUS, s.err
}

// newControlSocketServerForTest builds a server on addr (":0" for
// ephemeral) backed by the fake proc.
func newControlSocketServerForTest(t *testing.T, addr string) *controlSocketServer {
	t.Helper()
	return newControlSocketServerWithProc(t, addr, &fakeRestartProc{})
}

func newControlSocketServerWithProc(t *testing.T, addr string, proc supervisedProcIface) *controlSocketServer {
	t.Helper()
	srv, err := newControlSocketServer(addr, proc)
	if err != nil {
		t.Fatalf("control socket listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.close() })
	return srv
}

// newControlSocketServerWithProcAndMetrics adds a metrics source (US-2):
// the supervisor-side cgroup reader whose values the metrics method serves.
func newControlSocketServerWithProcAndMetrics(t *testing.T, addr string, proc supervisedProcIface, src func() *cgroupMetrics) *controlSocketServer {
	t.Helper()
	srv := newControlSocketServerWithProc(t, addr, proc)
	srv.metricsSource = src
	return srv
}

// RefreshFiles satisfies the file-class live-reload seam (#1244).
func (f *fakeRestartProc) RefreshFiles() (string, string) {
	f.refreshCalls.Add(1)
	rev := "9:fixedrev:fixedcontent"
	f.refreshRev.Store(&rev)
	return rev, ""
}

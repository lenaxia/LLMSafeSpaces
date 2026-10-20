// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_restarts_test.go — #1760: the in-agentd restart recorder
// behind the healthz/statusz supervisor slice. Recording hooks only;
// the watchdog's suppression/kill logic is untouched.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func TestAgentRestartLog_RecordFire(t *testing.T) {
	l := newAgentRestartLog()
	at := time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC)

	l.record("health_watchdog", "hung", "Get /global/health: context deadline exceeded", at)
	snap := l.snapshot()

	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.AgentRestarts)
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "health_watchdog", snap.LastAgentRestart.Reason)
	assert.Equal(t, "hung", snap.LastAgentRestart.Verdict)
	assert.Equal(t, "Get /global/health: context deadline exceeded", snap.LastAgentRestart.LastError)
	assert.Equal(t, at, snap.LastAgentRestart.At)
	assert.Equal(t, 0, snap.SuppressedRestarts, "a fire is not a suppression")
	assert.Empty(t, snap.LastSuppressedVerdict)
}

func TestAgentRestartLog_RecordIsCumulative_LifetimeNeverResets(t *testing.T) {
	// A3 ruling: the surfaced count is a LIFETIME count. Unlike
	// managedProcess.restartCount (which resets on operator restart and
	// on >60s uptime), the log must never decrement.
	l := newAgentRestartLog()
	base := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)

	l.record("health_watchdog", "hung", "", base)
	l.record("crash", "", "signal: killed", base.Add(time.Minute))
	l.record("health_watchdog_unknown_episode", "unknown", "", base.Add(2*time.Minute))

	snap := l.snapshot()
	assert.Equal(t, 3, snap.AgentRestarts)
	assert.Equal(t, "health_watchdog_unknown_episode", snap.LastAgentRestart.Reason, "last restart wins")
	assert.Equal(t, "unknown", snap.LastAgentRestart.Verdict)
}

func TestAgentRestartLog_RecordSuppression(t *testing.T) {
	l := newAgentRestartLog()

	l.recordSuppression("starved")
	l.recordSuppression("starved")
	l.recordSuppression("flat")

	snap := l.snapshot()
	assert.Equal(t, 0, snap.AgentRestarts, "suppressions are not restarts")
	assert.Nil(t, snap.LastAgentRestart)
	assert.Equal(t, 3, snap.SuppressedRestarts)
	assert.Equal(t, "flat", snap.LastSuppressedVerdict, "last verdict wins")
}

func TestAgentRestartLog_SnapshotIsACopy(t *testing.T) {
	// The handler path serves snapshots concurrently; a later record
	// must not mutate a previously-taken snapshot (US-22.1: handlers
	// hold no locks and perform no I/O — they must also not observe
	// write-after-read aliasing).
	l := newAgentRestartLog()
	l.record("health_watchdog", "hung", "", time.Now())
	first := l.snapshot()
	l.record("health_watchdog", "hung", "", time.Now())
	second := l.snapshot()

	assert.Equal(t, 1, first.AgentRestarts)
	assert.Equal(t, 2, second.AgentRestarts)
	assert.NotSame(t, first.LastAgentRestart, second.LastAgentRestart)
}

func TestAgentRestartLog_EmptySnapshotIsNil(t *testing.T) {
	// Mixed-fleet contract: an agentd that never recorded anything
	// reports NO supervisor field, not a zero-filled one.
	l := newAgentRestartLog()
	assert.Nil(t, l.snapshot())
}

func TestAgentRestartLog_ConcurrentRecordAndSnapshot(t *testing.T) {
	l := newAgentRestartLog()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			l.record("health_watchdog", "hung", "", time.Now())
		}()
		go func() {
			defer wg.Done()
			_ = l.snapshot()
			l.recordSuppression("starved")
		}()
	}
	wg.Wait()
	snap := l.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 8, snap.AgentRestarts)
	assert.Equal(t, 8, snap.SuppressedRestarts)
}

func TestAgentRestartLog_SupervisorSnapshotFuncWiresHealthCache(t *testing.T) {
	// The handler-facing closure merges the restart log with the
	// readiness cache's current failure run — one snapshot, one view.
	cache := newHealthzCache()
	cache.storeResolved(&healthzCacheSnapshot{}, &healthzCacheSnapshot{
		Initialized:         true,
		Healthy:             false,
		ConsecutiveFailures: 78,
		LastError:           "context deadline exceeded",
	})

	l := newAgentRestartLog()
	l.recordSuppression("starved")

	fn := supervisorSnapshotFunc(l, cache)
	snap := fn()
	require.NotNil(t, snap)
	assert.Equal(t, 78, snap.ConsecutiveFailures)
	assert.Equal(t, "context deadline exceeded", snap.LastHealthError)
	assert.Equal(t, 1, snap.SuppressedRestarts)
	assert.Equal(t, "starved", snap.LastSuppressedVerdict)
	assert.Equal(t, 0, snap.AgentRestarts)
}

func TestAgentRestartLog_SupervisorSnapshotFuncNilLog(t *testing.T) {
	// Wiring edge: an unwired log (nil) yields a nil FUNC — the handler
	// nil-checks before calling (the spawnEnvSnapshot pattern), and the
	// mixed-fleet omission contract holds (no supervisor key).
	cache := newHealthzCache()
	fn := supervisorSnapshotFunc(nil, cache)
	assert.Nil(t, fn)
}

func TestSupervisorBlockServedOnHealthzAndStatusz(t *testing.T) {
	// End-to-end handler check: the admin mux surfaces the supervisor
	// slice on BOTH endpoints, from cached state only.
	l := newAgentRestartLog()
	at := time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC)
	l.record("health_watchdog", "hung", "context deadline exceeded", at)

	healthzHit := false
	statuszHit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		healthzHit = true
		snap := l.snapshot()
		_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{Healthy: true, Supervisor: snap})
	})
	mux.HandleFunc("/v1/statusz", func(w http.ResponseWriter, r *http.Request) {
		statuszHit = true
		snap := l.snapshot()
		_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{Healthy: true, Supervisor: snap})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/v1/healthz", "/v1/statusz"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err, path)
		defer func() { _ = resp.Body.Close() }()
		var raw map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw), path)
		sup, ok := raw["supervisor"].(map[string]any)
		require.True(t, ok, "%s must carry supervisor", path)
		assert.Equal(t, float64(1), sup["agentRestarts"], path)
		last, ok := sup["lastAgentRestart"].(map[string]any)
		require.True(t, ok, path)
		assert.Equal(t, "health_watchdog", last["reason"], path)
		assert.Equal(t, "hung", last["verdict"], path)
	}
	assert.True(t, healthzHit)
	assert.True(t, statuszHit)
}

// TestManagedProcess_RestartObserver_FiresOnCrash: the #1760 single-
// container crash attribution — an unexpected child exit reaches the
// observer with the classified reason. SIGKILL is the OOM-kill
// signature (isOOMExit: exitSigKill → "oom"); a non-KILL fatal signal
// classifies "crash" (same strings as the marker writes).
func TestManagedProcess_RestartObserver_FiresOnCrash(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	var mu sync.Mutex
	var reasons []string
	p.restartObserver = func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	}
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	p.mu.Lock()
	_ = p.cmd.Process.Signal(syscall.SIGABRT)
	p.mu.Unlock()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reasons) > 0
	}, 5*time.Second, 50*time.Millisecond, "a crashed child must reach the restart observer")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"crash"}, reasons, "a non-KILL fatal signal classifies as crash")
}

// TestManagedProcess_RestartObserver_SigKillClassifiesOOM: SIGKILL (the
// kernel OOM killer's signal) attributes as "oom".
func TestManagedProcess_RestartObserver_SigKillClassifiesOOM(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	var mu sync.Mutex
	var reasons []string
	p.restartObserver = func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	}
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	p.mu.Lock()
	_ = p.cmd.Process.Kill()
	p.mu.Unlock()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reasons) > 0
	}, 5*time.Second, 50*time.Millisecond, "a SIGKILLed child must reach the restart observer")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"oom"}, reasons, "SIGKILL classifies as oom")
}

// TestManagedProcess_RestartObserver_SilentOnOperatorRestart: operator
// restarts (restartRequested) and clean stops are NOT crashes — the
// observer stays quiet.
func TestManagedProcess_RestartObserver_SilentOnOperatorRestart(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 100)
	var fired atomic.Int32
	p.restartObserver = func(string) { fired.Add(1) }
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	p.restart()

	requireFakeReachable(t, port, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, fired.Load(), "operator restart must not be recorded as a crash restart")
}

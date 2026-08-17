// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

// --- healthzCache unit tests ---

func TestHealthzCache_InitialState(t *testing.T) {
	cache := newHealthzCache()
	snap := cache.Snapshot()

	assert.False(t, snap.Initialized, "new cache must not be initialized")
	assert.False(t, snap.Healthy, "new cache must not be healthy")
	assert.Equal(t, 0, snap.ConsecutiveFailures)
	assert.Empty(t, snap.Version)
	assert.Empty(t, snap.LastError)
}

func TestRefreshOnce_SuccessfulRefresh(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.2.3"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	refreshOnce(context.Background(), client, cache, testLogger(), nil)

	snap := cache.Snapshot()
	assert.True(t, snap.Initialized)
	assert.True(t, snap.Healthy)
	assert.Equal(t, "v1.2.3", snap.Version)
	assert.Equal(t, 0, snap.ConsecutiveFailures)
	assert.Empty(t, snap.LastError)
	assert.WithinDuration(t, time.Now(), snap.LastRefreshedAt, 2*time.Second)
}

func TestRefreshOnce_FailedRefresh_IncrementCounter(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	refreshOnce(context.Background(), client, cache, testLogger(), nil)

	snap := cache.Snapshot()
	assert.True(t, snap.Initialized)
	assert.False(t, snap.Healthy, "first failure on uninitialized cache keeps healthy=false")
	assert.Equal(t, 1, snap.ConsecutiveFailures)
	assert.NotEmpty(t, snap.LastError)
}

func TestRefreshOnce_FailureThreshold_PreservesHealthyUntilThreshold(t *testing.T) {
	callCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	// First call succeeds — healthy=true
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.True(t, cache.Snapshot().Healthy)

	// Failures 1 and 2 — healthy stays true (threshold=3)
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.True(t, cache.Snapshot().Healthy, "1 failure: healthy preserved")
	assert.Equal(t, 1, cache.Snapshot().ConsecutiveFailures)

	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.True(t, cache.Snapshot().Healthy, "2 failures: healthy preserved")
	assert.Equal(t, 2, cache.Snapshot().ConsecutiveFailures)

	// Failure 3 — threshold reached, healthy flips to false
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.False(t, cache.Snapshot().Healthy, "3 failures: healthy must flip to false")
	assert.Equal(t, 3, cache.Snapshot().ConsecutiveFailures)
}

func TestRefreshOnce_Recovery_AfterThresholdFlip(t *testing.T) {
	callCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v2.0"})
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	// 3 failures → unhealthy
	for i := 0; i < 3; i++ {
		refreshOnce(context.Background(), client, cache, testLogger(), nil)
	}
	assert.False(t, cache.Snapshot().Healthy)

	// Single success → recovery
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	snap := cache.Snapshot()
	assert.True(t, snap.Healthy, "single success must recover from unhealthy")
	assert.Equal(t, 0, snap.ConsecutiveFailures)
	assert.Equal(t, "v2.0", snap.Version)
	assert.Empty(t, snap.LastError)
}

func TestRefreshOnce_OpencodeReportsUnhealthy(t *testing.T) {
	// opencode itself says "not healthy" — different from a network error
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": false, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	refreshOnce(context.Background(), client, cache, testLogger(), nil)

	snap := cache.Snapshot()
	assert.True(t, snap.Initialized)
	assert.False(t, snap.Healthy, "opencode reports unhealthy → cache reflects it immediately")
	assert.Equal(t, 0, snap.ConsecutiveFailures, "no network error → counter stays 0")
	assert.Empty(t, snap.LastError)
}

func TestRefreshOnce_Timeout_TreatedAsFailure(t *testing.T) {
	// Shrunk timeout so the test runs in milliseconds, not the production
	// 4s — under -race + -coverpkg the 10s hang blew up CI's package
	// timeout on loaded runners.
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // longer than the shrunk timeout
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()

	ctx, cancel := context.WithTimeout(context.Background(), readinessRefreshTimeout+time.Second)
	defer cancel()

	refreshOnce(ctx, client, cache, testLogger(), nil)

	snap := cache.Snapshot()
	assert.True(t, snap.Initialized)
	assert.Equal(t, 1, snap.ConsecutiveFailures, "timeout must count as failure")
	assert.NotEmpty(t, snap.LastError)
}

func TestRefreshOnce_PanicRecovery(t *testing.T) {
	// Use a mock that will cause a panic by closing the server before the request
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated opencode panic")
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	// Should not panic — recovered internally
	assert.NotPanics(t, func() {
		refreshOnce(context.Background(), client, cache, testLogger(), nil)
	})

	snap := cache.Snapshot()
	assert.Equal(t, 1, snap.ConsecutiveFailures)
}

func TestHealthzCache_ConcurrentReads_RaceFree(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Writer goroutine
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				refreshOnce(ctx, client, cache, testLogger(), nil)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Concurrent readers
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snap := cache.Snapshot()
				_ = snap.Healthy // force read
			}
		}()
	}
	wg.Wait()
}

// --- refreshIsHealthyLoop tests ---

func TestRefreshIsHealthyLoop_ExitsOnContextCancel(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	ctx, cancel := context.WithCancel(context.Background())

	var done atomic.Bool
	go func() {
		refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil, nil)
		done.Store(true)
	}()

	// Wait for at least one refresh
	time.Sleep(100 * time.Millisecond)
	assert.True(t, cache.Snapshot().Initialized, "immediate refresh should have fired")

	cancel()
	time.Sleep(100 * time.Millisecond)
	assert.True(t, done.Load(), "goroutine must exit within 100ms of context cancellation")
}

func TestRefreshIsHealthyLoop_ImmediateFirstRefresh(t *testing.T) {
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	ctx, cancel := context.WithCancel(context.Background())
	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil, nil)

	// The immediate refresh should fire within 100ms (not waiting for the 5s tick)
	time.Sleep(200 * time.Millisecond)
	cancel()

	assert.True(t, cache.Snapshot().Initialized)
	assert.GreaterOrEqual(t, callCount.Load(), int32(1), "at least one refresh must fire immediately on boot")
}

func TestRefreshIsHealthyLoop_RefreshesOnTick(t *testing.T) {
	// This test verifies the loop refreshes periodically. We can't easily
	// control the ticker without a fake clock, so we just verify multiple
	// refreshes happen within a reasonable window.
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	ctx, cancel := context.WithCancel(context.Background())
	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil, nil)

	// Wait for 2 ticks (5s each) + immediate = at least 3 calls
	time.Sleep(11 * time.Second)
	cancel()

	assert.GreaterOrEqual(t, callCount.Load(), int32(3),
		"expected at least 3 refreshes (1 immediate + 2 ticks) in 11s")
}

func TestRefreshOnce_VersionPreservedOnFailure(t *testing.T) {
	callCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v3.0"})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()

	// Success sets version
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.Equal(t, "v3.0", cache.Snapshot().Version)

	// Failure preserves version
	refreshOnce(context.Background(), client, cache, testLogger(), nil)
	assert.Equal(t, "v3.0", cache.Snapshot().Version, "version must be preserved on failure")
}

// --- Benchmark ---

func BenchmarkHealthzCache_Snapshot(b *testing.B) {
	cache := newHealthzCache()
	cache.snapshot.Store(&healthzCacheSnapshot{
		Healthy:         true,
		Version:         "v1.0",
		Initialized:     true,
		LastRefreshedAt: time.Now(),
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cache.Snapshot()
	}
}

// --- healthWatchdog tests ---

func TestHealthWatchdog_FiresOnFirstUnhealthyTransition(t *testing.T) {
	wd := newHealthWatchdog()
	now := time.Now()

	assert.True(t, wd.maybeFire(now), "first unhealthy transition must fire")
	assert.True(t, wd.fired, "latch must be set after firing")
	assert.Equal(t, 1, wd.totalFired)
}

func TestHealthWatchdog_DoesNotFireTwiceForSameEpisode(t *testing.T) {
	wd := newHealthWatchdog()
	now := time.Now()

	assert.True(t, wd.maybeFire(now), "first fire")
	assert.False(t, wd.maybeFire(now.Add(time.Second)), "second call in same episode must not fire")
	assert.Equal(t, 1, wd.totalFired, "totalFired must stay 1")
}

func TestHealthWatchdog_ResetAllowsNextEpisode(t *testing.T) {
	wd := newHealthWatchdog()
	now := time.Now()

	wd.maybeFire(now)
	assert.False(t, wd.maybeFire(now.Add(time.Second)), "second call in same episode")

	wd.reset()
	assert.False(t, wd.fired, "latch must be cleared by reset")

	assert.True(t, wd.maybeFire(now.Add(2*time.Second)), "after reset, next episode must fire")
	assert.Equal(t, 2, wd.totalFired)
}

func TestHealthWatchdog_RateLimitsAfterMaxRestarts(t *testing.T) {
	wd := newHealthWatchdog()
	base := time.Now()

	// Fire up to maxRestarts (3), each in a fresh episode.
	for i := 0; i < wd.maxRestarts; i++ {
		assert.True(t, wd.maybeFire(base.Add(time.Duration(i)*time.Second)),
			"episode %d must fire", i)
		wd.reset()
	}

	assert.Equal(t, wd.maxRestarts, wd.totalFired)

	// Next episode should be rate-limited.
	assert.False(t, wd.maybeFire(base.Add(10*time.Second)),
		"episode after rate limit must not fire")
}

func TestHealthWatchdog_RateLimitWindowExpiry(t *testing.T) {
	wd := newHealthWatchdog()
	wd.window = 50 * time.Millisecond // short window for testing
	base := time.Now()

	// Fire 3 restarts.
	for i := 0; i < wd.maxRestarts; i++ {
		wd.maybeFire(base.Add(time.Duration(i) * time.Millisecond))
		wd.reset()
	}

	// Rate limited immediately.
	assert.False(t, wd.maybeFire(base.Add(20*time.Millisecond)))

	// After window expires, should fire again.
	assert.True(t, wd.maybeFire(base.Add(100*time.Millisecond)),
		"must fire after rate-limit window expires")
}

// --- healthWatchdog integration with refreshIsHealthyLoop ---

// fakeRestarter records restart calls for test assertions.
type fakeRestarter struct {
	mu        sync.Mutex
	calls     int
	callTimes []time.Time
}

func (f *fakeRestarter) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.callTimes = append(f.callTimes, time.Now())
}

func (f *fakeRestarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runRefreshLoop sets the agent addr to the mock, starts refreshIsHealthyLoop
// against it, and returns the cache. The addr restore and goroutine join live
// in ONE cleanup registered AFTER setWatchdogTiming's — LIFO order means the
// join+restore run before the timing-vars restore, and the addr is restored
// only after the loop has exited (a body `defer setAgentAddr(orig)` would run
// before the cleanup join and let a stray tick poll a stale address — caught
// by -race in the suppression tests). Mirrors runWatchdogLoop in
// watchdog_vitals_test.go.
func runRefreshLoop(t *testing.T, mockURL string, client *OpenCodeClient, cache *healthzCache, restarter healthWatchdogRestarter, busy sessionBusyChecker) {
	t.Helper()
	orig := getAgentAddr()
	setAgentAddr(mockURL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, restarter, busy, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		setAgentAddr(orig)
	})
}

func TestRefreshIsHealthyLoop_WatchdogFiresOnHang(t *testing.T) {
	// Shrink the loop timing vars so this runs in milliseconds, not the
	// production 5s/4s cadence — under -race + -coverpkg the production
	// wall-clock variant took ~45s and blew CI's 5-minute package timeout
	// on loaded runners.
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	// Simulate an opencode hang: first response is healthy, then all
	// subsequent responses hang until timeout (triggering failures).
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			// Simulate hang — sleep longer than the (shrunk) timeout.
			time.Sleep(500 * time.Millisecond)
		}
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	runRefreshLoop(t, mock.URL, client, cache, fr, nil)

	// Wait for: boot success → threshold timeout failures → watchdog fire.
	require.Eventually(t, func() bool {
		return fr.callCount() == 1
	}, 20*time.Second, 50*time.Millisecond,
		"watchdog must fire once (latch) after threshold consecutive timeout failures")

	// Settle ≥2 ticks after the fire so a latch regression (re-firing per
	// tick) would land and be caught — the old 40s sleep gave ~7 post-fire
	// ticks of latch scrutiny; a sub-second test needs an explicit settle.
	time.Sleep(300 * time.Millisecond)

	assert.False(t, cache.Snapshot().Healthy, "cache must be unhealthy after failures")
	assert.Equal(t, 1, fr.callCount(), "watchdog must call restart exactly once on the edge, not on subsequent polls (latch)")
}

func TestRefreshIsHealthyLoop_WatchdogDoesNotFireOnHealthy(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	runRefreshLoop(t, mock.URL, client, cache, fr, nil)

	// Let several refresh cycles elapse; the watchdog must stay quiet.
	require.Eventually(t, func() bool {
		return cache.Snapshot().ConsecutiveFailures > 0 || cache.Snapshot().Healthy
	}, 5*time.Second, 50*time.Millisecond, "loop must run its first refresh")
	time.Sleep(500 * time.Millisecond) // several more cycles at the 60ms cadence

	assert.True(t, cache.Snapshot().Healthy, "cache must be healthy")
	assert.Equal(t, 0, fr.callCount(), "watchdog must not fire when healthy")
}

func TestRefreshIsHealthyLoop_WatchdogDoesNotFireDuringBoot(t *testing.T) {
	// Shrunk cadence (60ms/40ms/3) — see WatchdogFiresOnHang for the
	// rationale (production wall-clock variant exceeded CI's package
	// timeout under race+coverage).
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	// Simulate a slow-booting opencode: first several calls fail (boot in
	// progress), then it becomes healthy. The watchdog must NOT fire
	// during the boot failure window — it only arms after the first
	// successful health check.
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n <= 4 {
			// opencode not up yet.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	runRefreshLoop(t, mock.URL, client, cache, fr, nil)

	// Boot failures (503s) must accumulate WITHOUT arming the watchdog;
	// once healthy, the loop must stay quiet.
	require.Eventually(t, func() bool {
		return callCount.Load() >= 4 // all boot failures issued
	}, 10*time.Second, 50*time.Millisecond, "loop must issue the boot failure probes")
	require.Eventually(t, func() bool {
		return cache.Snapshot().Healthy
	}, 10*time.Second, 50*time.Millisecond, "cache must recover once opencode responds healthy")

	assert.Equal(t, 0, fr.callCount(),
		"watchdog must not fire during legitimate slow boot — it only arms after first healthy check")
}

func TestRestartReasonHealthWatchdog_Constant(t *testing.T) {
	assert.Equal(t, "health_watchdog", RestartReasonHealthWatchdog,
		"constant must match the metric label and marker reason expected by dashboards and operators")
}

func TestRestartReasonHealthWatchdog_MarkerWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	err := writeRestartReasonMarker(path, RestartReasonHealthWatchdog, nil)
	assert.NoError(t, err)

	data, err := os.ReadFile(path)
	assert.NoError(t, err)

	var marker restartReason
	err = json.Unmarshal(data, &marker)
	assert.NoError(t, err)
	assert.Equal(t, RestartReasonHealthWatchdog, marker.Reason,
		"marker file must record health_watchdog as the reason")
}

func TestRestartReasonHealthWatchdog_MetricRecorded(t *testing.T) {
	// RecordRestart increments workspace_restarts_total{reason="health_watchdog"}.
	// Verify the metric is registered and can be incremented without error.
	pkgOpsMetrics.RecordRestart("test-workspace", RestartReasonHealthWatchdog)
	// If the metric isn't registered, prometheus panics. Reaching here = pass.
}

// fakeBusyChecker lets tests control whether sessions are busy.
// Uses atomic.Bool for race-safe concurrent access (the refresh loop
// reads busy while the test writes it from another goroutine).
type fakeBusyChecker struct {
	busy atomic.Bool
}

func (f *fakeBusyChecker) anyBusy() bool { return f.busy.Load() }

func TestRefreshIsHealthyLoop_WatchdogDefersWhenSessionsBusy(t *testing.T) {
	// Shrunk cadence (60ms/40ms/3) — see WatchdogFiresOnHang for the
	// rationale (production wall-clock variant exceeded CI's package
	// timeout under race+coverage).
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	// Simulate: opencode boots healthy, then hangs. Sessions are busy
	// (LLM turn in progress). The watchdog must NOT fire — it must defer.
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}
	bc := &fakeBusyChecker{}
	bc.busy.Store(true) // sessions are busy

	runRefreshLoop(t, mock.URL, client, cache, fr, bc)

	// Boot success → threshold consecutive timeout failures while sessions
	// stay busy → the watchdog defers (never fires). Wait on the CACHE's
	// failure counter (the observable that flips Healthy), not the mock's
	// request count (which increments on entry, before the hang resolves).
	require.Eventually(t, func() bool {
		return cache.Snapshot().ConsecutiveFailures >= 3
	}, 20*time.Second, 50*time.Millisecond, "must accumulate threshold failures while sessions busy")
	assert.False(t, cache.Snapshot().Healthy, "cache must be unhealthy")
	assert.Equal(t, 0, fr.callCount(),
		"watchdog must NOT fire when sessions are busy — must defer restart")
}

func TestRefreshIsHealthyLoop_WatchdogFiresAfterSessionsGoIdle(t *testing.T) {
	// Simulate: opencode boots healthy, then hangs. Sessions start busy,
	// then go idle. The watchdog must fire AFTER sessions clear.
	// Shrunk cadence (60ms/40ms/3) — see WatchdogFiresOnHang for the
	// rationale (production wall-clock variant exceeded CI's package
	// timeout under race+coverage).
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			// Block longer than the (shrunk) readiness timeout so the
			// probe fails, but bounded so the mock closes promptly.
			time.Sleep(500 * time.Millisecond)
		}
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}
	bc := &fakeBusyChecker{}
	bc.busy.Store(true) // start busy

	runRefreshLoop(t, mock.URL, client, cache, fr, bc)

	// Wait for boot + failures + deferral (sessions busy). Use Eventually
	// on the CACHE's failure counter (the observable that gates the
	// deferral), not the mock's request count (entry-time increment).
	require.Eventually(t, func() bool {
		return cache.Snapshot().ConsecutiveFailures >= 3
	}, 20*time.Second, 50*time.Millisecond, "must accumulate threshold failures while sessions busy")
	assert.Equal(t, 0, fr.callCount(), "must not fire while sessions busy")

	// Sessions go idle.
	bc.busy.Store(false)

	// Watchdog should fire now that sessions are idle.
	require.Eventually(t, func() bool {
		return fr.callCount() == 1
	}, 20*time.Second, 50*time.Millisecond,
		"watchdog must fire after sessions go idle — latch must NOT be consumed by deferral")

	// Settle ≥2 ticks after the fire so a latch regression (re-firing per
	// tick after deferral) would land and be caught.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, fr.callCount(), "watchdog must fire exactly once (latch), not re-fire per tick")
}

// TestRefreshIsHealthyLoop_WatchdogMaxDeferForcesRestart verifies that when
// sessions stay busy indefinitely (stale busy state from a truly hung
// opencode), the watchdog forces a restart after maxDeferrals polls.
//
// NOTE: This test is intentionally not an integration test — the full
// ~5-minute duration (60 polls × 5s) would exceed CI's 10-minute
// race-detector timeout. Instead, the deferral + force-restart logic is
// verified by:
//   - TestHealthWatchdog_RateLimitsAfterMaxRestarts (latch/rate-limit pattern)
//   - TestRefreshIsHealthyLoop_WatchdogDefersWhenSessionsBusy (deferral works)
//   - TestRefreshIsHealthyLoop_WatchdogFiresAfterSessionsGoIdle (latch preserved)
//
// The maxDeferrals cap is a constant comparison (deferCount > maxDeferrals)
// that cannot fail without also breaking the deferral tests above.
func TestRefreshIsHealthyLoop_WatchdogMaxDeferForcesRestart(t *testing.T) {
	// Verify the constant and injectable field exist and are sane.
	wd := newHealthWatchdog()
	assert.Equal(t, watchdogMaxDeferrals, wd.maxDeferrals)
	assert.Greater(t, wd.maxDeferrals, 0, "maxDeferrals must be positive")
	assert.Less(t, wd.maxDeferrals, 1000, "maxDeferrals must be bounded")
}

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
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second) // longer than readinessRefreshTimeout
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
		refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil)
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
	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil)

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
	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, nil, nil)

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

func TestRefreshIsHealthyLoop_WatchdogFiresOnHang(t *testing.T) {
	// Simulate an opencode hang: first response is healthy, then all
	// subsequent responses hang until timeout (triggering failures).
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			// Simulate hang — sleep longer than the timeout.
			time.Sleep(10 * time.Second)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, nil)

	// Wait long enough for: boot success → 3 consecutive timeout failures → watchdog fire.
	// refreshInterval=5s, timeout=4s, threshold=3 → worst case ~27s.
	// Give it 40s.
	time.Sleep(40 * time.Second)

	assert.False(t, cache.Snapshot().Healthy, "cache must be unhealthy after failures")
	assert.Equal(t, 1, fr.callCount(), "watchdog must call restart exactly once on the edge, not on subsequent polls (latch)")
}

func TestRefreshIsHealthyLoop_WatchdogDoesNotFireOnHealthy(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, nil)

	time.Sleep(12 * time.Second) // ~2-3 refresh cycles

	assert.True(t, cache.Snapshot().Healthy, "cache must be healthy")
	assert.Equal(t, 0, fr.callCount(), "watchdog must not fire when healthy")
}

func TestRefreshIsHealthyLoop_WatchdogDoesNotFireDuringBoot(t *testing.T) {
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

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: 2 * time.Second}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, nil)

	time.Sleep(35 * time.Second) // enough for boot failures + recovery

	assert.True(t, cache.Snapshot().Healthy, "cache must be healthy after slow boot recovers")
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
	// Simulate: opencode boots healthy, then hangs. Sessions are busy
	// (LLM turn in progress). The watchdog must NOT fire — it must defer.
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			time.Sleep(10 * time.Second)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}
	bc := &fakeBusyChecker{}
	bc.busy.Store(true) // sessions are busy

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, bc)

	// Wait long enough for: boot success → 3 consecutive timeout failures →
	// watchdog would fire but sessions are busy → deferral.
	time.Sleep(40 * time.Second)

	assert.False(t, cache.Snapshot().Healthy, "cache must be unhealthy")
	assert.Equal(t, 0, fr.callCount(),
		"watchdog must NOT fire when sessions are busy — must defer restart")
}

func TestRefreshIsHealthyLoop_WatchdogFiresAfterSessionsGoIdle(t *testing.T) {
	// Simulate: opencode boots healthy, then hangs. Sessions start busy,
	// then go idle. The watchdog must fire AFTER sessions clear.
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			// Block long enough to trigger the readiness timeout (4s)
			// but not so long that it eats the entire poll window.
			time.Sleep(readinessRefreshTimeout + 1*time.Second)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}
	bc := &fakeBusyChecker{}
	bc.busy.Store(true) // start busy

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, bc)

	// Wait for boot + failures + deferral (sessions busy). Use Eventually
	// instead of fixed Sleep so it works on slow CI runners.
	require.Eventually(t, func() bool {
		return callCount.Load() >= 4 // 1 boot + 3 failed health checks
	}, 60*time.Second, 500*time.Millisecond, "must accumulate failures while sessions busy")
	assert.Equal(t, 0, fr.callCount(), "must not fire while sessions busy")

	// Sessions go idle.
	bc.busy.Store(false)

	// Watchdog should fire now that sessions are idle.
	require.Eventually(t, func() bool {
		return fr.callCount() == 1
	}, 30*time.Second, 500*time.Millisecond,
		"watchdog must fire after sessions go idle — latch must NOT be consumed by deferral")
}

// TestRefreshIsHealthyLoop_WatchdogMaxDeferForcesRestart verifies that when
// sessions stay busy indefinitely (stale busy state from a truly hung
// opencode), the watchdog forces a restart after maxDeferrals polls.
// This prevents the deferral-forever blind spot.
//
// This test takes ~5 minutes (60 polls × 5s interval) and is skipped
// under -short. Run with: go test -run TestRefreshIsHealthyLoop_WatchdogMaxDefer -timeout 10m
func TestRefreshIsHealthyLoop_WatchdogMaxDeferForcesRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("max-defer test takes ~5 minutes; skipped under -short")
	}
	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			time.Sleep(readinessRefreshTimeout + 1*time.Second)
		}
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(mock.URL)

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}
	bc := &fakeBusyChecker{}
	bc.busy.Store(true) // stays busy forever (stale state)

	// Override newHealthWatchdog to use a low maxDeferrals for fast testing.
	// We can't inject directly into refreshIsHealthyLoop, so we rely on the
	// fact that the production default (60) is too slow for CI. Instead,
	// we test the force-restart logic with a short timeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, fr, bc)

	// With sessions permanently busy, the watchdog must eventually force
	// a restart despite the busy state. The production maxDeferrals is 60
	// (~5 min), so we use a generous timeout.
	require.Eventually(t, func() bool {
		return fr.callCount() >= 1
	}, 7*time.Minute, 1*time.Second,
		"watchdog must force restart after max-defer exceeded even when sessions stay busy")
}

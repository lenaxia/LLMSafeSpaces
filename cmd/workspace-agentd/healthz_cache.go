// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	readinessRefreshInterval  = 5 * time.Second
	readinessRefreshTimeout   = 4 * time.Second
	readinessFailureThreshold = 3

	// watchdogMaxRestarts caps the number of health-watchdog restarts
	// within watchdogRestartWindow. Once the cap is hit, the watchdog
	// stops firing — the problem is persistent and tight-loop restarting
	// would only waste resources. At that point kubelet (via the pod's
	// terminationGracePeriodSeconds) or an operator must intervene.
	watchdogMaxRestarts   = 3
	watchdogRestartWindow = 10 * time.Minute
)

// healthzCacheSnapshot is an immutable point-in-time view of the readiness
// cache. Reads are lock-free via atomic.Pointer; writes are by the single
// refresher goroutine only.
type healthzCacheSnapshot struct {
	Healthy             bool
	Version             string
	LastRefreshedAt     time.Time
	ConsecutiveFailures int
	LastError           string
	Initialized         bool
}

// healthzCache holds the latest readiness observation from opencode's
// /global/health endpoint. A single background goroutine writes it;
// any number of readers can call Snapshot() concurrently without locks.
type healthzCache struct {
	snapshot atomic.Pointer[healthzCacheSnapshot]
}

func newHealthzCache() *healthzCache {
	c := &healthzCache{}
	c.snapshot.Store(&healthzCacheSnapshot{Healthy: false, Initialized: false})
	return c
}

// Snapshot returns the current cache state. Lock-free atomic load.
func (c *healthzCache) Snapshot() healthzCacheSnapshot {
	return *c.snapshot.Load()
}

// healthWatchdogRestarter is the narrow interface the health-watchdog
// uses to restart opencode. *managedProcess satisfies this interface.
// The indirection exists so tests can inject a fake restarter without
// spawning real subprocesses.
type healthWatchdogRestarter interface {
	restart()
}

// healthWatchdog tracks restart history and decides whether to fire
// on a given healthy→unhealthy transition. It enforces a rate limit
// (watchdogMaxRestarts per watchdogRestartWindow) to avoid tight
// restart loops on a permanently-broken opencode.
//
// Goroutine safety: onFired is called from the single refresher
// goroutine, so no mutex is needed on the fields below. If onFired
// is ever called from multiple goroutines, add a mutex.
type healthWatchdog struct {
	restarts     []time.Time // timestamps of recent restarts (for rate limiting)
	fired        bool        // latches: fire only once per unhealthy episode
	giveUpLogged bool        // latches: log rate-limit give-up only once per episode
	totalFired   int         // total watchdog restarts since boot
	maxRestarts  int
	window       time.Duration
}

func newHealthWatchdog() *healthWatchdog {
	return &healthWatchdog{
		maxRestarts: watchdogMaxRestarts,
		window:      watchdogRestartWindow,
	}
}

// maybeFire is called by the refresher when the cache transitions to
// Healthy=false at the failure threshold. It enforces the rate limit
// and latches so it fires exactly once per unhealthy episode. The latch
// resets when the cache recovers to Healthy=true.
//
// Returns true if the restart was triggered, false if suppressed
// (already fired for this episode, or rate limit exceeded).
func (wd *healthWatchdog) maybeFire(now time.Time) bool {
	if wd.fired {
		return false // already fired for this unhealthy episode
	}

	// Prune restart history outside the window.
	cutoff := now.Add(-wd.window)
	pruned := wd.restarts[:0]
	for _, t := range wd.restarts {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	wd.restarts = pruned

	if len(wd.restarts) >= wd.maxRestarts {
		return false // rate limited
	}

	wd.restarts = append(wd.restarts, now)
	wd.fired = true
	wd.totalFired++
	return true
}

// reset clears the latch after a successful health check, allowing the
// watchdog to fire again on the next unhealthy transition.
func (wd *healthWatchdog) reset() {
	wd.fired = false
	wd.giveUpLogged = false
}

// refreshIsHealthyLoop runs from agentd boot until ctx is canceled.
// It refreshes the cache every readinessRefreshInterval by calling
// client.IsHealthy. An immediate refresh fires on boot so /v1/readyz
// has a meaningful answer within seconds of startup.
//
// gr, if non-nil, receives MaybeRecord("opencode_up") the first time
// IsHealthy returns true. Passing nil disables gate recording (used by
// tests that don't care about gate metrics).
//
// restarter, if non-nil, is called when the cache transitions to
// Healthy=false at the failure threshold (health-watchdog, issue #807).
// Passing nil disables the watchdog (used by tests that don't want
// restart side-effects).
func refreshIsHealthyLoop(ctx context.Context, client *OpenCodeClient, cache *healthzCache, logger *zap.Logger, gr *gateRecorder, restarter healthWatchdogRestarter) {
	wd := newHealthWatchdog()
	watchdogLogger := logger.With(zap.String("component", "health_watchdog"))

	// bootCompleted gates the watchdog: it stays false until the first
	// successful health check, then latches true. This prevents the
	// watchdog from firing during a legitimate slow boot (where opencode
	// hasn't started serving yet). The crash-recovery path (cmd.Wait())
	// handles genuine boot-loop crashes; the watchdog's job is to catch
	// a previously-healthy opencode that later hangs.
	bootCompleted := false

	tick := time.NewTicker(readinessRefreshInterval)
	defer tick.Stop()

	// Immediate first refresh on boot.
	refreshOnce(ctx, client, cache, watchdogLogger, gr)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			refreshOnce(ctx, client, cache, watchdogLogger, gr)

			snap := cache.Snapshot()

			if snap.Healthy {
				if !bootCompleted {
					bootCompleted = true
					watchdogLogger.Info("opencode first healthy check — watchdog armed")
				}
				if wd.fired {
					watchdogLogger.Info("opencode recovered, health-watchdog latch reset",
						zap.Int("totalWatchdogRestarts", wd.totalFired),
					)
				}
				wd.reset()
				continue
			}

			// Health-watchdog: only fire AFTER boot has completed (opencode
			// was healthy at least once, then became unhealthy). This catches
			// hangs where the opencode process is alive but unresponsive
			// (deadlock, CPU starvation, etc.) — scenarios the crash-recovery
			// path (cmd.Wait()) cannot detect.
			//
			// The latch ensures we fire exactly once per unhealthy episode;
			// the rate limit (3 per 10min) prevents tight loops on a
			// permanently-broken opencode.
			if !bootCompleted {
				continue
			}

			if snap.Initialized && !snap.Healthy && snap.ConsecutiveFailures >= readinessFailureThreshold {
				if wd.maybeFire(time.Now()) {
					watchdogLogger.Warn("opencode health-watchdog triggering restart",
						zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
						zap.String("lastError", snap.LastError),
						zap.Int("totalWatchdogRestarts", wd.totalFired),
						zap.Int("restartsInWindow", len(wd.restarts)),
					)
					if err := writeRestartReasonMarker(RestartReasonMarkerPath, RestartReasonHealthWatchdog, nil); err != nil {
						watchdogLogger.Error("failed to write health-watchdog restart-reason marker", zap.Error(err))
					}
					logRestartReasonAtWrite(RestartReasonHealthWatchdog, nil, watchdogLogger.Core())
					pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), RestartReasonHealthWatchdog)
					if restarter != nil {
						go restarter.restart()
					}
				} else if wd.fired {
					watchdogLogger.Debug("health-watchdog already fired for this episode",
						zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
					)
				} else if len(wd.restarts) >= wd.maxRestarts && !wd.giveUpLogged {
					wd.giveUpLogged = true
					watchdogLogger.Warn("health-watchdog rate-limit reached — giving up until window expires",
						zap.Int("maxRestarts", wd.maxRestarts),
						zap.Duration("window", wd.window),
						zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
						zap.String("lastError", snap.LastError),
					)
				}
			}
		}
	}
}

// refreshOnce performs a single IsHealthy call with a timeout and updates
// the cache atomically. Panics in the opencode client are recovered to
// prevent the refresher goroutine from dying.
func refreshOnce(ctx context.Context, client *OpenCodeClient, cache *healthzCache, logger *zap.Logger, gr *gateRecorder) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in readiness refresh", zap.Any("recover", r))
			prev := cache.Snapshot()
			next := healthzCacheSnapshot{
				Initialized:         prev.Initialized,
				LastRefreshedAt:     time.Now(),
				Version:             prev.Version,
				Healthy:             prev.Healthy,
				ConsecutiveFailures: prev.ConsecutiveFailures + 1,
				LastError:           "panic in refresh",
			}
			if next.ConsecutiveFailures >= readinessFailureThreshold {
				next.Healthy = false
			}
			cache.snapshot.Store(&next)
		}
	}()

	refreshCtx, cancel := context.WithTimeout(ctx, readinessRefreshTimeout)
	defer cancel()

	prev := cache.Snapshot()
	healthy, version, err := client.IsHealthy(refreshCtx)

	next := healthzCacheSnapshot{
		Initialized:         true,
		LastRefreshedAt:     time.Now(),
		Version:             prev.Version,
		Healthy:             prev.Healthy,
		ConsecutiveFailures: prev.ConsecutiveFailures,
		LastError:           prev.LastError,
	}

	if err != nil {
		next.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		next.LastError = err.Error()
		if next.ConsecutiveFailures >= readinessFailureThreshold {
			next.Healthy = false
		}
		logger.Warn("readyz refresh failed",
			zap.Int("consecutiveFailures", next.ConsecutiveFailures),
			zap.Error(err))
	} else {
		next.Healthy = healthy
		next.Version = version
		next.ConsecutiveFailures = 0
		next.LastError = ""
		// Record opencode_up gate on first successful health check.
		if healthy && gr != nil {
			gr.MaybeRecord(gateOpencodeUp)
		}
	}

	cache.snapshot.Store(&next)
}

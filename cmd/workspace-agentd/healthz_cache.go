// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Timing knobs are vars (not consts) so tests can shrink the loops into
// sub-second territory — same pattern as memoryWarningThreshold /
// memoryCheckInterval in memory_pressure.go. Production values are the
// defaults here; nothing in prod overrides them.
var (
	readinessRefreshInterval  = 5 * time.Second
	readinessRefreshTimeout   = 4 * time.Second
	readinessFailureThreshold = 3

	// watchdogMaxDeferrals caps how many polls the watchdog will defer
	// when sessions are busy. At 5s intervals, 60 deferrals = ~5 minutes.
	// After this, the restart is forced regardless of busy state — unless
	// vitals evidence (watchdog_vitals.go) says opencode is alive (starved
	// or flat) or that crash recovery owns the respawn, in which case the
	// deferral window is EXTENDED (the force exists for stale busy state
	// on a dead-listener hang, which the vitals evidence rules out). Var
	// for tests.
	watchdogMaxDeferrals = 60

	// watchdogUnknownEpisodeBound (#1632 fix #2): how long a CONTINUOUS
	// UNKNOWN episode may suppress before the suppression itself is
	// escalated to a bounded soft restart. Rationale: "killing without
	// evidence is banned" (#892) assumed UNKNOWN means "the probe is
	// momentarily degraded" — the 2026-10-08 incident proved a topology
	// where UNKNOWN was PERMANENT by design (sidecar agentd structurally
	// unable to read the workspace container's /proc): 36+ suppressions,
	// a wedged-but-listening opencode, zero self-healing for 30+
	// minutes. After this bound the watchdog fires once per episode
	// (rate-limited by the existing maybeFire), writes a marker, and
	// recovers the agent — trading a bounded risk of one restart against
	// an unbounded hang. Non-UNKNOWN suppressions (FLAT/STARVED/RESPAWN)
	// are honest evidence of a live process and never escalate. Var for
	// tests.
	watchdogUnknownEpisodeBound = 15 * time.Minute
)

const (
	// watchdogMaxRestarts caps the number of health-watchdog restarts
	// within watchdogRestartWindow. Once the cap is hit, the watchdog
	// stops firing — the problem is persistent and tight-loop restarting
	// would only waste resources. At that point an operator must
	// intervene (the pod's livenessProbe targets /v1/healthz which
	// hardcodes Healthy:true and does not check opencode, so kubelet
	// will not kill the pod on its own).
	watchdogMaxRestarts   = 3
	watchdogRestartWindow = 10 * time.Minute

	// healthyEpisodeResetPolls (#1632 fix #3): how many CONSECUTIVE
	// healthy polls end an unhealthy episode for liveness purposes. The
	// 2026-10-08 incident's wedge CYCLED — stall ~10min, one brief
	// healthy poll, stall again — so a single healthy poll must not
	// reset the clock (a continuous-failure bound would never fire).
	// Three consecutive polls (15s at the 5s cadence) is responsiveness,
	// not a blip.
	healthyEpisodeResetPolls = 3

	// agentUnhealthyEpisodeSustain (#1632 fix #3): how long an unhealthy
	// episode must persist before /v1/agentz (the sidecar-mode workspace
	// container's liveness target) starts failing. Generously beyond
	// the #892 starvation bursts (2026-08-15: ~3 consecutive failures,
	// then real recovery — duty cycle far below a sustained episode) and
	// beyond legitimate long-turn /global/health blackouts; far below
	// the incident's 30+ minute stall loop. kubelet adds its own
	// failureThreshold margin on top (8×10s).
	agentUnhealthyEpisodeSustain = 10 * time.Minute
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

	// UnhealthyEpisodeStartedAt (#1632 fix #3): when the current
	// unhealthy EPISODE began (first unhealthy poll after the last
	// sustained recovery). Zero when not in an episode. "Sustained
	// recovery" = healthyEpisodeResetPolls consecutive healthy polls —
	// brief single-poll recoveries (the incident's stall/recover
	// cycling) deliberately do NOT reset it. /v1/agentz (liveness)
	// fails only once time.Since(this) ≥ agentUnhealthyEpisodeSustain.
	UnhealthyEpisodeStartedAt time.Time

	// ConsecutiveHealthy drives the episode reset (see above).
	ConsecutiveHealthy int

	// Generation (#1632 r3) is the agent-generation epoch: every
	// noteAgentGeneration boundary bumps it and clears the episode.
	// refreshOnce is a read-modify-write spanning seconds of I/O; the
	// epoch lets its final Store detect that a boundary landed
	// mid-poll and preserve the boundary's episode-clear instead of
	// reinstating the stale pre-poll clock (the lost-update race the
	// r3 reviewer demonstrated empirically — a wedged agent times out
	// every poll, so boundaries land inside refresh windows with high
	// probability, and the reinstated ≥10m episode would 503 the
	// booting replacement into a kill loop).
	Generation uint64
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

// noteAgentGeneration re-arms the unhealthy-episode clock at an agent
// generation boundary (#1632 r1 review): a NEW child (operator restart,
// crash recovery, kubelet container restart) must not inherit the dead
// generation's old episode — otherwise /v1/agentz keeps failing the
// liveness probe through the replacement's boot and kubelet kills it
// mid-start (a boot-kill loop). Implemented as a generation-epoch bump
// (#1632 r3): the bump is what refreshOnce's Store detects and
// preserves — a plain episode-clear here could be clobbered by an
// in-flight refresh whose pre-poll snapshot still carried the old
// clock. Everything else in the snapshot rides through untouched. CAS
// loop against the refresher's own CAS Store.
func (c *healthzCache) noteAgentGeneration() {
	for {
		p := c.snapshot.Load()
		n := *p
		n.Generation = p.Generation + 1
		n.UnhealthyEpisodeStartedAt = time.Time{}
		if c.snapshot.CompareAndSwap(p, &n) {
			return
		}
	}
}

// storeResolved CAS-stores a completed refresh's result, resolving a
// generation bump that landed mid-poll: if the current snapshot's
// epoch no longer matches the one the poll started from, the boundary's
// episode-clear is newer truth than next's stale pre-poll derivation —
// next adopts the current epoch and episode fields (the boundary owns
// episode state across its bump; the poll owns health/version/failure
// fields). Retries on concurrent swaps (noteAgentGeneration).
func (c *healthzCache) storeResolved(prev *healthzCacheSnapshot, next *healthzCacheSnapshot) {
	for {
		cur := c.snapshot.Load()
		next.Generation = cur.Generation
		if cur.Generation != prev.Generation {
			next.UnhealthyEpisodeStartedAt = cur.UnhealthyEpisodeStartedAt
			next.ConsecutiveHealthy = cur.ConsecutiveHealthy
		}
		if c.snapshot.CompareAndSwap(cur, next) {
			return
		}
	}
}

// healthWatchdogRestarter is the narrow interface the health-watchdog
// uses to restart opencode. *managedProcess satisfies this interface.
// The indirection exists so tests can inject a fake restarter without
// spawning real subprocesses.
type healthWatchdogRestarter interface {
	restart()
}

// sessionBusyChecker returns true if any session is currently busy
// (LLM turn in progress). The watchdog uses this to defer restarts
// during active turns, preventing false-positive kills of legitimate
// long-running responses. *sessionStatusTracker satisfies this via
// trackerHasBusyOrUnknown.
type sessionBusyChecker interface {
	anyBusy() bool
}

// busySessionChecker wraps *sessionStatusTracker to satisfy
// sessionBusyChecker. Returns true if the tracker shows any busy or
// unknown sessions.
type busySessionChecker struct {
	tracker *sessionStatusTracker
}

func (b *busySessionChecker) anyBusy() bool {
	return trackerHasBusyOrUnknown(b.tracker)
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
	restarts       []time.Time // timestamps of recent restarts (for rate limiting)
	fired          bool        // latches: fire only once per unhealthy episode
	deferredLogged bool        // latches: log session-busy deferral (superseded by deferCount)
	giveUpLogged   bool        // latches: log rate-limit give-up only once per episode
	maxDeferLogged bool        // latches: log max-defer exceeded only once per episode
	deferCount     int         // consecutive deferrals due to busy sessions
	totalFired     int         // total watchdog restarts since boot
	maxRestarts    int
	maxDeferrals   int // injectable for fast tests (default: watchdogMaxDeferrals)
	window         time.Duration

	// Suppression state (watchdog_vitals.go, #892 / design 0050 D1).
	// Suppressing a restart consumes neither the fired latch nor the
	// rate-limit budget; suppressedCount resets on recovery like every
	// other episode latch. There is deliberately NO stand-down: chronic
	// starvation is expected under the fixed 2-CPU quota, and
	// suppressing forever is the policy — visibility comes from
	// workspace_watchdog_suppressions_total{reason} and the periodic
	// Warn logs, not from re-arming the kill.
	suppressedCount int  // consecutive suppressions this episode (any reason)
	suppressLogged  bool // latches: log the first suppression (then every 12th)
	// unknownSince (#1632 fix #2): when the current run of CONSECUTIVE
	// verdictUnknown suppressions began. Zero when the last verdict was
	// not UNKNOWN (or the episode reset). Ages toward
	// watchdogUnknownEpisodeBound; any non-UNKNOWN verdict or a healthy
	// transition re-zeroes it.
	unknownSince time.Time
}

func newHealthWatchdog() *healthWatchdog {
	return &healthWatchdog{
		maxRestarts:  watchdogMaxRestarts,
		maxDeferrals: watchdogMaxDeferrals,
		window:       watchdogRestartWindow,
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
	wd.deferredLogged = false
	wd.giveUpLogged = false
	wd.maxDeferLogged = false
	wd.deferCount = 0
	wd.suppressedCount = 0
	wd.suppressLogged = false
	wd.unknownSince = time.Time{}
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
//
// busyChecker, if non-nil, is consulted before restarting. If sessions
// are busy (LLM turn in progress), the restart is deferred to avoid
// killing legitimate long-running turns (issue #807 Assumption #2).
// The latch ensures we only log/defer once per episode.
//
// vitals, if non-nil, is the corroboration probe (see watchdog_vitals.go).
// It is consulted at every would-fire moment — including the max-defer
// force path. Only a corroborated dead-listener hang (verdictHung: dial
// refused, supervised pid alive) reaches the restart; every other verdict
// suppresses without consuming the latch or rate-limit budget (#892 /
// design 0050 D1). nil disables corroboration (tests, partial wiring) and
// the watchdog keeps its pre-corroboration semantics exactly.
func refreshIsHealthyLoop(ctx context.Context, client *OpenCodeClient, cache *healthzCache, logger *zap.Logger, gr *gateRecorder, restarter healthWatchdogRestarter, busyChecker sessionBusyChecker, vitals vitalsGatherer) {
	wd := newHealthWatchdog()
	watchdogLogger := logger.With(zap.String("component", "health_watchdog"))

	// bootCompleted gates the watchdog: it stays false until the first
	// successful health check, then latches true. This prevents the
	// watchdog from firing during a legitimate slow boot (where opencode
	// hasn't started serving yet). The crash-recovery path (cmd.Wait())
	// handles genuine boot-loop crashes; the watchdog's job is to catch
	// a previously-healthy opencode that later hangs.
	bootCompleted := false

	tick, stopTick := agentdNewTicker(readinessRefreshInterval)
	defer stopTick()

	// Immediate first refresh on boot.
	refreshOnce(ctx, client, cache, watchdogLogger, gr)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
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
				// Session-aware deferral (issue #807 Assumption #2):
				// If sessions are busy (LLM turn in progress), the
				// health-check failures may be a false positive caused
				// by CPU contention. Defer the restart without consuming
				// the latch or rate-limit budget. The watchdog re-checks
				// every poll; when sessions go idle, the restart proceeds.
				//
				// If opencode is truly hung, the session tracker's busy
				// state is stale (it only clears on SSE idle events that
				// will never arrive). To prevent deferral-forever, a
				// max-defer counter forces the restart after
				// watchdogMaxDeferrals polls (~5 min). This mirrors the
				// maxDefer pattern in secrets.go's credential-reload path.
				forceDespiteBusy := false
				if busyChecker != nil && busyChecker.anyBusy() {
					wd.deferCount++
					if wd.deferCount <= wd.maxDeferrals {
						if !wd.deferredLogged || wd.deferCount%6 == 0 {
							watchdogLogger.Warn("health-watchdog deferring restart — sessions are busy",
								zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
								zap.String("lastError", snap.LastError),
								zap.Int("deferCount", wd.deferCount),
								zap.Int("maxDefers", watchdogMaxDeferrals),
							)
						}
						continue
					}
					if !wd.maxDeferLogged {
						wd.maxDeferLogged = true
						watchdogLogger.Warn("health-watchdog max-defer exceeded — forcing restart despite busy sessions",
							zap.Int("deferCount", wd.deferCount),
							zap.Int("maxDefers", watchdogMaxDeferrals),
						)
					}
					forceDespiteBusy = true
				}

				// Vitals corroboration (#892 / design 0050 D1, see
				// watchdog_vitals.go): timeout evidence alone cannot
				// distinguish a hung event loop from a starved, blocked,
				// or respawning one. Before firing — including on the
				// max-defer force path — gather one vital-signs sample.
				//
				// The kill set is DEAD-LISTENER ONLY: verdictHung (dial
				// refused, supervised pid alive) is the sole verdict that
				// reaches the restart. STARVED (advancing), FLAT (blocked
				// on upstream I/O — alive), RESPAWN (crash recovery owns
				// the respawn), and UNKNOWN (no evidence — killing
				// without evidence is banned) all suppress, re-arm the
				// busy-deferral window, and count in
				// workspace_watchdog_suppressions_total{reason}.
				if vitals != nil {
					v := vitals.gather(ctx)
					verdict, why := v.classify()
					if verdict != verdictHung {
						wd.suppressedCount++
						// #1632 fix #2 — age the UNKNOWN run. Only
						// CONSECUTIVE UNKNOWN verdicts age toward the bound;
						// any honest evidence verdict (STARVED/FLAT/RESPAWN)
						// re-zeroes the clock (a live process is producing
						// evidence; the banned-kill policy stands).
						now := time.Now()
						if verdict == verdictUnknown {
							if wd.unknownSince.IsZero() {
								wd.unknownSince = now
							}
						} else {
							wd.unknownSince = time.Time{}
						}
						if forceDespiteBusy {
							// Evidence says opencode is alive (or the
							// respawn owns it): the busy sessions may be
							// real, so re-arm the deferral window instead
							// of forcing a kill.
							wd.deferCount = 0
							wd.maxDeferLogged = false
							wd.deferredLogged = false
						}
						if !wd.suppressLogged || wd.suppressedCount%12 == 0 {
							logFn := watchdogLogger.Warn
							if verdict == verdictUnknown {
								// Unknown = the probe itself is degraded
								// (no pid, /proc failure). Raise the
								// volume: this needs operator attention
								// even though killing stays banned.
								logFn = watchdogLogger.Error
							}
							logFn("health-watchdog suppressing restart — vitals say not a dead-listener hang",
								zap.String("verdict", v.suppressionReason()),
								zap.String("evidence", why),
								zap.Float64("cgroupThrottledMS", v.throttleDeltaUS/1e6),
								zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
								zap.String("lastError", snap.LastError),
								zap.Int("suppressions", wd.suppressedCount),
							)
							wd.suppressLogged = true
						}
						pkgOpsMetrics.RecordWatchdogSuppression(workspaceIDFromEnv(), v.suppressionReason())

						// #1632 fix #2 — the bound. A continuous UNKNOWN
						// episode this old, on a past-boot process with
						// continuous health timeouts, is no longer "the
						// probe is momentarily degraded": the evidence
						// channel itself is broken (by deployment design
						// pre-#1631 sidecar topology, or a dead control
						// socket), and suppressing forever means hanging
						// forever. Escalate ONCE per episode to a soft
						// restart — maybeFire's latch + rate limit bound
						// the blast radius exactly like a corroborated
						// dead-listener kill; the fired latch holds until
						// a healthy poll resets the episode.
						if verdict == verdictUnknown &&
							!wd.unknownSince.IsZero() &&
							now.Sub(wd.unknownSince) >= watchdogUnknownEpisodeBound {
							if wd.maybeFire(now) {
								watchdogLogger.Error("health-watchdog UNKNOWN-episode bound exceeded — escalating to bounded soft restart",
									zap.String("evidence", why),
									zap.Duration("unknownEpisode", now.Sub(wd.unknownSince).Round(time.Second)),
									zap.Duration("bound", watchdogUnknownEpisodeBound),
									zap.Int("suppressions", wd.suppressedCount),
									zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
									zap.String("lastError", snap.LastError),
									zap.Int("totalWatchdogRestarts", wd.totalFired),
								)
								if err := writeRestartReasonMarker(markerPathFromEnv(), RestartReasonHealthWatchdogUnknownEpisode, nil); err != nil {
									watchdogLogger.Error("failed to write UNKNOWN-episode restart-reason marker", zap.Error(err))
									pkgOpsMetrics.RecordMarkerWriteFailure(workspaceIDFromEnv(), RestartReasonHealthWatchdogUnknownEpisode)
								}
								logRestartReasonAtWrite(RestartReasonHealthWatchdogUnknownEpisode, nil, watchdogLogger.Core())
								pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), RestartReasonHealthWatchdogUnknownEpisode)
								if restarter != nil {
									go restarter.restart()
								}
							} else if !wd.giveUpLogged {
								// Rate-limited out (or latched): say so once.
								wd.giveUpLogged = true
								watchdogLogger.Warn("UNKNOWN-episode bound exceeded but watchdog cannot fire (latched/rate-limited) — suppressing until the window allows",
									zap.Duration("unknownEpisode", now.Sub(wd.unknownSince).Round(time.Second)),
								)
							}
						}
						continue
					}
					watchdogLogger.Info("health-watchdog corroborated dead-listener hang",
						zap.String("evidence", why),
						zap.Int("consecutiveFailures", snap.ConsecutiveFailures))
				}

				if wd.maybeFire(time.Now()) {
					watchdogLogger.Warn("opencode health-watchdog triggering restart",
						zap.Int("consecutiveFailures", snap.ConsecutiveFailures),
						zap.String("lastError", snap.LastError),
						zap.Int("totalWatchdogRestarts", wd.totalFired),
						zap.Int("restartsInWindow", len(wd.restarts)),
					)
					if err := writeRestartReasonMarker(markerPathFromEnv(), RestartReasonHealthWatchdog, nil); err != nil {
						watchdogLogger.Error("failed to write health-watchdog restart-reason marker", zap.Error(err))
						pkgOpsMetrics.RecordMarkerWriteFailure(workspaceIDFromEnv(), RestartReasonHealthWatchdog)
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
				// Episode state rides through a panic (a panic is an
				// unhealthy poll; the clock logic above is not re-run).
				UnhealthyEpisodeStartedAt: prev.UnhealthyEpisodeStartedAt,
				ConsecutiveHealthy:        0,
			}
			if next.ConsecutiveFailures >= readinessFailureThreshold {
				next.Healthy = false
			}
			if !next.Healthy && next.UnhealthyEpisodeStartedAt.IsZero() {
				next.UnhealthyEpisodeStartedAt = time.Now()
			}
			cache.storeResolved(&prev, &next)
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
		if healthy {
			next.ConsecutiveHealthy = prev.ConsecutiveHealthy + 1
			// Record opencode_up gate on first successful health check.
			if gr != nil {
				gr.MaybeRecord(gateOpencodeUp)
			}
		}
	}

	// Unhealthy-episode tracking (#1632 fix #3), keyed off the resolved
	// Healthy flag so the erred-poll and unhealthy-answer paths share
	// one clock: the episode starts when Healthy first drops (threshold
	// crossing for erred polls; immediate for an answered-unhealthy
	// body) and ends only after healthyEpisodeResetPolls CONSECUTIVE
	// healthy polls — a brief single-poll recovery (the incident's
	// stall/recover cycling) does not reset it. No boot gating: a
	// from-boot-wedged agent that binds its port but never answers
	// health is exactly what /v1/agentz exists to catch.
	if !next.Healthy {
		next.ConsecutiveHealthy = 0
		if prev.UnhealthyEpisodeStartedAt.IsZero() {
			next.UnhealthyEpisodeStartedAt = time.Now()
		} else {
			next.UnhealthyEpisodeStartedAt = prev.UnhealthyEpisodeStartedAt
		}
	} else if next.ConsecutiveHealthy >= healthyEpisodeResetPolls {
		next.UnhealthyEpisodeStartedAt = time.Time{}
	} else {
		next.UnhealthyEpisodeStartedAt = prev.UnhealthyEpisodeStartedAt
	}

	// CAS-resolved store (#1632 r3): preserves a generation bump that
	// landed during the poll instead of clobbering it with the stale
	// pre-poll episode derivation.
	cache.storeResolved(&prev, &next)
}

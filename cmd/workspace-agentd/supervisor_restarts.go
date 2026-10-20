// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_restarts.go — #1760: the in-agentd recorder behind the
// supervisor slice of /v1/healthz and /v1/statusz. The health-watchdog
// runs in agentd in BOTH container topologies (single-container: the
// in-process restarter; sidecar: the control-socket restarter — the
// loop itself is shared, server.go backgroundLoops), so this one log
// owns restart attribution for watchdog fires in both. Process-crash
// restarts record here in both topologies too (#1776 closed the A11
// gap): single-container via the restartObserver hook, sidecar via the
// supervisor-status poller's crash-counter delta (reason crash/oom,
// empty verdict).
//
// Recording hooks ONLY — the suppression/kill decisions in
// healthz_cache.go and watchdog_vitals.go are untouched.
//
// Counts are LIFETIME-since-agentd-boot (orchestrator A3 ruling):
// unlike managedProcess.restartCount (a crash-loop backoff counter that
// resets on operator restart and on >60s uptime), AgentRestarts never
// decrements. Handlers read via snapshot() — a lock-free copy under the
// internal mutex; the US-22.1 healthz contract (no I/O, cached state
// only) is preserved.

import (
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// agentRestartLog records supervisor/watchdog agent restarts and
// suppressed would-fire restarts. All methods are safe from any
// goroutine.
type agentRestartLog struct {
	mu sync.Mutex
	// restarts is the lifetime count of recorded restarts.
	restarts int
	// last is the most recent recorded restart (nil until the first).
	last *agentd.AgentRestartEvent
	// suppressed is the lifetime count of suppressed would-fires.
	suppressed int
	// lastSuppressedVerdict is the verdict of the most recent
	// suppression ("" until the first).
	lastSuppressedVerdict string
}

func newAgentRestartLog() *agentRestartLog {
	return &agentRestartLog{}
}

// record logs one restart with its attribution. verdict may be empty
// (non-watchdog restarts, or a fire without corroboration evidence).
func (l *agentRestartLog) record(reason, verdict, lastError string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restarts++
	l.last = &agentd.AgentRestartEvent{
		Reason:    reason,
		Verdict:   verdict,
		LastError: lastError,
		At:        at,
	}
}

// recordSuppression logs one suppressed would-fire restart. The verdict
// is the watchdog's classification (starved | flat | respawn | unknown —
// verdictHung never suppresses).
func (l *agentRestartLog) recordSuppression(verdict string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.suppressed++
	l.lastSuppressedVerdict = verdict
}

// snapshot returns a point-in-time copy for the healthz/statusz
// handlers, or nil when nothing was ever recorded (the mixed-fleet
// omission contract: no evidence is not a restart).
func (l *agentRestartLog) snapshot() *agentd.SupervisorStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restarts == 0 && l.suppressed == 0 && l.last == nil {
		return nil
	}
	s := &agentd.SupervisorStatus{
		AgentRestarts:         l.restarts,
		SuppressedRestarts:    l.suppressed,
		LastSuppressedVerdict: l.lastSuppressedVerdict,
	}
	if l.last != nil {
		ev := *l.last
		s.LastAgentRestart = &ev
	}
	return s
}

// supervisorSnapshotFor projects the deps' restart log + readiness cache
// into the handler-facing snapshot closure (nil when the log is unwired,
// so both handlers omit the field — mixed-fleet contract). Mirrors
// relayLivenessSnapshotFor.
func supervisorSnapshotFor(deps serverDeps) func() *agentd.SupervisorStatus {
	return supervisorSnapshotFunc(deps.restartLog, deps.healthCache)
}

// supervisorSnapshotFunc builds the handler-facing snapshot closure:
// the restart log merged with the readiness cache's current failure run
// and last error — one view, cached state only (US-22.1). A nil log
// yields a nil func so handlers omit the field entirely (unwired
// topologies/tests).
func supervisorSnapshotFunc(l *agentRestartLog, cache *healthzCache) func() *agentd.SupervisorStatus {
	if l == nil {
		return nil
	}
	return func() *agentd.SupervisorStatus {
		snap := l.snapshot()
		if snap == nil {
			snap = &agentd.SupervisorStatus{}
		}
		hc := cache.Snapshot()
		snap.ConsecutiveFailures = hc.ConsecutiveFailures
		snap.LastHealthError = hc.LastError
		return snap
	}
}

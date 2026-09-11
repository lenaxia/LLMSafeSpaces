// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package obs holds cross-binary observability contracts. The
// loop-liveness family (epic 71 / 0c, #1319) is stamped by periodic loops
// in BOTH the agentd binary and the API binary — this package owns the
// constants AND the single registration, so the stamps can never drift
// and no binary can register the family twice (two promauto collectors
// under one name panic at init; 0c part 2 centralized what #1322's
// constants-only version left per-package).
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The loop-liveness family contract: every periodic loop stamps the unix
// timestamp of its last COMPLETED pass under its own loop label. A loop
// whose series goes stale has died silently; alerting on staleness is
// gated until L3/L4 are green (epic-71 wave plan).
const (
	LoopLivenessMetric = "llmsafespaces_loop_last_run_timestamp_seconds"
	LoopLivenessLabel  = "loop"
	LoopLivenessHelp   = "Unix timestamp of each periodic loop's last COMPLETED pass (epic-71 dead-loop detection; the `loop` label names the exporter — a loop whose last-run goes stale has died silently)."
)

// Loop names — the family's vocabulary. New loops append here; the label
// value is the staleness rule's key.
const (
	LoopReconcileWatchdog   = "reconcile_watchdog"    // agentd: the #1311 reconcile watchdog (both pod modes)
	LoopOutboxParkedSweeper = "outbox_parked_sweeper" // API: the #1316 parked-error sweeper (its Run loop only)
	LoopCanaryProbe         = "canary_probe"          // API: the #1312 canary probe runner (0c)
)

// loopLastRun is the family's only registration (one per binary; both
// binaries import this package).
var loopLastRun = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: LoopLivenessMetric,
	Help: LoopLivenessHelp,
}, []string{LoopLivenessLabel})

// StampLoopLastRun records a periodic loop's last COMPLETED pass. Call
// from the loop itself, at end-of-pass only — direct/one-shot callers
// must not refresh it (a dead loop must be allowed to look stale).
func StampLoopLastRun(loop string) {
	loopLastRun.WithLabelValues(loop).SetToCurrentTime()
}

// LoopLastRun exposes the family gauge for tests (per-pass-advance pins
// and scrape-completeness rows). Production code stamps; it does not read.
func LoopLastRun() *prometheus.GaugeVec {
	return loopLastRun
}

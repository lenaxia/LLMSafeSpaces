// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package obs holds cross-binary observability contracts. The
// loop-liveness family (epic 71 / 0c, #1319) is registered by BOTH the
// agentd binary and the API binary — these constants are the single
// source so the two registrations can never drift apart.
package obs

// LoopLivenessMetric is the shared dead-loop-detection family: every
// periodic loop stamps the unix timestamp of its last COMPLETED pass
// with its own loop label.
const LoopLivenessMetric = "llmsafespaces_loop_last_run_timestamp_seconds"

// LoopLivenessLabel is the family's only label.
const LoopLivenessLabel = "loop"

// LoopLivenessHelp documents the family uniformly across binaries.
const LoopLivenessHelp = "Unix timestamp of each periodic loop's last COMPLETED pass (epic-71 dead-loop detection; the `loop` label names the exporter — a loop whose last-run goes stale has died silently)."

// Loop label values (snake_case).
const (
	LoopOutboxParkedSweeper = "outbox_parked_sweeper"
	LoopReconcileWatchdog   = "reconcile_watchdog"
)

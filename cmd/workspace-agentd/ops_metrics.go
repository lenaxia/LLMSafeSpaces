// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// opsMetrics holds the workspace-level Prometheus metrics required by
// US-44.8 for SRE dashboards. All metrics are NOT user-facing.
//
// Registered via promauto (default Prometheus registry) so they appear
// on the agentd admin port (:4098/metrics) alongside gate timings. The
// chart ships a PodMonitor that scrapes this endpoint on every workspace
// pod — see helm/templates/podmonitor-agentd.yaml.
type opsMetrics struct {
	restartsTotal     *prometheus.CounterVec
	trackerBusyResets *prometheus.CounterVec
	memoryBytes       *prometheus.GaugeVec
	activeSessions    *prometheus.GaugeVec
	contextTokens     *prometheus.GaugeVec
	// watchdogSuppressions counts would-fire moments the health-watchdog
	// withheld because vitals corroboration (watchdog_vitals.go) showed a
	// non-lethal state: starved (CPU advancing), flat (blocked on
	// upstream I/O), respawn (crash recovery owns it), or unknown (probe
	// degraded — killing without evidence is banned, #892). Sustained
	// growth on a workspace is an operator signal (CPU quota vs load,
	// probe breakage), not an opencode problem.
	watchdogSuppressions *prometheus.CounterVec
	// markerWriteFailures counts failed restart-reason marker writes. The
	// 2026-08-15 incident had 9 attempted marker writes land 0 (only
	// visible in container stdout); this counter makes that loss visible
	// on /metrics.
	markerWriteFailures *prometheus.CounterVec
	// orphansReaped counts zombie children reaped by the orphan reaper
	// (#904). Steady low-rate noise on a healthy workspace; sustained
	// growth points at a tool population being orphaned mid-execution
	// (the #892 stuck-running correlation).
	orphansReaped *prometheus.CounterVec
}

// pkgOpsMetrics is the package-level singleton. Tests create their own
// via newOpsMetrics (which shares the same registered collectors since
// promauto registers on init).
var pkgOpsMetrics = newOpsMetrics()

func newOpsMetrics() *opsMetrics {
	return &opsMetrics{
		restartsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_restarts_total",
			Help: "Total opencode restarts by reason (env_secrets, api_key, crash, oom, user_requested, health_watchdog)",
		}, []string{"workspace_id", "reason"}),

		trackerBusyResets: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_tracker_busy_resets_total",
			Help: "Orphaned busy flags cleared at opencode generation change (design 0050 D2); increments by the number of sessions healed per reset",
		}, []string{"workspace_id"}),

		memoryBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_memory_bytes",
			Help: "Current memory usage in bytes (from cgroup v2 memory.current)",
		}, []string{"workspace_id"}),

		activeSessions: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_active_sessions",
			Help: "Number of sessions currently marked busy (from sessionStatusTracker)",
		}, []string{"workspace_id"}),

		contextTokens: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_context_tokens",
			Help: "Sum of context tokens (input + cache) across all tracked sessions",
		}, []string{"workspace_id"}),

		watchdogSuppressions: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_watchdog_suppressions_total",
			Help: "Health-watchdog restarts suppressed by vitals corroboration, by reason (starved, flat, respawn, unknown) — #892/design 0050 D1",
		}, []string{"workspace_id", "reason"}),

		markerWriteFailures: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_restart_marker_write_failures_total",
			Help: "Failed restart-reason marker writes by restart reason (observability: the marker is the persistent incident record)",
		}, []string{"workspace_id", "reason"}),

		orphansReaped: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_orphans_reaped_total",
			Help: "Zombie children reaped by the orphan reaper (adopted grandchildren of agentd, #904)",
		}, []string{"workspace_id"}),
	}
}

// RecordTrackerBusyReset adds n healed sessions to the busy-reset
// counter (design 0050 D2 observability).
func (m *opsMetrics) RecordTrackerBusyReset(workspaceID string, n int) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.trackerBusyResets.WithLabelValues(workspaceID).Add(float64(n))
}

// RecordWatchdogSuppression increments the suppression counter for the
// workspace with the verdict's reason label. See
// opsMetrics.watchdogSuppressions.
func (m *opsMetrics) RecordWatchdogSuppression(workspaceID, reason string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if reason == "" {
		reason = "unknown"
	}
	m.watchdogSuppressions.WithLabelValues(workspaceID, reason).Inc()
}

// RecordMarkerWriteFailure counts a failed restart-reason marker write.
func (m *opsMetrics) RecordMarkerWriteFailure(workspaceID, reason string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if reason == "" {
		reason = "unknown"
	}
	m.markerWriteFailures.WithLabelValues(workspaceID, reason).Inc()
}

// RecordOrphanReap counts one zombie reaped by the orphan reaper
// (#904 observability).
func (m *opsMetrics) RecordOrphanReap(workspaceID string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.orphansReaped.WithLabelValues(workspaceID).Inc()
}

// RecordRestart increments the restart counter for the given reason.
// Reasons: env_secrets, api_key, crash, oom, user_requested, health_watchdog.
func (m *opsMetrics) RecordRestart(workspaceID, reason string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.restartsTotal.WithLabelValues(workspaceID, reason).Inc()
}

// SetMemoryUsage sets the current memory usage gauge.
func (m *opsMetrics) SetMemoryUsage(workspaceID string, bytes int64) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.memoryBytes.WithLabelValues(workspaceID).Set(float64(bytes))
}

// SetActiveSessions sets the active (busy) session count gauge.
func (m *opsMetrics) SetActiveSessions(workspaceID string, count int) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.activeSessions.WithLabelValues(workspaceID).Set(float64(count))
}

// SetContextTokens sets the total context tokens gauge.
func (m *opsMetrics) SetContextTokens(workspaceID string, tokens int64) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	m.contextTokens.WithLabelValues(workspaceID).Set(float64(tokens))
}

// UpdateFromTracker reads busy session count and total prompt tokens
// from the sessionStatusTracker and updates the corresponding gauges.
// Called periodically from the background metrics-collection loop.
func (m *opsMetrics) UpdateFromTracker(workspaceID string, tracker *sessionStatusTracker) {
	if tracker == nil {
		return
	}
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	busy, tokens := tracker.snapshot()
	m.SetActiveSessions(workspaceID, busy)
	m.SetContextTokens(workspaceID, tokens)
}

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
	restartsTotal  *prometheus.CounterVec
	memoryBytes    *prometheus.GaugeVec
	activeSessions *prometheus.GaugeVec
	contextTokens  *prometheus.GaugeVec
}

// pkgOpsMetrics is the package-level singleton. Tests create their own
// via newOpsMetrics (which shares the same registered collectors since
// promauto registers on init).
var pkgOpsMetrics = newOpsMetrics()

func newOpsMetrics() *opsMetrics {
	return &opsMetrics{
		restartsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_restarts_total",
			Help: "Total opencode restarts by reason (env_secrets, api_key, crash, oom, user_requested)",
		}, []string{"workspace_id", "reason"}),

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
	}
}

// RecordRestart increments the restart counter for the given reason.
// Reasons: env_secrets, api_key, crash, oom, user_requested.
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

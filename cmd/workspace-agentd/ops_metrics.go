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
	// fileUploads counts PUT /v1/files outcomes (Epic 68 US-68.1):
	// accepted, rejected_name, rejected_cap, write_error, unauthorized —
	// the design doc's agentd-side observability (cap hits + write
	// failures) plus the rejection reasons the API cannot see.
	fileUploads *prometheus.CounterVec
	// uploadScrubRemoved counts stale staging-*.tmp upload temps removed
	// by the boot scrub (design epic-68 D3; the 0060 structural marker).
	uploadScrubRemoved *prometheus.CounterVec
	// Design 0060 §4.6 staging surfaces.
	uploadStagingBytes      *prometheus.GaugeVec
	uploadStagingReserved   *prometheus.GaugeVec
	uploadStagingFiles      *prometheus.GaugeVec
	uploadStagingCredential *prometheus.GaugeVec
	uploadBytesTotal        *prometheus.CounterVec
	uploadDestOutcomes      *prometheus.CounterVec
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

		fileUploads: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_agentd_file_uploads_total",
			Help: "PUT /v1/files outcomes by reason (accepted, rejected_name, rejected_cap, write_error, unauthorized) — Epic 68 US-68.1",
		}, []string{"workspace_id", "outcome"}),

		uploadScrubRemoved: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_agentd_upload_scrub_removed_total",
			Help: "Stale staging-*.tmp upload temps removed by the agentd boot scrub (Epic 68 D3 atomic-or-absent contract; design 0060 structural marker)",
		}, []string{"workspace_id"}),

		// Design 0060 §4.6: the staging-leg surfaces.
		uploadStagingBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_agentd_upload_staging_bytes",
			Help: "Staged-upload bytes on the credential-shared tmpfs (walked truth, reconciled by the sweeper) — the §6.1 residency pin reads this",
		}, []string{"workspace_id"}),
		uploadStagingReserved: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_agentd_upload_staging_reserved_bytes",
			Help: "Admitted-not-released upload reservations (held until the bytes leave the tmpfs)",
		}, []string{"workspace_id"}),
		uploadStagingFiles: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_agentd_upload_staging_files",
			Help: "Staged upload objects in the staging dir",
		}, []string{"workspace_id"}),
		uploadStagingCredential: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workspace_agentd_upload_staging_credential_bytes",
			Help: "Credential-surface usage on the shared tmpfs (the clause-B admission input — makes the floor policy auditable)",
		}, []string{"workspace_id"}),
		uploadBytesTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_agentd_upload_bytes_total",
			Help: "Cumulative upload bytes by direction: staged_in (API→tmpfs) and copied_out (tmpfs→PVC, from the ack's verified size)",
		}, []string{"workspace_id", "direction"}),

		uploadDestOutcomes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "workspace_agentd_upload_dest_outcomes_total",
			Help: "Supervisor-side destination outcomes by code (dest_disk_full, integrity mismatches, dest_margin_consumed) — counted by agentd from the upload_apply ack (design 0060 §4.6)",
		}, []string{"workspace_id", "code"}),
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

// RecordUploadOutcome counts one PUT /v1/files request resolution
// (Epic 68 US-68.1 observability).
func (m *opsMetrics) RecordUploadOutcome(workspaceID string, outcome uploadOutcome) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	m.fileUploads.WithLabelValues(workspaceID, string(outcome)).Inc()
}

// RecordStagingGauges pushes the design-0060 §4.6 staging snapshot.
func (m *opsMetrics) RecordStagingGauges(stagedBytes, reservedBytes, credentialBytes int64, files int) {
	ws := uploadWorkspaceID()
	m.uploadStagingBytes.WithLabelValues(ws).Set(float64(stagedBytes))
	m.uploadStagingReserved.WithLabelValues(ws).Set(float64(reservedBytes))
	m.uploadStagingCredential.WithLabelValues(ws).Set(float64(credentialBytes))
	m.uploadStagingFiles.WithLabelValues(ws).Set(float64(files))
}

// RecordUploadBytes counts staged_in / copied_out upload bytes (§4.6).
func (m *opsMetrics) RecordUploadBytes(direction string, n int64) {
	m.uploadBytesTotal.WithLabelValues(uploadWorkspaceID(), direction).Add(float64(n))
}

// RecordScrubbed counts staging-scrub reclaims through the stager's
// injected seam (design 0060 §4.6 staging_scrubbed).
func (m *opsMetrics) RecordScrubbed(files int) {
	if files > 0 {
		m.RecordUploadOutcome(uploadWorkspaceID(), uploadOutcomeStagingScrubbed)
	}
}

// RecordDestOutcome counts a supervisor-side destination outcome
// (design 0060 §4.6): rejections by code and the success-path
// dest_margin_consumed observation (from the ack's flag).
func (m *opsMetrics) RecordDestOutcome(code string) {
	m.uploadDestOutcomes.WithLabelValues(uploadWorkspaceID(), code).Add(1)
}

// RecordUploadScrub adds n to the boot-scrub removed counter.
func (m *opsMetrics) RecordUploadScrub(workspaceID string, files int) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if files > 0 {
		m.uploadScrubRemoved.WithLabelValues(workspaceID).Add(float64(files))
	}
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

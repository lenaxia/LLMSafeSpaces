// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var startupBuckets = []float64{1, 3, 5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300}

var (
	WorkspacesCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspaces_created_total", Help: "Total workspaces created"},
		[]string{"runtime", "security_level"},
	)
	WorkspacesDeletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspaces_deleted_total", Help: "Total workspaces deleted"},
		[]string{"runtime", "security_level"},
	)
	WorkspacesRunning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspaces_running", Help: "Workspaces currently in Active phase"},
		[]string{"runtime", "security_level"},
	)
	WorkspacesFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspaces_failed_total", Help: "Workspaces entering SafeMode, by failure class (incremented once per episode on the failure that trips SafeMode)"},
		[]string{"reason"},
	)
	WorkspaceRecoveryAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_recovery_attempts_total", Help: "Recovery state-machine entries by failure class"},
		[]string{"failure_class"},
	)
	WorkspaceRecoverySuccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_recovery_success_total", Help: "Recovery attempts that returned to Active"},
		[]string{"failure_class"},
	)
	WorkspaceRecoveryBackoffDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_workspace_recovery_backoff_duration_seconds",
			Help:    "Time in recovery backoff before restart attempt",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1800},
		},
		[]string{"failure_class"},
	)
	WorkspaceSafeModeActive = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspace_safe_mode_active", Help: "Count of workspaces currently in SafeMode (aggregate, no per-workspace label per F18)"},
	)
	WorkspaceSafeModeEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_safe_mode_entries_total", Help: "Total entries into SafeMode, labeled by trigger"},
		[]string{"trigger"},
	)
	WorkspaceSafeModeExitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_safe_mode_exits_total", Help: "Total exits from SafeMode, labeled by method"},
		[]string{"method"},
	)
	WorkspaceControllerRestartsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_controller_restarts_total", Help: "Pod restarts initiated by the controller's health-check loop (distinct from user-initiated RestartGeneration bumps)"},
	)
	WorkspacesInRecovery = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspaces_in_recovery", Help: "Workspaces currently in recovery backoff (ConsecutiveFailures > 0 and not Active)"},
	)
	// #863: entrypoint agentd sha256 verification failures. Should-never-
	// fire signal — a single occurrence indicates rollout drift, node
	// corruption, or tampering. Page-on-any, not threshold.
	WorkspaceAgentdVerifyFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_agentd_verify_failures_total", Help: "Entrypoint agentd binary verification failures by outcome/node/agentd digest (verify_failed = sha256 mismatch, overlay_missing = pinned binary absent). Once per failure episode, not per restart"},
		[]string{"outcome", "node", "agentd"},
	)
	// Design 0051 sidecar migration step 1: platform boot-phase failures
	// (init-fs operational failure, bootstrap/materialize non-zero exit in
	// the sidecar's boot phase or the legacy platform init containers).
	// High-signal — a persistent occurrence means a platform regression
	// (code or contract skew), never user load.
	WorkspacePlatformBootFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_platform_boot_failures_total", Help: "Platform boot-phase failures (init-fs/bootstrap/materialize in platform containers) by container/node. Once per failure episode, not per restart"},
		[]string{"container", "node"},
	)
	WorkspaceRecoveryDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_workspace_recovery_duration_seconds",
			Help:    "Wall-clock time from recovery entry (enterRecovery) to successful return to Active",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"failure_class"},
	)
	WorkspaceStatusUpdateConflictsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_workspace_status_update_conflicts_total",
			Help: "Optimistic-lock conflicts on workspace status updates, labeled by the calling site",
		},
		[]string{"site"},
	)
	WorkspaceCreateDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "llmsafespaces_workspace_create_duration_seconds", Help: "Wall-clock time from creation request to Active", Buckets: startupBuckets},
		[]string{"has_packages", "has_init_script"},
	)
	WorkspaceResumeDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "llmsafespaces_workspace_resume_duration_seconds", Help: "Wall-clock time from Resuming to Active", Buckets: startupBuckets},
		[]string{"resume_type"},
	)
	WorkspaceInitContainerDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "llmsafespaces_workspace_init_container_duration_seconds", Help: "Time in workspace-setup init container", Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300}},
	)
	ReconciliationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "llmsafespaces_reconciliation_duration_seconds", Help: "Reconciliation loop duration", Buckets: prometheus.DefBuckets},
		[]string{"resource", "status"},
	)
	ReconciliationErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_reconciliation_errors_total", Help: "Reconciliation errors"},
		[]string{"resource", "error_type"},
	)

	// METERING — per workspace, second-level precision

	WorkspaceActiveSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_active_seconds_total", Help: "Cumulative seconds in Active phase per workspace"},
		[]string{"workspace_id", "user_id", "runtime", "security_level"},
	)
	WorkspaceStorageBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspace_storage_bytes", Help: "PVC allocated bytes per workspace"},
		[]string{"workspace_id", "user_id"},
	)
	WorkspaceDiskUsedBytesSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_disk_used_bytes_seconds_total", Help: "Cumulative (disk_used_bytes x elapsed_seconds) per workspace"},
		[]string{"workspace_id", "user_id"},
	)
	WorkspaceDiskUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspace_disk_used_bytes", Help: "Current disk bytes used (gauge for alerting)"},
		[]string{"workspace_id", "user_id"},
	)
	WorkspaceMemoryUsedBytesSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_memory_used_bytes_seconds_total", Help: "Cumulative (memory_used_bytes x elapsed_seconds) per workspace"},
		[]string{"workspace_id", "user_id"},
	)
	WorkspaceMemoryUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_workspace_memory_used_bytes", Help: "Current memory bytes used (gauge for alerting)"},
		[]string{"workspace_id", "user_id"},
	)
	WorkspaceCPUMillisecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_workspace_cpu_milliseconds_total", Help: "Cumulative CPU milliseconds consumed by the workspace pod cgroup"},
		[]string{"workspace_id", "user_id"},
	)

	// BILLING — per user aggregates

	UserActiveSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_user_active_seconds_total", Help: "Cumulative active compute seconds per user"},
		[]string{"user_id", "runtime", "security_level"},
	)
	UserCPUMillisecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_user_cpu_milliseconds_total", Help: "Cumulative CPU milliseconds per user (divide by 60000 for CPU-minutes)"},
		[]string{"user_id"},
	)
	UserDiskBytesSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_user_disk_bytes_seconds_total", Help: "Cumulative (disk_used_bytes x elapsed_seconds) per user"},
		[]string{"user_id"},
	)
	UserMemoryBytesSecondsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_user_memory_bytes_seconds_total", Help: "Cumulative (memory_used_bytes x elapsed_seconds) per user"},
		[]string{"user_id"},
	)
	APIKeyLegacyTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "llmsafespaces_api_key_legacy_total", Help: "API keys using plaintext storage (pre-migration 000017, target: 0)"},
	)

	// RELAY — InferenceRelay fleet lifecycle (Epic 42)

	RelayHealthyReplicas = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "llmsafespaces_relay_healthy_replicas", Help: "count of healthy relay VMs"},
	)
	RelayProvisioningFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_relay_provisioning_failed", Help: "circuit breaker tripped (0/1)"},
		[]string{"provider"},
	)
	RelayDraining = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_relay_draining", Help: "relay in drain state (0/1)"},
		[]string{"provider"},
	)
	RelayQuotaExhausted = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "llmsafespaces_relay_quota_exhausted", Help: "egress quota exhausted (0/1)"},
		[]string{"provider"},
	)
	RelayProvisionDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_relay_provision_duration_seconds",
			Help:    "time to provision + health-check a relay",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 900, 1200},
		},
		[]string{"provider"},
	)
	RelayRotationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "llmsafespaces_relay_rotation_total", Help: "rotation events (429, failure, manual)"},
		[]string{"provider", "reason"},
	)
)

func collectors() []prometheus.Collector {
	return AllCollectors()
}

// AllCollectors returns all registered metric collectors. Exported for testing.
func AllCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		WorkspacesCreatedTotal, WorkspacesDeletedTotal, WorkspacesRunning, WorkspacesFailedTotal,
		WorkspaceRecoveryAttemptsTotal, WorkspaceRecoverySuccessTotal,
		WorkspaceRecoveryBackoffDurationSeconds,
		WorkspaceSafeModeActive, WorkspaceSafeModeEntriesTotal, WorkspaceSafeModeExitsTotal,
		WorkspaceControllerRestartsTotal, WorkspacesInRecovery,
		WorkspaceAgentdVerifyFailuresTotal,
		WorkspacePlatformBootFailuresTotal,
		WorkspaceRecoveryDurationSeconds,
		WorkspaceStatusUpdateConflictsTotal,
		WorkspaceCreateDurationSeconds, WorkspaceResumeDurationSeconds,
		WorkspaceInitContainerDurationSeconds,
		ReconciliationDurationSeconds, ReconciliationErrorsTotal,
		WorkspaceActiveSecondsTotal, WorkspaceCPUMillisecondsTotal,
		WorkspaceStorageBytes, WorkspaceDiskUsedBytesSecondsTotal, WorkspaceDiskUsedBytes,
		WorkspaceMemoryUsedBytesSecondsTotal, WorkspaceMemoryUsedBytes,
		UserActiveSecondsTotal, UserCPUMillisecondsTotal,
		UserDiskBytesSecondsTotal, UserMemoryBytesSecondsTotal,
		APIKeyLegacyTotal,
		RelayHealthyReplicas, RelayProvisioningFailed, RelayDraining,
		RelayQuotaExhausted, RelayProvisionDurationSeconds, RelayRotationTotal,
	}
}

func SetupMetrics() {
	for _, c := range collectors() {
		prometheus.MustRegister(c)
	}
}

func RegisterWith(reg prometheus.Registerer) error {
	for _, c := range collectors() {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// SeedWorkspacesRunning resets the WorkspacesRunning gauge to match
// the provided count of currently-active workspaces. Called once at
// controller startup after the informer cache syncs, so the gauge
// reflects reality even though existing Active workspaces never
// trigger the Creating→Active transition that normally calls .Inc().
func SeedWorkspacesRunning(runtime, secLevel string, count int) {
	WorkspacesRunning.WithLabelValues(runtime, secLevel).Set(float64(count))
}

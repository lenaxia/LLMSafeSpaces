// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// observeReconcileDurationInto records a single reconcile loop duration.
// Injected histogram enables isolated unit tests.
func observeReconcileDurationInto(hist *prometheus.HistogramVec, resource, status string, d time.Duration) {
	hist.WithLabelValues(resource, status).Observe(d.Seconds())
}

// observeReconcileDuration records into the package-level metric.
func observeReconcileDuration(resource, status string, d time.Duration) {
	observeReconcileDurationInto(metrics.ReconciliationDurationSeconds, resource, status, d)
}

// countReconcileErrorInto increments a reconciliation error counter.
func countReconcileErrorInto(ctr *prometheus.CounterVec, resource, errorType string) {
	ctr.WithLabelValues(resource, errorType).Inc()
}

// countReconcileError increments into the package-level metric.
func countReconcileError(resource, errorType string) {
	countReconcileErrorInto(metrics.ReconciliationErrorsTotal, resource, errorType)
}

// incrementWorkspacesDeletedInto increments the deleted counter.
func incrementWorkspacesDeletedInto(ctr *prometheus.CounterVec, ws *v1.Workspace) {
	ctr.WithLabelValues(ws.Spec.Runtime, ws.Spec.SecurityLevel).Inc()
}

// incrementWorkspacesDeleted increments into the package-level metric.
func incrementWorkspacesDeleted(ws *v1.Workspace) {
	incrementWorkspacesDeletedInto(metrics.WorkspacesDeletedTotal, ws)
}

// incrementPVCCleanupDelegatedInto increments the delegated-PVC-cleanup
// counter (#772), labeled by the API error reason (conflict, forbidden, …).
// Non-status errors map to "unknown".
func incrementPVCCleanupDelegatedInto(ctr *prometheus.CounterVec, err error) {
	reason := string(apierrors.ReasonForError(err))
	if reason == "" {
		reason = "unknown"
	}
	ctr.WithLabelValues(reason).Inc()
}

// incrementPVCCleanupDelegated increments into the package-level metric.
func incrementPVCCleanupDelegated(err error) {
	incrementPVCCleanupDelegatedInto(metrics.WorkspacePVCCleanupDelegatedTotal, err)
}

// recordRecoveryMetricsInto records recovery-related metrics after enterRecovery
// updates workspace status. Called with the post-increment ConsecutiveFailures and
// the resolved NextRetryAt / SafeMode values.
func recordRecoveryMetricsInto(
	ws *v1.Workspace,
	class FailureClass,
	wasInRecovery bool,
	wasInSafeMode bool,
	attempts *prometheus.CounterVec,
	backoffHist *prometheus.HistogramVec,
	safeModeGauge prometheus.Gauge,
	safeModeEntries *prometheus.CounterVec,
	failedCtr *prometheus.CounterVec,
	inRecoveryGauge prometheus.Gauge,
) {
	attempts.WithLabelValues(string(class)).Inc()

	// Only Inc the gauge on the first entry into recovery for this episode.
	// A workspace that fails N times only Dec's once on recovery success,
	// so N Inc's would drift the gauge by N-1.
	if !wasInRecovery {
		inRecoveryGauge.Inc()
	}

	if ws.Status.NextRetryAt != nil {
		backoff := time.Until(ws.Status.NextRetryAt.Time)
		if backoff < 0 {
			backoff = 0
		}
		backoffHist.WithLabelValues(string(class)).Observe(backoff.Seconds())
	}

	// Only Inc safe-mode gauge + entries on the false→true transition.
	if ws.Status.SafeMode && !wasInSafeMode {
		safeModeGauge.Inc()
		safeModeEntries.WithLabelValues(string(class)).Inc()
		failedCtr.WithLabelValues(string(class)).Inc()
	}
}

func recordRecoveryMetrics(ws *v1.Workspace, class FailureClass, wasInRecovery, wasInSafeMode bool) {
	recordRecoveryMetricsInto(
		ws, class, wasInRecovery, wasInSafeMode,
		metrics.WorkspaceRecoveryAttemptsTotal,
		metrics.WorkspaceRecoveryBackoffDurationSeconds,
		metrics.WorkspaceSafeModeActive,
		metrics.WorkspaceSafeModeEntriesTotal,
		metrics.WorkspacesFailedTotal,
		metrics.WorkspacesInRecovery,
	)
}

// accumulateActiveSecondsInto adds elapsed active time to per-workspace and
// per-user counters. elapsed must be > 0 and ws.Status.StartTime must be set.
func accumulateActiveSecondsInto(
	ws *v1.Workspace,
	elapsed time.Duration,
	wsActive *prometheus.CounterVec,
	userActive *prometheus.CounterVec,
) {
	if elapsed <= 0 || ws.Status.StartTime == nil {
		return
	}
	userID := ws.Labels["user-id"]
	secs := elapsed.Seconds()
	wsActive.WithLabelValues(ws.Name, userID, ws.Spec.Runtime, ws.Spec.SecurityLevel).Add(secs)
	userActive.WithLabelValues(userID, ws.Spec.Runtime, ws.Spec.SecurityLevel).Add(secs)
}

// accumulateActiveSeconds accumulates into package-level metrics.
func accumulateActiveSeconds(ws *v1.Workspace, elapsed time.Duration) {
	accumulateActiveSecondsInto(ws, elapsed, metrics.WorkspaceActiveSecondsTotal, metrics.UserActiveSecondsTotal)
}

// setStorageBytesInto sets the PVC-allocated storage gauge for a workspace.
// Parses ws.Spec.Storage.Size; silently no-ops on empty or unparseable size.
func setStorageBytesInto(ws *v1.Workspace, storageVec *prometheus.GaugeVec) {
	if ws.Spec.Storage.Size == "" {
		return
	}
	q, err := resource.ParseQuantity(ws.Spec.Storage.Size)
	if err != nil {
		return
	}
	userID := ws.Labels["user-id"]
	storageVec.WithLabelValues(ws.Name, userID).Set(float64(q.Value()))
}

// setStorageBytes sets into the package-level metric.
func setStorageBytes(ws *v1.Workspace) {
	setStorageBytesInto(ws, metrics.WorkspaceStorageBytes)
}

// recordStatusUpdateConflictInto increments the per-site conflict counter on
// the injected CounterVec. Site is the calling function name (e.g.
// "phase_active", "phase_suspend") — used to identify which controller code
// path is hitting conflicts most often. Epic 23 ships the metric so the
// >10-conflicts/day deferral condition for US-23.3 (single-writer migration)
// becomes observable; the retry helper itself is US-23.2.
//
// Nil ctr is a no-op (production callers always pass the package-level metric,
// bound at import time; the nil-guard lets unit tests skip setup).
func recordStatusUpdateConflictInto(ctr *prometheus.CounterVec, site string) {
	if ctr == nil {
		return
	}
	ctr.WithLabelValues(site).Inc()
}

// recordStatusUpdateConflictOnErrorInto inspects err and increments the
// per-site conflict counter only when err satisfies apierrors.IsConflict.
// Non-conflict errors (Forbidden, NotFound, internal) are silently ignored —
// the metric is exclusively about optimistic-lock conflicts.
//
// This is the helper used inline at the 21 r.Status().Update() call sites.
// The LastActivityAt annotation migration (US-23.3) moved activity writes
// out of Status, so the controller no longer writes LastActivityAt.
func recordStatusUpdateConflictOnErrorInto(ctr *prometheus.CounterVec, site string, err error) {
	if ctr == nil || err == nil {
		return
	}
	if apierrors.IsConflict(err) {
		ctr.WithLabelValues(site).Inc()
	}
}

// recordStatusUpdateConflictOnError records into the package-level metric.
func recordStatusUpdateConflictOnError(site string, err error) {
	recordStatusUpdateConflictOnErrorInto(metrics.WorkspaceStatusUpdateConflictsTotal, site, err)
}

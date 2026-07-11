package workspace

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lenaxia/llmsafespaces/controller/internal/common"
	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) handleTerminating(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	uid := string(workspace.UID)
	name := podName(workspace.Name, uid)

	// Capture active state BEFORE the status update. PodIP != "" is the proxy
	// for "this workspace is counted in WorkspacesRunning". We Dec only AFTER a
	// successful Status().Update so that an update failure does not leave the
	// gauge decremented while the workspace remains Active — which would cause a
	// double-decrement on the next reconcile attempt.
	wasActive := workspace.Status.PodIP != ""

	// Delete pod.
	r.deletePodByName(ctx, name, workspace.Namespace)

	// Delete PVC.
	if workspace.Status.PVCName != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		pvc.Name = workspace.Status.PVCName
		pvc.Namespace = workspace.Namespace
		if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Delete password secret.
	pwSecret := &corev1.Secret{}
	pwSecret.Name = passwordSecretName(workspace.Name)
	pwSecret.Namespace = workspace.Namespace
	if err := r.Delete(ctx, pwSecret); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// G36: delete the rest of the per-workspace ephemeral Secrets
	// (workspace-creds-* and any future additions to the cleanup list).
	// The explicit password-secret delete above handles workspace-pw-*;
	// cleanupFailedWorkspaceSecrets (secrets.go:33) re-attempts that
	// delete (idempotent) and additionally removes workspace-creds-*,
	// which previously persisted indefinitely after workspace deletion.
	// Best-effort: failures are logged, not propagated — the workspace
	// is already being torn down and the finalizer must still release.
	// Mirrors the Failed-phase cleanup pattern (recovery.go:31,60,112).
	r.cleanupFailedWorkspaceSecrets(ctx, workspace)

	workspace.Status.Phase = v1.WorkspacePhaseTerminated

	// Record deletion metric.
	incrementWorkspacesDeleted(workspace)

	// Clean up in-memory state for this workspace.
	r.lastDeepStatusMu.Lock()
	delete(r.lastDeepStatus, workspace.Name)
	r.lastDeepStatusMu.Unlock()
	workspace.Status.PodName = ""
	workspace.Status.PodIP = ""
	workspace.Status.Endpoint = ""
	workspace.Status.Sessions = nil
	workspace.Status.ActiveSessions = 0
	workspace.Status.DiskUsedBytes = 0
	workspace.Status.DiskTotalBytes = 0
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handleTerminating_clear_status", err)
		return ctrl.Result{}, err
	}

	if wasActive {
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
	}

	if workspace.Status.SafeMode {
		metrics.WorkspaceSafeModeActive.Dec()
		metrics.WorkspaceSafeModeExitsTotal.WithLabelValues("termination").Inc()
	}

	common.RemoveFinalizer(workspace, WorkspaceFinalizer)
	return ctrl.Result{}, r.Update(ctx, workspace)
}

func (r *WorkspaceReconciler) handleDeletion(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workspace, WorkspaceFinalizer) {
		return ctrl.Result{}, nil
	}
	// Reuse terminating logic.
	workspace.Status.Phase = v1.WorkspacePhaseTerminating
	return r.handleTerminating(ctx, workspace)
}

// --- Transient recovery ---

package workspace

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// Delete PVC — best-effort (#772). The PVC carries a controller owner
	// reference to the Workspace (SetControllerReference in handlePending),
	// so Kubernetes GC deletes it once the Workspace object is removed.
	// Propagating a persistent delete error (RBAC denial, or a PVC whose
	// own CSI finalizer returns conflicts on every attempt — the Longhorn
	// node-loss case) used to wedge the workspace in Terminating forever:
	// the early return meant the finalizer below was never removed. On
	// error we surface the delegated cleanup (warning log + Event +
	// metric) and continue — the finalizer removal unblocks GC, which owns
	// the residual. A PVC with a stuck finalizer still blocks PHYSICAL
	// volume reclaim until that finalizer clears; the Event makes that
	// residual explicit. NotFound stays silent (already gone = done).
	if workspace.Status.PVCName != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		pvc.Name = workspace.Status.PVCName
		pvc.Namespace = workspace.Namespace
		if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
			r.reportPVCCleanupDelegated(ctx, workspace, err)
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

// reportPVCCleanupDelegated surfaces a failed explicit PVC delete during
// termination (#772). The workspace still finalizes — the PVC's controller
// owner reference means GC owns the residual cleanup — but the operator
// must know: until the PVC's own finalizers clear (stuck CSI finalizer,
// RBAC-denied delete), the physical volume is NOT reclaimed.
func (r *WorkspaceReconciler) reportPVCCleanupDelegated(ctx context.Context, workspace *v1.Workspace, deleteErr error) {
	pvcName := workspace.Status.PVCName
	log.FromContext(ctx).Error(deleteErr, "PVC delete failed during termination — removal delegated to owner-reference garbage collection; "+
		"physical volume reclaim can stay blocked if the PVC's own finalizers are stuck",
		"pvc", pvcName)
	incrementPVCCleanupDelegated(deleteErr)
	if r.Recorder != nil {
		r.Recorder.Eventf(workspace, corev1.EventTypeWarning, string(v1.ReasonPVCCleanupDelegated),
			"PVC %s delete failed (%v); removal delegated to owner-reference garbage collection — "+
				"physical volume reclaim can stay blocked if the PVC's own finalizers are stuck", pvcName, deleteErr)
	}
}

// --- Transient recovery ---

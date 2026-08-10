package workspace

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/common"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) handlePending(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if common.AddFinalizer(workspace, WorkspaceFinalizer) {
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Issue #699: honor Spec.Suspend=true in Pending. Less urgent than
	// Creating (typically no pod yet, so no mkfs loop), but the per-
	// workspace escape hatch should be uniform across all pre-Suspended
	// phases. Goes after the finalizer (so cleanup runs on delete) but
	// before PVC/pod creation (so we don't allocate resources we're
	// about to park).
	if workspace.Spec.Suspend != nil && *workspace.Spec.Suspend {
		logger.Info("Spec.Suspend=true in Pending; transitioning to Suspended")
		return r.suspendFromPreActive(ctx, workspace)
	}

	// Ensure PVC.
	pvcName := fmt.Sprintf("workspace-%s", workspace.Name)
	existingPVC := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: workspace.Namespace}, existingPVC)

	if err == nil {
		if r.isPVCStale(existingPVC, workspace) {
			logger.Info("Deleting stale PVC", "pvc", pvcName, "reason", "owner mismatch or terminating")
			if delErr := r.Delete(ctx, existingPVC); delErr != nil {
				return ctrl.Result{}, delErr
			}
			err = errors.NewNotFound(corev1.Resource("persistentvolumeclaims"), pvcName)
		}
	}

	if err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// If we're in backoff from a prior PVC creation failure, wait.
		if wait := timeUntilNextRetry(workspace); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		workspace.Status.NextRetryAt = nil

		newPVC := r.buildPVC(workspace, pvcName)
		if err := controllerutil.SetControllerReference(workspace, newPVC, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newPVC); err != nil {
			if errors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: requeueCreating}, nil
			}
			return ctrl.Result{}, err
		}
		workspace.Status.PVCName = pvcName
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handlePending_pvc_created", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueCreating}, nil
	}

	// PVC exists — check if bound.
	if existingPVC.Status.Phase != corev1.ClaimBound {
		if r.pvcUsesWaitForFirstConsumer(ctx, existingPVC) {
			// WaitForFirstConsumer: PVC won't bind until pod mounts it.
			// Transition to Creating so pod gets created.
			workspace.Status.PVCName = pvcName
			workspace.Status.Phase = v1.WorkspacePhaseCreating
			if err := r.Status().Update(ctx, workspace); err != nil {
				recordStatusUpdateConflictOnError("handlePending_wait_for_first_consumer", err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		if r.pendingTimedOut(workspace) {
			return r.enterRecovery(ctx, workspace, FailureClassInfrastructure)
		}
		return ctrl.Result{RequeueAfter: requeueActive}, nil
	}

	// PVC bound — ensure password secret, then transition to Creating.
	if err := r.ensurePasswordSecret(ctx, workspace); err != nil {
		logger.Error(err, "Failed to ensure password secret")
		return ctrl.Result{}, err
	}
	// Epic 35 US-35.1: ensure per-workspace ServiceAccount for secretless
	// credential injection. The init container uses its projected token to
	// fetch credentials from the API at boot. Also ensured defensively in
	// handleCreating for the resume path.
	if err := r.ensureWorkspaceServiceAccount(ctx, workspace); err != nil {
		logger.Error(err, "Failed to ensure workspace ServiceAccount")
		return ctrl.Result{}, err
	}

	// Set the PendingAt anchor on first entry so the controller can measure
	// end-to-end create latency. Prefer the AnnotationRequestedAt written by
	// the API (user-perceived start time); fall back to now (controller-first-
	// reconcile) if the annotation is absent or unparseable.
	if workspace.Status.PendingAt == nil {
		anchor := metav1.Now()
		if raw, ok := workspace.Annotations[v1.AnnotationRequestedAt]; ok {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				anchor = metav1.NewTime(t)
			}
		}
		workspace.Status.PendingAt = &anchor
	}

	workspace.Status.PVCName = pvcName
	workspace.Status.Phase = v1.WorkspacePhaseCreating
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handlePending_pvc_bound", err)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

package workspace

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
)

// handleFailed handles workspaces that are in the legacy Failed phase.
// With Epic 24's recovery system, no new workspaces should enter this phase.
// This handler exists to self-heal any workspace that was in Failed before
// the recovery system was deployed (or from a future bug).
func (r *WorkspaceReconciler) handleFailed(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Still respect declarative restartGeneration bumps.
	if workspace.Spec.RestartGeneration > workspace.Status.ObservedRestartGeneration {
		logger.Info("RestartGeneration bumped on Failed workspace; transitioning to Pending",
			"gen", workspace.Spec.RestartGeneration,
			"observed", workspace.Status.ObservedRestartGeneration)
		r.cleanupFailedWorkspaceSecrets(ctx, workspace)
		workspace.Status.Phase = v1.WorkspacePhasePending
		workspace.Status.Message = ""
		workspace.Status.FailureReason = v1.FailureReasonNone
		workspace.Status.PodName = ""
		workspace.Status.PodNamespace = ""
		workspace.Status.PodIP = ""
		workspace.Status.Endpoint = ""
		clearRecoveryState(workspace)
		workspace.Status.RestartCount++
		workspace.Status.ObservedRestartGeneration = workspace.Spec.RestartGeneration
		// #1505: clear a refresh-stamped force marker at the observe
		// site (Failed recovery recycles nothing — no bypass applies),
		// BEFORE the status update so the clear's RV-sync holds for it.
		if _, forced := workspace.Annotations[v1.AnnotationForceRecycle]; forced {
			if err := r.clearForceRecycleAnnotation(ctx, workspace); err != nil {
				logger.Error(err, "failed to clear force-recycle annotation; requeueing")
				return ctrl.Result{Requeue: true}, nil
			}
		}
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleFailed_gen_bump", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	uid := string(workspace.UID)
	name := podName(workspace.Name, uid)

	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspace.Namespace}, pod)
	if err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		logger.Info("Workspace Failed; no pod exists, retrying")
		r.cleanupFailedWorkspaceSecrets(ctx, workspace)
		workspace.Status.Phase = v1.WorkspacePhaseCreating
		workspace.Status.PodIP = ""
		workspace.Status.Endpoint = ""
		clearRecoveryState(workspace)
		workspace.Status.Message = ""
		workspace.Status.FailureReason = v1.FailureReasonNone
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleFailed_no_pod", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready {
			logger.Info("Workspace Failed but pod is Running/Ready; self-healing to Active")
			runtime := workspace.Spec.Runtime
			secLevel := string(workspace.Spec.SecurityLevel)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
			now := metav1.Now()
			workspace.Status.Phase = v1.WorkspacePhaseActive
			workspace.Status.PodName = pod.Name
			workspace.Status.PodNamespace = pod.Namespace
			workspace.Status.PodIP = pod.Status.PodIP
			workspace.Status.Endpoint = fmt.Sprintf("http://%s:4096", pod.Status.PodIP)
			workspace.Status.ImageTag = imageTagFromPod(pod)
			workspace.Status.StartTime = &now
			clearRecoveryState(workspace)
			workspace.Status.ConsecutiveHealthFailures = 0
			workspace.Status.Message = ""
			workspace.Status.FailureReason = v1.FailureReasonNone
			if err := r.Status().Update(ctx, workspace); err != nil {
				recordStatusUpdateConflictOnError("handleFailed_self_heal", err)
				metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	logger.Info("Workspace Failed; pod not healthy, deleting and retrying", "podPhase", pod.Status.Phase)
	r.deletePodByName(ctx, name, workspace.Namespace)
	r.cleanupFailedWorkspaceSecrets(ctx, workspace)
	workspace.Status.Phase = v1.WorkspacePhaseCreating
	workspace.Status.PodIP = ""
	workspace.Status.Endpoint = ""
	clearRecoveryState(workspace)
	workspace.Status.Message = ""
	workspace.Status.FailureReason = v1.FailureReasonNone
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handleFailed_retry", err)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// --- Pod management helpers ---

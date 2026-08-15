package workspace

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
)

func (r *WorkspaceReconciler) handleActive(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	uid := string(workspace.UID)
	name := podName(workspace.Name, uid)

	// Refresh per-workspace egress NetPol on every Active reconcile so
	// (a) spec.networkAccess.egress changes take effect without a pod
	// restart, (b) DNS-resolved /32 ipBlocks self-refresh as CDN IPs
	// rotate, and (c) toggling NetworkAccess off cleanly deletes the
	// policy. Failure is non-fatal — log and continue. (F1.2.4 / G4
	// part 2 — validator pass 2 catch.)
	if err := r.ensureWorkspaceEgressNetworkPolicy(ctx, workspace); err != nil {
		logger.Error(err, "Failed to refresh per-workspace egress NetworkPolicy (continuing)")
	}

	// US-23.3: if the API set Spec.Suspend=true, transition to Suspending.
	// The controller is the sole writer of Status.Phase. After the status
	// transition commits, clear Spec.Suspend to acknowledge the request.
	// Order matters: Status().Update must come FIRST (using the cache's
	// resourceVersion), then clearSuspendRequest fetches its own fresh
	// copy. Reversing them would bump the RV via Update, then the
	// Status().Update would 409 on the stale local RV and the request
	// would be permanently lost on re-reconcile (Spec.Suspend already nil).
	if workspace.Spec.Suspend != nil && *workspace.Spec.Suspend {
		logger.Info("Spec.Suspend=true; transitioning to Suspending")
		if err := r.transitionActiveToSuspending(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.clearSuspendRequest(ctx, workspace); err != nil {
			logger.Error(err, "Failed to clear Spec.Suspend after suspend transition; will retry on next reconcile")
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, nil
	}

	// D20 (US-43.19): org-level suspension. If the workspace belongs to a
	// suspended org, transition to Suspending so the pod is killed (PVC
	// retained). The controller never auto-resumes. A status-lookup failure
	// fails open (workspace keeps running); the cached client absorbs
	// transient API outages. This check runs AFTER the Spec.Suspend path so
	// an explicit API suspend is always honored and cleared first.
	if transitioned, err := r.applyOrgSuspension(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	} else if transitioned {
		return ctrl.Result{}, nil
	}

	// Check restart generation.
	if workspace.Spec.RestartGeneration > workspace.Status.ObservedRestartGeneration {
		logger.Info("Restart generation bumped; deleting pod", "gen", workspace.Spec.RestartGeneration)
		r.deletePodByName(ctx, name, workspace.Namespace)
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
		workspace.Status.Phase = v1.WorkspacePhaseCreating
		workspace.Status.PodIP = ""
		workspace.Status.Endpoint = ""
		workspace.Status.RestartCount++
		workspace.Status.ObservedRestartGeneration = workspace.Spec.RestartGeneration
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleActive_restart_gen", err)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure password secret still exists (can be lost during crash cycles
	// or cleanup). If missing, recycle pod so handleCreating regenerates it.
	pwSec := &corev1.Secret{}
	if pwErr := r.Get(ctx, types.NamespacedName{Name: passwordSecretName(workspace.Name), Namespace: workspace.Namespace}, pwSec); pwErr != nil {
		if errors.IsNotFound(pwErr) {
			logger.Info("Password secret missing in Active phase; recycling pod to regenerate", "secret", passwordSecretName(workspace.Name))
			r.deletePodByName(ctx, name, workspace.Namespace)
			runtime := workspace.Spec.Runtime
			secLevel := string(workspace.Spec.SecurityLevel)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
			workspace.Status.Phase = v1.WorkspacePhaseCreating
			workspace.Status.PodIP = ""
			workspace.Status.Endpoint = ""
			// US-24.7 counter semantics: ControllerRestartCount is incremented
			// by the health-check loop only. This password-secret self-heal is
			// a different recovery path, so it bumps RestartCount (total pod
			// restarts) but deliberately NOT ControllerRestartCount.
			workspace.Status.RestartCount++
			if err := r.Status().Update(ctx, workspace); err != nil {
				recordStatusUpdateConflictOnError("handleActive_pw_missing", err)
				metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Check pod exists and is running.
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspace.Namespace}, pod)
	if err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
		result, err := r.enterRecovery(ctx, workspace, FailureClassInfrastructure)
		if err != nil {
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
		}
		return result, err
	}

	// US-23.1: A pod with DeletionTimestamp set is being terminated by the
	// controller itself (e.g., checkAgentHealth deleted it). Transition to
	// Creating so a new pod is built once the old one is reaped. Do NOT
	// count this as a transient failure — the controller initiated the delete.
	if isPodTerminating(pod) {
		logger.V(1).Info("Pod is terminating in Active phase; transitioning to Creating", "pod", pod.Name)
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
		workspace.Status.Phase = v1.WorkspacePhaseCreating
		workspace.Status.PodIP = ""
		workspace.Status.Endpoint = ""
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleActive_pod_terminating", err)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueCreating}, nil
	}

	if pod.Status.Phase != corev1.PodRunning {
		obs := observePod(pod)
		class := classifyFailure(obs)
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
		result, err := r.enterRecovery(ctx, workspace, class)
		if err != nil {
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
		}
		return result, err
	}

	// Detect architecture drift: if the running pod's nodeSelector doesn't
	// match the desired architecture, delete the pod so it gets recreated
	// on a node with the correct arch. Skip if pod has no nodeSelector
	// (legacy pod created before multi-arch support).
	desiredArch := workspace.Spec.Architecture
	if desiredArch == "" {
		desiredArch = "amd64"
	}
	if pod.Spec.NodeSelector != nil && pod.Spec.NodeSelector["kubernetes.io/arch"] != desiredArch {
		logger.Info("Architecture changed; recreating pod", "desired", desiredArch, "current", pod.Spec.NodeSelector["kubernetes.io/arch"])
		r.deletePodByName(ctx, name, workspace.Namespace)
		runtime := workspace.Spec.Runtime
		secLevel := string(workspace.Spec.SecurityLevel)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
		workspace.Status.Phase = v1.WorkspacePhaseCreating
		workspace.Status.PodIP = ""
		workspace.Status.Endpoint = ""
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleActive_arch_drift", err)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// #863: agentd integrity check runs BEFORE the crashloop recovery
	// branch. A verify failure (exit 81/82) cannot be fixed by restarting
	// the pod — the same digest pin reproduces the same mismatch — so
	// entering recovery would only burn the restart budget. Surface the
	// condition/event/metric and requeue slowly for the operator.
	if r.detectAgentdVerificationFailure(ctx, workspace, pod) {
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleActive_agentd_verify", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: agentdVerifyFailureRequeue}, nil
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			obs := observePod(pod)
			class := classifyFailure(obs)
			runtime := workspace.Spec.Runtime
			secLevel := string(workspace.Spec.SecurityLevel)
			metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
			result, err := r.enterRecovery(ctx, workspace, class)
			if err != nil {
				metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
			}
			return result, err
		}
	}

	// #863: overlay-mode pod observed running clean — publish the
	// positive verification condition (idempotent).
	r.markAgentdVerified(workspace)

	// Pod running — check timeout.
	if workspace.Spec.Timeout > 0 && workspace.Status.StartTime != nil {
		elapsed := time.Since(workspace.Status.StartTime.Time)
		if elapsed > time.Duration(workspace.Spec.Timeout)*time.Second {
			logger.Info("Pod timeout exceeded; suspending")
			workspace.Status.Phase = v1.WorkspacePhaseSuspending
			if err := r.Status().Update(ctx, workspace); err != nil {
				recordStatusUpdateConflictOnError("handleActive_timeout", err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if workspace.Status.PodIP != "" && workspace.Status.StartTime != nil {
		if workspace.Status.LastHealthCheckAt != nil && workspace.Status.LastHealthCheckAt.Before(workspace.Status.StartTime) {
			workspace.Status.ConsecutiveHealthFailures = 0
			workspace.Status.LastHealthCheckAt = nil
		}
	}

	// Check idle auto-suspend.
	// US-23.3: read LastActivityAt from the annotation (authoritative)
	// with fallback to the deprecated Status field for workspaces
	// created before the migration.
	if workspace.Spec.AutoSuspend != nil && workspace.Spec.AutoSuspend.Enabled {
		timeout := workspace.Spec.AutoSuspend.IdleTimeoutSeconds
		if timeout <= 0 {
			timeout = 86400
		}
		lastActivity := v1.GetLastActivityAt(workspace)
		if lastActivity != nil {
			idle := time.Since(lastActivity.Time)
			if idle > time.Duration(timeout)*time.Second {
				logger.Info("Workspace idle timeout exceeded; suspending",
					"lastActivity", lastActivity, "idle", idle, "timeout", time.Duration(timeout)*time.Second)
				workspace.Status.Phase = v1.WorkspacePhaseSuspending
				if err := r.Status().Update(ctx, workspace); err != nil {
					recordStatusUpdateConflictOnError("handleActive_idle", err)
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
	}

	// Reset failure counter if stable long enough (2 min).
	maybeResetConsecutiveFailures(workspace)
	// Accumulate active compute seconds for billing metrics (requeueActive is the elapsed window).
	accumulateActiveSeconds(workspace, requeueActive)
	// Set PVC-allocated storage gauge (idempotent set; cheap).
	setStorageBytes(workspace)
	phaseBefore := workspace.Status.Phase
	r.checkAgentHealth(ctx, workspace)
	r.maybeEnrichAgentStatus(ctx, workspace)

	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handleActive_health", err)
		if phaseBefore == v1.WorkspacePhaseActive && workspace.Status.Phase == v1.WorkspacePhaseCreating {
			metrics.WorkspacesRunning.WithLabelValues(workspace.Spec.Runtime, string(workspace.Spec.SecurityLevel)).Inc()
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueActive}, nil
}

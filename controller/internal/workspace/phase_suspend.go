package workspace

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// suspendFromPreActive honors a Spec.Suspend=true request on a workspace in
// the Pending or Creating phase. These phases have no running agent to drain,
// so they bypass the Suspending phase and transition directly to Suspended.
// The PVC (if any) is retained; any existing pod is deleted to halt CSI
// volume retries (the data-loss amplifier documented in issue #699).
//
// Unlike handleSuspending, this path does NOT decrement WorkspacesRunning —
// that gauge is only incremented on the Creating→Active transition, so a
// pre-Active workspace has never incremented it.
//
// Ordering follows clearSuspendRequest's contract: Status().Update commits
// the phase transition first, then clearSuspendRequest clears the spec
// field using a fresh fetch (so the stale resourceVersion from the status
// write does not cause a 409 on the spec write).
func (r *WorkspaceReconciler) suspendFromPreActive(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Delete any existing pod. In Creating this detaches the volume and
	// halts kubelet's NodeStageVolume retries (and CSI's mkfs retries on
	// the PVC). In Pending there is typically no pod; the delete is a
	// no-op. Computes the pod name from workspace name + UID rather than
	// trusting Status.PodName, which may not be set yet or may be stale
	// (same convention as handleSuspending).
	name := podName(workspace.Name, string(workspace.UID))
	r.deletePodByName(ctx, name, workspace.Namespace)

	// US-24.8 F22 parity: suspend clears recovery state for a fresh start
	// on resume. SafeMode is intentionally preserved so handleSuspended
	// can disable TTL for safe-mode workspaces.
	wasInRecovery := workspace.Status.ConsecutiveFailures > 0
	if wasInRecovery {
		metrics.WorkspacesInRecovery.Dec()
	}

	now := metav1.Now()
	priorPhase := workspace.Status.Phase
	workspacePhaseTransitions.WithLabelValues(string(priorPhase), string(v1.WorkspacePhaseSuspended)).Inc()
	workspace.Status.Phase = v1.WorkspacePhaseSuspended
	workspace.Status.PodName = ""
	workspace.Status.PodNamespace = ""
	workspace.Status.PodIP = ""
	workspace.Status.Endpoint = ""
	workspace.Status.SuspendedAt = &now
	workspace.Status.ConsecutiveFailures = 0
	workspace.Status.ControllerRestartCount = 0
	workspace.Status.NextRetryAt = nil
	workspace.Status.LastFailureClass = ""
	workspace.Status.LastFailureAt = nil
	workspace.Status.LastStableAt = nil
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("suspendFromPreActive_suspended", err)
		if wasInRecovery {
			metrics.WorkspacesInRecovery.Inc()
		}
		return ctrl.Result{}, err
	}

	logger.Info("Suspended from pre-Active phase via Spec.Suspend request",
		"priorPhase", priorPhase)

	// Acknowledge the suspend request by clearing Spec.Suspend. Must come
	// after the status update (see clearSuspendRequest doc).
	if err := r.clearSuspendRequest(ctx, workspace); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) handleSuspending(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	uid := string(workspace.UID)
	name := podName(workspace.Name, uid)

	r.deletePodByName(ctx, name, workspace.Namespace)

	// US-24.8 F22: suspend clears recovery state for a fresh start on resume.
	// SafeMode is intentionally preserved (US-24.13 AC 9) so handleSuspended
	// can disable TTL for safe-mode workspaces.
	wasInRecovery := workspace.Status.ConsecutiveFailures > 0
	if wasInRecovery {
		metrics.WorkspacesInRecovery.Dec()
	}

	now := metav1.Now()
	workspacePhaseTransitions.WithLabelValues(string(v1.WorkspacePhaseSuspending), string(v1.WorkspacePhaseSuspended)).Inc()
	runtime := workspace.Spec.Runtime
	secLevel := string(workspace.Spec.SecurityLevel)
	metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
	workspace.Status.Phase = v1.WorkspacePhaseSuspended
	workspace.Status.PodName = ""
	workspace.Status.PodNamespace = ""
	workspace.Status.PodIP = ""
	workspace.Status.Endpoint = ""
	workspace.Status.SuspendedAt = &now
	workspace.Status.ConsecutiveFailures = 0
	workspace.Status.ControllerRestartCount = 0
	workspace.Status.NextRetryAt = nil
	workspace.Status.LastFailureClass = ""
	workspace.Status.LastFailureAt = nil
	workspace.Status.LastStableAt = nil
	workspace.Status.Sessions = nil
	workspace.Status.ActiveSessions = 0
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handleSuspending_suspended", err)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
		if wasInRecovery {
			metrics.WorkspacesInRecovery.Inc()
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) handleSuspended(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	// US-23.3: if the API explicitly set Spec.Suspend=false (pointer
	// non-nil, value false), transition to Resuming and clear the request.
	// The controller MUST clear Spec.Suspend after consuming it, otherwise
	// a stale &false would cause handleSuspended to immediately resume
	// after any controller-initiated suspend (idle/timeout/TTL), creating
	// an infinite suspend/resume loop.
	// A nil pointer means "field not set" or "request already acknowledged"
	// — treat as "no resume requested."
	//
	// Order matters: Status().Update must come FIRST (using the cache's
	// resourceVersion), then clearSuspendRequest fetches its own fresh
	// copy. Reversing them would bump the RV via Update, then the
	// Status().Update would 409 on the stale local RV and the request
	// would be permanently lost on re-reconcile.
	if workspace.Spec.Suspend != nil && !*workspace.Spec.Suspend {
		workspacePhaseTransitions.WithLabelValues(string(v1.WorkspacePhaseSuspended), string(v1.WorkspacePhaseResuming)).Inc()
		workspace.Status.Phase = v1.WorkspacePhaseResuming
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleSuspended_resume", err)
			return ctrl.Result{}, err
		}
		if err := r.clearSuspendRequest(ctx, workspace); err != nil {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, nil
	}

	if workspace.Spec.TTLSecondsAfterSuspended <= 0 || workspace.Status.SuspendedAt == nil {
		return ctrl.Result{}, nil
	}
	elapsed := time.Since(workspace.Status.SuspendedAt.Time)
	ttl := time.Duration(workspace.Spec.TTLSecondsAfterSuspended) * time.Second
	if elapsed >= ttl {
		workspacePhaseTransitions.WithLabelValues(string(v1.WorkspacePhaseSuspended), string(v1.WorkspacePhaseTerminating)).Inc()
		workspace.Status.Phase = v1.WorkspacePhaseTerminating
		if err := r.Status().Update(ctx, workspace); err != nil {
			recordStatusUpdateConflictOnError("handleSuspended_ttl_expired", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: ttl - elapsed}, nil
}

func (r *WorkspaceReconciler) handleResuming(ctx context.Context, workspace *v1.Workspace) (ctrl.Result, error) {
	// Ensure password secret exists (may have been cleaned up).
	if err := r.ensurePasswordSecret(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	}
	workspace.Status.Phase = v1.WorkspacePhaseCreating
	workspace.Status.SuspendedAt = nil
	// US-23.3: LastActivityAt is now written by the API service to the
	// metadata annotation (see ActivateWorkspace). The controller no
	// longer writes LastActivityAt — single-writer principle.
	// Set the resume anchor so handleCreating can measure end-to-end
	// resume latency via WorkspaceResumeDurationSeconds.
	now := metav1.Now()
	workspace.Status.ResumedAt = &now
	if err := r.Status().Update(ctx, workspace); err != nil {
		recordStatusUpdateConflictOnError("handleResuming_creating", err)
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

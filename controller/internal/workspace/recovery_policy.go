package workspace

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

type RecoveryPolicy struct {
	MaxAttempts    int
	BackoffBase    time.Duration
	BackoffMax     time.Duration
	BackoffFactor  int
	StabilityReset time.Duration
	// ExhaustionAfter is the ConsecutiveFailures count at which the
	// recovery episode is flagged exhausted (#760): a RecoveryExhausted
	// condition, a warning Event, and a
	// WorkspaceRecoveryExhaustedTotal increment fire once, on the
	// crossing. Retries are NOT halted — backoff continues, and
	// spec.suspend=true (#699) is the operator's halt.
	ExhaustionAfter int32
}

var recoveryPolicies = map[FailureClass]RecoveryPolicy{
	FailureClassInfrastructure: {0, 5 * time.Second, 2 * time.Minute, 2, 2 * time.Minute, 10},
	FailureClassResource:       {0, 10 * time.Second, 5 * time.Minute, 2, 2 * time.Minute, 6},
	FailureClassProcess:        {0, 10 * time.Second, 5 * time.Minute, 2, 2 * time.Minute, 6},
	FailureClassConfiguration:  {0, 30 * time.Second, 5 * time.Minute, 2, 2 * time.Minute, 3},
}

func calculateBackoff(failures int32, policy RecoveryPolicy) time.Duration {
	if failures <= 0 {
		return 0
	}
	shift := int(failures - 1)
	if shift > 30 {
		shift = 30
	}
	// failures >= 1 here (early-return above), so shift is bounded [0,30];
	// shift the duration directly to avoid an int→uint conversion
	// (gosec G115) while staying bit-identical to BackoffBase * 2^shift.
	backoff := policy.BackoffBase << shift
	if backoff > policy.BackoffMax || backoff < 0 {
		backoff = policy.BackoffMax
	}
	return backoff
}

// recoveryExhausted derives the exhaustion signal from the consecutive
// failure count. Every class carries a non-zero threshold (#760): the
// Infrastructure class previously had none and its failure loops (the
// Longhorn silent-loop case) produced no operator signal at all.
func recoveryExhausted(consecutiveFailures int32, policy RecoveryPolicy) bool {
	return policy.ExhaustionAfter > 0 && consecutiveFailures >= policy.ExhaustionAfter
}

// recoveryExhaustedMessage is the single source for the condition and
// Event text. It states what the signal means (retries continue), the
// count/class that crossed, and the operator remedy (#699 Spec.Suspend
// — the per-workspace halt for a loop that will not self-heal).
func recoveryExhaustedMessage(failures int32, class FailureClass) string {
	return fmt.Sprintf(
		"recovery exhausted after %d consecutive %s failures; backoff retries continue — suspend the workspace (spec.suspend=true) to halt the loop and investigate",
		failures, class)
}

func timeUntilNextRetry(ws *v1.Workspace) time.Duration {
	if ws.Status.NextRetryAt == nil {
		return 0
	}
	remaining := time.Until(ws.Status.NextRetryAt.Time)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// clearRecoveryState returns the workspace to a fresh-start recovery
// episode. Every path that zeroes ConsecutiveFailures MUST go through
// this: the RecoveryExhausted condition is derived from the counters,
// so it must never outlive them (#760).
func clearRecoveryState(ws *v1.Workspace) {
	ws.Status.ConsecutiveFailures = 0
	ws.Status.LastFailureClass = ""
	ws.Status.LastFailureAt = nil
	ws.Status.NextRetryAt = nil
	ws.Status.LastStableAt = nil
	removeCondition(ws, v1.WorkspaceConditionRecoveryExhausted)
}

// maybeResetConsecutiveFailures clears recovery state after the workspace
// has been healthy for the stability window (2 min). If LastStableAt is nil
// and there is outstanding recovery state, it starts the clock.
//
// The guard checks BOTH ConsecutiveFailures and ControllerRestartCount: a
// health-check restart bumps ControllerRestartCount without touching
// ConsecutiveFailures (US-24.7), so the reset must be reachable for either
// counter independently. Worklog 0372 (C1): the previous guard checked only
// ConsecutiveFailures, leaving ControllerRestartCount un-resettable in the
// common (health-restart-only) case. Matches US-24.8 spec lines 13-17.
func maybeResetConsecutiveFailures(ws *v1.Workspace) {
	if ws.Status.ConsecutiveFailures == 0 && ws.Status.ControllerRestartCount == 0 {
		return
	}
	if ws.Status.LastStableAt == nil {
		now := metav1.Now()
		ws.Status.LastStableAt = &now
		return
	}
	elapsed := time.Since(ws.Status.LastStableAt.Time)
	if elapsed >= stabilityResetWindow {
		clearRecoveryState(ws)
		ws.Status.ControllerRestartCount = 0
	}
}

const stabilityResetWindow = 2 * time.Minute

// markRecoveryExhausted derives the RecoveryExhausted condition from the
// post-increment ConsecutiveFailures (#760). It returns true only on an
// episode's not-exhausted → exhausted crossing — the single transition
// that emits the Event and increments the counter — so a workspace
// already flagged exhausted never double-fires while the episode
// continues (including across a mid-episode failure-class switch: the
// counters are class-agnostic, so the crossed condition stands until
// the recovery state resets).
func (r *WorkspaceReconciler) markRecoveryExhausted(ws *v1.Workspace, class FailureClass, policy RecoveryPolicy) bool {
	if !recoveryExhausted(ws.Status.ConsecutiveFailures, policy) {
		return false
	}
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionRecoveryExhausted {
			return false
		}
	}
	r.setCondition(ws, v1.WorkspaceConditionRecoveryExhausted, "True",
		v1.ReasonRecoveryExhausted, recoveryExhaustedMessage(ws.Status.ConsecutiveFailures, class))
	return true
}

func (r *WorkspaceReconciler) enterRecovery(ctx context.Context, ws *v1.Workspace, class FailureClass) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Capture transition state before incrementing. Gauges must only Inc
	// on the not-in-recovery → in-recovery transition, not on every
	// enterRecovery call (a workspace can fail N times before recovering,
	// but only Dec's once on success — so N Inc's would drift by N-1).
	wasInRecovery := ws.Status.ConsecutiveFailures > 0

	ws.Status.ConsecutiveFailures++
	ws.Status.LastFailureClass = string(class)
	now := metav1.Now()
	ws.Status.LastFailureAt = &now

	policy := recoveryPolicies[class]
	exhaustionCrossed := r.markRecoveryExhausted(ws, class, policy)

	backoff := calculateBackoff(ws.Status.ConsecutiveFailures, policy)
	if backoff > 0 {
		nextRetry := metav1.NewTime(now.Add(backoff))
		ws.Status.NextRetryAt = &nextRetry
	}

	ws.Status.Phase = v1.WorkspacePhaseCreating
	ws.Status.PodIP = ""
	ws.Status.Endpoint = ""

	logger.Info("Recovery initiated",
		"class", class, "failures", ws.Status.ConsecutiveFailures,
		"backoff", backoff, "recoveryExhausted", exhaustionCrossed)

	result := ctrl.Result{RequeueAfter: backoff}
	if err := r.Status().Update(ctx, ws); err != nil {
		recordStatusUpdateConflictOnError("enterRecovery", err)
		return result, err
	}
	if exhaustionCrossed {
		logger.Info("Recovery exhausted",
			"failures", ws.Status.ConsecutiveFailures, "class", class)
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(v1.ReasonRecoveryExhausted),
				"%s", recoveryExhaustedMessage(ws.Status.ConsecutiveFailures, class))
		}
	}
	recordRecoveryMetrics(ws, class, wasInRecovery, exhaustionCrossed)
	return result, nil
}

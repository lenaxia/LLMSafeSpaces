// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_update_policy.go — #1746 agentdDelivery.updatePolicy.
//
// Owner ruling (2026-10-19): workspace pods must NOT roll when a
// platform upgrade changes the agentd digest — the user owns when their
// sessions are interrupted; agentd updates apply at the next natural
// pod recreation (compute refresh, suspend/resume, any other
// recreation). The overlay's lazy delivery already behaves this way
// (#1724 analysis); this file makes it a POLICY with three shapes:
//
//   - OnRefresh (default — the product posture): no pod touch; record
//     the desired digest on status (agentdPendingDigest) for the
//     "agentd X pending — applies on next refresh" surface.
//   - Rolling (opt-in, per workspace or instance-wide): roll on drift,
//     THROUGH the #761 drain — deferred behind busy sessions until
//     idle or stalled, like every polite deletion.
//   - forceRoll (escape hatch, any policy): reconcile NOW, drain
//     bypassed — the operator setting it declares consent to cut
//     in-flight turns (security-critical agentd fixes; the #1505
//     user-consent posture).
//
// Digest identity: drift compares the DIGEST PORTION only
// ("sha256:<64hex>"). A repo/tag re-pin of the same digest is not
// drift — the binary is identical — so Renovate tag bumps never churn
// the fleet.
//
// Deletion-path classification (upgrade_rollout_kill_test.go
// inventory): the one deletePodByName site here executes inside
// handleActive (already classified); its drain ruling is deliberate —
// Rolling defers per #761, forceRoll is consent-bypassed per #1505.

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	// drainReasonAgentdUpdateRoll is the #761 drain reason for the
	// Rolling policy's proactive recreation (the polite path).
	drainReasonAgentdUpdateRoll = "agentd_update_roll"
	// drainReasonAgentdForceRoll is the forced-metric reason for the
	// forceRoll hatch (drain bypassed by declared consent).
	drainReasonAgentdForceRoll = "agentd_update_roll_forced"
)

// resolveAgentdUpdatePolicy applies the override chain: per-workspace
// spec.agentdDelivery.updatePolicy → controller instance default
// (--agentd-update-policy) → OnRefresh (the product posture).
func (r *WorkspaceReconciler) resolveAgentdUpdatePolicy(ws *v1.Workspace) v1.AgentdUpdatePolicy {
	if ws.Spec.AgentdDelivery != nil && ws.Spec.AgentdDelivery.UpdatePolicy != "" {
		return ws.Spec.AgentdDelivery.UpdatePolicy
	}
	if r.AgentdUpdatePolicyDefault != "" {
		return r.AgentdUpdatePolicyDefault
	}
	return v1.AgentdUpdatePolicyOnRefresh
}

// agentdDigestPortion extracts the digest identity ("sha256:<64hex>")
// from a digest-pinned image reference; "" when none is present.
func agentdDigestPortion(ref string) string {
	if i := strings.Index(ref, "@sha256:"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// podAgentdDigest reads the digest the pod's agentd overlay
// image-volume was built with; "" for pods without overlay wiring
// (legacy baked-in mode).
func podAgentdDigest(pod *corev1.Pod) string {
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == agentdVolumeName && v.Image != nil {
			return agentdDigestPortion(v.Image.Reference)
		}
	}
	return ""
}

// recordAgentdDigestOnBuild stamps the build outcome on status (#1746):
// effective = the digest the built pod carries; pending = the desired
// digest when it still differs (possible when the controller's desired
// image changed between the build and this write — the next Active
// pass re-evaluates).
func (r *WorkspaceReconciler) recordAgentdDigestOnBuild(ws *v1.Workspace, pod *corev1.Pod) {
	effective := podAgentdDigest(pod)
	ws.Status.AgentdEffectiveDigest = effective
	ws.Status.AgentdPendingDigest = ""
	if desired := agentdDigestPortion(r.AgentdImage); desired != "" && effective != desired {
		ws.Status.AgentdPendingDigest = desired
	}
}

// reconcileAgentdDelivery is the Active-phase policy pass (#1746),
// invoked once the pod is Running. It maintains the drift status fields
// and, when the policy asks for an immediate roll (Rolling, or
// forceRoll under any policy), executes it through the same
// delete-to-Creating transition the arch-drift path uses.
//
// Returns handled=true when the caller must return result as-is (the
// roll executed, or it is deferred behind busy sessions — status
// already persisted in both cases).
func (r *WorkspaceReconciler) reconcileAgentdDelivery(ctx context.Context, ws *v1.Workspace, pod *corev1.Pod) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	effective := podAgentdDigest(pod)
	desired := agentdDigestPortion(r.AgentdImage)

	// Level-set the drift fields every pass: effective follows the
	// running pod; pending is set exactly while desired is unapplied.
	ws.Status.AgentdEffectiveDigest = effective
	ws.Status.AgentdPendingDigest = ""
	if desired == "" || effective == desired {
		return ctrl.Result{}, false, nil // converged (or legacy mode)
	}
	ws.Status.AgentdPendingDigest = desired

	force := ws.Spec.AgentdDelivery != nil && ws.Spec.AgentdDelivery.ForceRoll

	switch {
	case force:
		// The escape hatch: immediate, drain bypassed. The operator
		// setting forceRoll declared the consent to cut in-flight
		// turns (#1505 posture) — record it and roll now.
		logger.Info("agentdDelivery.forceRoll: rolling pod immediately (drain bypassed by operator consent)",
			"workspace", ws.Name, "effective", effective, "desired", desired)
		metrics.WorkspaceDrainForcedTotal.WithLabelValues(drainReasonAgentdForceRoll).Inc()
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeNormal, "AgentdUpdateForcedRoll",
				"agentd digest drift forced an immediate pod roll (effective %s → desired %s): the session drain was bypassed by agentdDelivery.forceRoll; in-flight turns were cut",
				shortDigest(effective), shortDigest(desired))
		}
	case r.resolveAgentdUpdatePolicy(ws) == v1.AgentdUpdatePolicyRolling:
		// The polite roll: deferred behind busy sessions (#761). The
		// pending-digest status written above stays visible while the
		// roll waits — persist it before the deferral returns.
		if r.drainBeforePodDeletion(ctx, ws, drainReasonAgentdUpdateRoll) == drainDefer {
			if err := r.Status().Update(ctx, ws); err != nil {
				recordStatusUpdateConflictOnError("handleActive_agentd_update_defer", err)
				return ctrl.Result{}, true, err
			}
			return ctrl.Result{RequeueAfter: drainPollInterval}, true, nil
		}
		logger.Info("agentdDelivery.updatePolicy=Rolling: rolling pod on digest drift",
			"workspace", ws.Name, "effective", effective, "desired", desired)
	default:
		// OnRefresh: record and wait for the next natural recreation.
		// The user owns when their sessions are interrupted. The status
		// fields persist via handleActive's own Status().Update.
		return ctrl.Result{}, false, nil
	}

	// Roll: delete to Creating; handleCreating's buildPod applies the
	// desired digest (the overlay pulls the current pin at build time).
	name := podName(ws.Name, string(ws.UID))
	r.deletePodByName(ctx, name, ws.Namespace)
	runtime := ws.Spec.Runtime
	secLevel := string(ws.Spec.SecurityLevel)
	metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Dec()
	ws.Status.Phase = v1.WorkspacePhaseCreating
	ws.Status.PodIP = ""
	ws.Status.Endpoint = ""
	if err := r.Status().Update(ctx, ws); err != nil {
		recordStatusUpdateConflictOnError("handleActive_agentd_update_roll", err)
		metrics.WorkspacesRunning.WithLabelValues(runtime, secLevel).Inc()
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, true, nil
}

// shortDigest renders a digest for logs/events (the full value rides
// status only).
func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19] + "…"
	}
	return d
}

// validateAgentdUpdatePolicyFlag guards --agentd-update-policy at
// controller startup (empty = built-in default OnRefresh).
func validateAgentdUpdatePolicyFlag(v string) error {
	switch v1.AgentdUpdatePolicy(v) {
	case "", v1.AgentdUpdatePolicyOnRefresh, v1.AgentdUpdatePolicyRolling:
		return nil
	default:
		return fmt.Errorf("--agentd-update-policy must be Rolling or OnRefresh, got %q", v)
	}
}

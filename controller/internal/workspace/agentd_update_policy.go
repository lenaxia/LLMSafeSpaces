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
	"fmt"

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

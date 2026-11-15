// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// harness_delivery.go — the per-harness delivery map (pi-scoping report
// §6 item 2, S5): spec.harness is a first-class, choosable Workspace
// dimension, and each harness carries its own digest-pinned overlay
// artifact (opencode → /opencode, design 0053 §4.2; pi → /pi, §5.4).
//
// Pre-S5 the overlay wiring was unconditional: buildPod pinned BOTH
// image volumes into every pod. With the harness dimension the wiring
// is per-workspace — exactly ONE harness overlay is wired, selected by
// spec.harness — and the supervisor subcommand follows the same
// selection (supervise-opencode vs supervise-pi, agentd_sidecar.go).
//
// Zero behavior change for default workspaces (the S5 constraint): the
// harness=opencode branch wires the opencode overlay exactly where and
// how wireOpencodeOverlay ran before — byte-identical pod specs, pinned
// by the golden pod-spec diff test.

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// harnessDeliverySpec is one harness's overlay-delivery pin set — the
// generalization of OpencodeDeliveryConfig/PiDeliveryConfig at the
// point of consumption (buildPod). The flags-to-reconciler structs stay
// per-artifact (their validation contracts differ: opencode kept a
// baked-fallback interim, pi never had one); THIS type is what the
// per-harness map resolves to.
type harnessDeliverySpec struct {
	Image             string
	BinarySHA256AMD64 string
	BinarySHA256ARM64 string
}

// harnessDeliveries renders the per-harness delivery map from the
// reconciler's resolved startup state. Both pins are boot-mandatory
// (validateOpencodeDeliveryConfig / validatePiDeliveryConfig refuse an
// unpinned controller), so every entry is configured in a booted
// controller; the map shape is the seam a future third harness extends.
func (r *WorkspaceReconciler) harnessDeliveries() map[v1.HarnessType]harnessDeliverySpec {
	return map[v1.HarnessType]harnessDeliverySpec{
		v1.HarnessOpencode: {
			Image:             r.OpencodeImage,
			BinarySHA256AMD64: r.OpencodeBinarySHA256AMD64,
			BinarySHA256ARM64: r.OpencodeBinarySHA256ARM64,
		},
		v1.HarnessPi: {
			Image:             r.PiImage,
			BinarySHA256AMD64: r.PiBinarySHA256AMD64,
			BinarySHA256ARM64: r.PiBinarySHA256ARM64,
		},
	}
}

// workspaceHarness resolves a Workspace's harness with the CRD default
// applied ("" = opencode) — the ONE place the defaulting rule lives for
// controller-side consumers.
func workspaceHarness(ws *v1.Workspace) v1.HarnessType {
	if ws.Spec.Harness == "" {
		return v1.HarnessOpencode
	}
	return ws.Spec.Harness
}

// wireHarnessOverlay wires the workspace's SELECTED harness overlay
// (image volume + RO mount + verify env pins) — the per-harness branch
// the S5 constraint pins byte-identical for harness=opencode.
func (r *WorkspaceReconciler) wireHarnessOverlay(workspace *v1.Workspace, mainContainer *corev1.Container, volumes *[]corev1.Volume) {
	switch workspaceHarness(workspace) {
	case v1.HarnessPi:
		r.wirePiOverlay(mainContainer, volumes)
	default:
		r.wireOpencodeOverlay(mainContainer, volumes)
	}
}

// harnessSupervisorSubcommand is the main container's supervisor
// subcommand for the workspace's harness: supervise-opencode (opencode
// serve, HTTP+SSE, one multi-session server) vs supervise-pi (JSONL RPC
// children, process-per-session — the §1.4 ruling). The agentd binary
// dispatches both (cmd/workspace-agentd/main.go); the controller only
// selects.
func harnessSupervisorSubcommand(ws *v1.Workspace) string {
	if workspaceHarness(ws) == v1.HarnessPi {
		return "supervise-pi"
	}
	return "supervise-opencode"
}

// validateWorkspaceHarness enforces the per-workspace harness
// constraints at pod build: a harness whose delivery is not configured
// (an unpinned controller cannot happen post-startup-validation, but a
// programmatically-constructed reconciler in tests can be missing
// fields) or whose pod topology has no supervisor role gets a LOUD
// refusal — never a pod that would crash-loop on a missing overlay.
//
// The recorded S5 constraint: pi requires the sidecar topology.
// supervise-pi is a PID-1 supervisor (the §1.4 process-per-session
// driver); the single-container mode's --supervise role is
// opencode-shaped (managed opencode serve spawn) and has no pi
// counterpart. A pi workspace on a non-sidecar controller is a
// configuration mismatch the operator must resolve on the controller
// (helm agentdSidecar.enabled) — not something to half-build.
func (r *WorkspaceReconciler) validateWorkspaceHarness(workspace *v1.Workspace) error {
	h := workspaceHarness(workspace)
	d, ok := r.harnessDeliveries()[h]
	if !ok {
		return fmt.Errorf("workspace harness %q has no delivery configured on this controller", h)
	}
	if d.Image == "" {
		return fmt.Errorf("workspace harness %q requires its overlay delivery (--%s-image) configured on this controller", h, h)
	}
	if h == v1.HarnessPi && !r.AgentdSidecarEnabled {
		return fmt.Errorf("workspace harness %q requires the agentd sidecar topology (supervise-pi is a PID-1 supervisor; set helm agentdSidecar.enabled on this controller)", h)
	}
	return nil
}

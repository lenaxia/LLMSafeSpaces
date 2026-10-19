// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Design 0053 §4.3: the base image's ENV block encodes PVC mount
// topology — platform contract, not OS content. Post-strip the
// controller injects it as pod env. Single source of truth for the
// composition; the workspace-setup init reuses it for package installs.
//
// PATH keeps the exact base ordering: PVC homes first (user-installed
// tools win), then the system mise shims (image-baked toolchains), then
// the distro default. agentd additionally prepends /sandbox-runtime/bin
// at opencode spawn (design 0053 S2) — supervisor-side, not here.
const platformBasePath = "/workspace/.local/bin:/workspace/.local/share/mise/shims:/workspace/.local/share/go/bin:/workspace/.local/share/cargo/bin:/usr/local/share/mise/shims:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// validateOverlayDeliveryPins enforces design 0053 §4.5: both overlay
// pins are mandatory; there is no baked-fallback mode. Called at pod
// build (fail the workspace, not the fleet) and at controller startup
// (fail the process). Error names match the Helm values keys so the
// operator hint is identical from both surfaces.
func (r *WorkspaceReconciler) validateOverlayDeliveryPins() error {
	if err := validateAgentdDeliveryConfig(r.AgentdImage, r.AgentdBinarySHA256AMD64, r.AgentdBinarySHA256ARM64); err != nil {
		if r.AgentdImage == "" {
			return fmt.Errorf("agentdDelivery.image is mandatory (design 0053 §4.5 — no baked fallback): %w", err)
		}
		return fmt.Errorf("agentdDelivery invalid: %w", err)
	}
	if err := validateOpencodeDeliveryConfig(r.OpencodeImage, r.OpencodeBinarySHA256AMD64, r.OpencodeBinarySHA256ARM64); err != nil {
		if r.OpencodeImage == "" {
			return fmt.Errorf("opencodeDelivery.image is mandatory (design 0053 §4.5 — no baked fallback): %w", err)
		}
		return fmt.Errorf("opencodeDelivery invalid: %w", err)
	}
	return nil
}

// platformBaseEnv is the relocated base-image ENV block: mise homes on
// the PVC, package-manager prefixes, the git-credential env layer
// (#1087), and the PATH composition. Applied to every container that
// runs platform tooling (main container, workspace-setup init).
//
// Containment (design 0051 D1, #942, README Rule 12): OPENCODE_* env
// names are runtime knowledge — they live behind the agent seam in the
// supervisor (cmd/workspace-agentd opencodeServeCmd), never here.
func platformBaseEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MISE_DATA_DIR", Value: "/workspace/.local/share/mise"},
		{Name: "CARGO_HOME", Value: "/workspace/.local/share/cargo"},
		{Name: "GEM_HOME", Value: "/workspace/.local/share/gem"},
		{Name: "GOPATH", Value: "/workspace/.local/share/go"},
		// #1758 (the #1744 maintainer addendum): the regenerable Go
		// build-cache economy moves OFF the PVC. Go's defaults land
		// GOCACHE in $HOME/.cache/go-build and GOTMPDIR in /tmp — both
		// PVC subPaths (every writable surface agents get is PVC-backed;
		// agents cannot self-relocate), which is the 55%→93%-in-minutes
		// fill incident. Both point at the dedicated disk-backed
		// build-cache emptyDir (pod_builder: 8Gi SizeLimit, dies with
		// the pod — correct for caches). GOPATH stays PVC-mounted:
		// GOMODCACHE re-downloads are network-expensive and durable
		// across suspend/resume by design; its diskrecovery class
		// covers pressure sweeps.
		{Name: "GOCACHE", Value: "/sandbox-cache/go-build"},
		{Name: "GOTMPDIR", Value: "/sandbox-cache/gotmp"},
		{Name: "NPM_CONFIG_PREFIX", Value: "/workspace/.local"},
		{Name: "PYTHONUSERBASE", Value: "/workspace/.local"},
		{Name: "GIT_CONFIG_COUNT", Value: "1"},
		{Name: "GIT_CONFIG_KEY_0", Value: "credential.helper"},
		{Name: "GIT_CONFIG_VALUE_0", Value: "store --file=/home/sandbox/.git-credentials"},
		{Name: "PATH", Value: platformBasePath},
	}
}

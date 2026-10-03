// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// feature_status.go — #1580 part 2: the in-pod feature-flag READ tool.
//
// Scope discipline (the issue's ruling + the orchestrator's
// approval): every entry is a flag agentd can see AUTHENTICALLY —
// the controller's boot-time env projection (the space-set class)
// and agentd's own operator/chart env surface. Instance-level
// settings (the API's settings registry: rate limiting, workflow
// and trigger knobs, the dev-preview kill-switch) are structurally
// NOT readable in-pod — the D3 no-API-credentials posture — and are
// deliberately NOT fabricated here; the tool's description names
// that class so the absence reads as design, not omission.
//
// The CONTROLLABLE set has one entry today: dev_preview_headers
// (design 0062) — agent-owned local tool state, written through the
// agentd MCP tool. Everything else still reports controllable:false
// as FACT; candidate-dynamic flags are enumerated in #1580's PR for
// the owner to bless — no control surface is invented ahead of that
// blessing.

import (
	"encoding/json"
	"fmt"
	"os"
)

// featureFlag is one reported entry (stable shape: the tool's output
// is machine-readable contract).
type featureFlag struct {
	Feature      string `json:"feature"`
	Active       bool   `json:"active"`
	Source       string `json:"source"`        // "space" | "operator" | "tool"
	SourceDetail string `json:"source_detail"` // the CRD path / env surface / local tool state behind it
	Controllable bool   `json:"controllable"`  // one true entry today (dev_preview_headers); the rest reported, not assumed
}

// mcpFeatureStatus assembles the flag inventory from the boot env.
// Values are activity booleans only — knob VALUES are not echoed
// (they are chart-internal configuration, not feature state).
func mcpFeatureStatus() (string, error) {
	flags := []featureFlag{}

	// The one SPACE-set flag: spec.networkAccess.devPreview. The API
	// pushes every toggle to the running pod (#1617); the boot env
	// (WORKSPACE_DEV_PREVIEW_ENABLED, projected at pod creation) is the
	// fallback. Neither present = controller skew (an older controller):
	// report active=false with the skew in the detail — never guess
	// "enabled".
	devPreview := featureFlag{
		Feature: "dev_preview",
		Source:  "space",
	}
	active, reported, live := devPreviewState()
	devPreview.Active = active
	switch {
	case live:
		devPreview.SourceDetail = "workspace CRD spec.networkAccess.devPreview (live push from the API toggle; boot-env fallback: WORKSPACE_DEV_PREVIEW_ENABLED)"
	case reported:
		devPreview.SourceDetail = "workspace CRD spec.networkAccess.devPreview (projected by the controller as WORKSPACE_DEV_PREVIEW_ENABLED)"
	default:
		devPreview.SourceDetail = "workspace CRD spec.networkAccess.devPreview (projected by the controller as WORKSPACE_DEV_PREVIEW_ENABLED) — UNREPORTED (controller skew: an older controller did not project the state)"
	}
	flags = append(flags, devPreview)

	// Operator/chart-set: the preview TOPOLOGY (which URL shape the
	// dev-preview tool mints) — PREVIEW_ORIGIN_BASE_DOMAIN presence.
	flags = append(flags, featureFlag{
		Feature:      "dev_preview_per_workspace_origin",
		Active:       os.Getenv("PREVIEW_ORIGIN_BASE_DOMAIN") != "",
		Source:       "operator",
		SourceDetail: "PREVIEW_ORIGIN_BASE_DOMAIN (the controller's preview-origin base domain; absent = path-mode preview URLs)",
	})

	// Operator/chart-set: the Epic 72 relay-only inference plane.
	flags = append(flags, featureFlag{
		Feature:      "inference_relay_plane",
		Active:       os.Getenv("INFERENCE_RELAY_BASEURL") != "",
		Source:       "operator",
		SourceDetail: "INFERENCE_RELAY_BASEURL (Epic 72 relay-only emission; agentd monitors liveness and degrades loudly when set)",
	})

	// Operator/chart-set: the upload staging leg (design 0060). r2:
	// the r0 key (LLMSAFESPACES_UPLOADS_STAGING_PATH) was the phantom
	// class — the controller projects only the UPLOAD_STAGING_* KNOBS
	// (the path env is an agentd-side override with a compiled-in
	// default). The truthful activity basis: buildSidecarDeps
	// constructs the stager unconditionally in sidecar mode and never
	// in single-container mode — the staging leg exists exactly where
	// this process is the sidecar (same basis as agent_sidecar).
	flags = append(flags, featureFlag{
		Feature:      "upload_staging",
		Active:       os.Getenv("AGENTD_SIDECAR_PASSWORD") != "",
		Source:       "operator",
		SourceDetail: "the sidecar's staged-upload leg (design 0060; constructed unconditionally by buildSidecarDeps in sidecar mode, never in single-container; the UPLOAD_STAGING_* knobs tune it when operator-set)",
	})

	// Operator/chart-set: the deployment shape. Truthful basis
	// (r1: the original SINGLE_CONTAINER_SPAWN_MARKER key was a
	// test-harness-only phantom — unset in every real deployment):
	// AGENTD_SIDECAR_PASSWORD is projected by the controller ONLY
	// into the sidecar container (agentd_sidecar.go), and sidecar boot
	// fail-closes without it — so its presence here means this process
	// IS the sidecar; its absence in a serving process means
	// single-container mode.
	flags = append(flags, featureFlag{
		Feature:      "agent_sidecar",
		Active:       os.Getenv("AGENTD_SIDECAR_PASSWORD") != "",
		Source:       "operator",
		SourceDetail: "AGENTD_SIDECAR_PASSWORD (projected only into the sidecar container; absent in this process = single-container deployment)",
	})

	// Tool-set (design 0062 §5): the agent's own preview-header
	// configuration — local tool state one process away, not projected
	// platform config. Active means entries are configured (headers
	// are being injected); the count and the tool itself are the
	// control surface, hence controllable:true.
	n := currentDevPreviewHeaders().count()
	flags = append(flags, featureFlag{
		Feature:      "dev_preview_headers",
		Active:       n > 0,
		Source:       "tool",
		SourceDetail: fmt.Sprintf("%d entries (agent-set literals in agentd memory-backed state)", n),
		Controllable: true,
	})

	out, err := json.MarshalIndent(flags, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Design 0053 §4.2: opencodeDelivery values gating — mirrors
// agentd_delivery_chart_test.go exactly.
//
//   - Default (empty image): no opencode flags on the controller
//     Deployment (opt-in; inert until the base strip in S3).
//   - Image-only (the RENOVATE FORM, repo:tag@sha256:digest): exactly
//     one flag renders — hashes resolve from index annotations at
//     controller startup.
//   - Full manual pin: all three flags render.
//   - Malformed configs FAIL the render: hashes without an image (they
//     are per-image overrides), or a one-sided hash pair (both or
//     neither).
//   - RBAC: the llmsafespaces-opencode-pins grant renders when the
//     image is set and is absent otherwise.
//   - No sidecar-style requires-image guard: opencodeDelivery does not
//     gate agentdSidecar (independent artifacts, independent cadences).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func opencodeFlags(t *testing.T, valuesYAML string) []string {
	t.Helper()
	docs := helmTemplate(t, valuesYAML)
	var flags []string
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"]
		if !strings.Contains(name.(string), "controller") {
			continue
		}
		spec := doc["spec"].(map[string]any)
		pod := spec["template"].(map[string]any)["spec"].(map[string]any)
		for _, c := range pod["containers"].([]any) {
			cm := c.(map[string]any)
			if cm["name"] != "manager" {
				continue
			}
			for _, a := range cm["args"].([]any) {
				if s, ok := a.(string); ok && strings.HasPrefix(s, "--opencode-") {
					flags = append(flags, s)
				}
			}
		}
	}
	return flags
}

func TestOpencodeDelivery_ConfiguredRendersAllFlags(t *testing.T) {
	flags := opencodeFlags(t, `controller:
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Amd64: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    binarySHA256Arm64: dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
`)
	require.Len(t, flags, 3, "full manual pin renders image + both overrides: %v", flags)
	require.Contains(t, flags, "--opencode-image=ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	require.Contains(t, flags, "--opencode-binary-sha256-amd64=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	require.Contains(t, flags, "--opencode-binary-sha256-arm64=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
}

// TestOpencodeDelivery_ImageOnlyRendersSingleFlag is the RENOVATE FORM:
// repo:tag@sha256:digest with no hash overrides — the single
// Renovate-updatable coordinate, same as agentdDelivery.
func TestOpencodeDelivery_ImageOnlyRendersSingleFlag(t *testing.T) {
	flags := opencodeFlags(t, `controller:
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`)
	require.Equal(t, []string{"--opencode-image=ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"}, flags,
		"image-only must render exactly one flag")
}

func TestOpencodeDelivery_OneSidedHashOverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Amd64: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--kube-version", testKubeVersion, "-f", valuesPath,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided hash override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

func TestOpencodeDelivery_OneSidedArm64OverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Arm64: dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--kube-version", testKubeVersion, "-f", valuesPath,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided arm64-only override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

// TestOpencodeDelivery_HashesWithoutImageFailsRender: the reverse
// half-configuration must be a render-time error — silently running
// baked-in mode while the operator believes overlay delivery is on is
// the worst failure mode.
func TestOpencodeDelivery_HashesWithoutImageFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  opencodeDelivery:
    binarySHA256Amd64: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
`), 0o600))

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--kube-version", testKubeVersion, "-f", valuesPath,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"hashes-without-image must fail the render; output: %s", out)
	require.Contains(t, string(out), "opencodeDelivery.image is mandatory",
		"the failure must be the reverse-guard fail; output: %s", out)
}

// TestOpencodeDelivery_PinsRBACGrantRenders guards the least-privilege
// pins grant: scoped get/update on llmsafespaces-opencode-pins plus the
// separate unscoped create (K8s cannot resourceNames-scope creation),
// rendered when opencodeDelivery.image is set independent of the other
// feature gates.
func TestOpencodeDelivery_PinsRBACGrantRenders(t *testing.T) {
	docs := helmTemplate(t, `controller:
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
  inferenceRelay:
    enabled: false
  freeModelsRefresher:
    enabled: false
`)
	type rule struct {
		Verbs         []string `json:"verbs"`
		Resources     []string `json:"resources"`
		ResourceNames []string `json:"resourceNames,omitempty"`
	}
	var scoped, createRule bool
	for _, doc := range docs {
		if doc["kind"] != "Role" {
			continue
		}
		raw, err := yaml.Marshal(doc)
		require.NoError(t, err)
		if !strings.Contains(string(raw), "llmsafespaces-opencode-pins") {
			continue
		}
		var role struct {
			Rules []rule `json:"rules"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &role))
		for _, r := range role.Rules {
			hasCM := false
			for _, res := range r.Resources {
				if res == "configmaps" {
					hasCM = true
				}
			}
			if !hasCM {
				continue
			}
			if len(r.ResourceNames) == 1 && r.ResourceNames[0] == "llmsafespaces-opencode-pins" &&
				len(r.Verbs) == 2 && r.Verbs[0] == "get" && r.Verbs[1] == "update" {
				scoped = true
			}
			if len(r.ResourceNames) == 0 && len(r.Verbs) == 1 && r.Verbs[0] == "create" {
				createRule = true // create cannot be resourceNames-scoped (object does not pre-exist)
			}
		}
	}
	require.True(t, scoped, "exact scoped rule required: get+update on configmaps resourceNames=[llmsafespaces-opencode-pins]")
	require.True(t, createRule, "separate unscoped create rule required (create cannot be resourceNames-scoped)")
}

// TestOpencodeDelivery_DoesNotGateAgentdSidecar locks the FEATURE
// independence of the two delivery artifacts: agentdSidecar requires
// agentdDelivery (its own guard), NOT opencodeDelivery — both directions
// render clean with both pins present (the S3 mandatory-pin world).
func TestOpencodeDelivery_DoesNotGateAgentdSidecar(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()

	// agentdSidecar on: renders (both pins per the S3 mandatory gate —
	// the FEATURE independence under test, not pin presence).
	p := filepath.Join(dir, "sidecar-without-opencode.yaml")
	require.NoError(t, os.WriteFile(p, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
  agentdSidecar:
    enabled: true
`), 0o600))
	out, err := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--kube-version", testKubeVersion, "-f", p).CombinedOutput()
	require.NoError(t, err, "agentdSidecar must not require opencodeDelivery; output: %s", out)

	// opencodeDelivery set with agentdSidecar off: renders.
	p2 := filepath.Join(dir, "opencode-without-sidecar.yaml")
	require.NoError(t, os.WriteFile(p2, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`), 0o600))
	out, err = exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--kube-version", testKubeVersion, "-f", p2).CombinedOutput()
	require.NoError(t, err, "opencodeDelivery must not require agentdSidecar; output: %s", out)
}

// TestOpencodeVerificationAlertRenders asserts the page-on-any alert
// for the opencode verify-failure counter renders with critical
// severity (mirroring the agentd alert block).
func TestOpencodeVerificationAlertRenders(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	found := false
	for _, doc := range docs {
		if doc["kind"] != "PrometheusRule" {
			continue
		}
		raw, err := yaml.Marshal(doc)
		require.NoError(t, err)
		if !strings.Contains(string(raw), "LLMSafeSpacesOpencodeVerificationFailed") {
			continue
		}
		found = true
		require.Contains(t, string(raw), "llmsafespaces_workspace_opencode_verify_failures_total",
			"the alert must target the opencode verify-failures counter")
		require.Contains(t, string(raw), "severity: critical")
	}
	require.True(t, found, "LLMSafeSpacesOpencodeVerificationFailed alert must render when monitoring is enabled")
}

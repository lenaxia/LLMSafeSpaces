// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// #863: agentdDelivery values gating.
//
//   - Default (empty image): no agentd flags on the controller Deployment.
//   - Image-only (the RENOVATE FORM, repo:tag@sha256:digest): exactly
//     one flag renders — hashes resolve from index annotations at
//     controller startup.
//   - Full manual pin: all three flags render.
//   - Malformed configs FAIL the render: hashes without an image (they
//     are per-image overrides), or a one-sided hash pair (both or
//     neither) — install-time errors beat entrypoint exit-81 storms.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func agentdFlags(t *testing.T, valuesYAML string) []string {
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
				if s, ok := a.(string); ok && strings.HasPrefix(s, "--agentd-") {
					flags = append(flags, s)
				}
			}
		}
	}
	return flags
}

func TestAgentdDelivery_ConfiguredRendersAllFlags(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    binarySHA256Arm64: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`)
	require.Len(t, flags, 3, "full manual pin renders image + both overrides: %v", flags)
	require.Contains(t, flags, "--agentd-image=ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.Contains(t, flags, "--agentd-binary-sha256-amd64=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Contains(t, flags, "--agentd-binary-sha256-arm64=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
}

// TestAgentdDelivery_ImageOnlyRendersSingleFlag is the RENOVATE FORM:
// repo:tag@sha256:digest with no hash overrides. The controller
// resolves the hashes from the index annotations at startup — anything
// more here would reintroduce the desync footgun this exists to fix.
func TestAgentdDelivery_ImageOnlyRendersSingleFlag(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	require.Equal(t, []string{"--agentd-image=ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, flags,
		"image-only must render exactly one flag — the Renovate-updatable coordinate")
}

// TestAgentdDelivery_OneSidedHashOverrideFailsRender guards the
// both-or-neither override rule (amd64-set direction).
func TestAgentdDelivery_OneSidedHashOverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath,
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided hash override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

// TestAgentdDelivery_HashesWithoutImageFailsRender covers the REVERSE
// half-configuration: the with-gate on image would silently skip all
// flags (legacy mode) while the operator believes overlay mode is on.
// The explicit fail makes it a render-time error.
func TestAgentdDelivery_HashesWithoutImageFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`), 0o600))

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath,
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"hashes-without-image must fail the render — silently running legacy while the operator believes overlay mode is on is the worst failure mode; output: %s", out)
	require.Contains(t, string(out), "agentdDelivery.image is mandatory",
		"the failure must be the mandatory-pin gate; output: %s", out)
}

// TestAgentdDelivery_OneSidedArm64OverrideFailsRender mirrors the
// one-sided guard in the arm64-set direction (the Go validation table
// and the amd64 chart test cover the other direction).
func TestAgentdDelivery_OneSidedArm64OverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    binarySHA256Arm64: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath,
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided arm64-only override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

// TestAgentdDelivery_PinsRBACGrantRenders guards the least-privilege
// regression from PR review: the agentd-pins cache ConfigMap grant must
// render when agentdDelivery.image is set (independent of relay /
// free-models feature gates) and be absent otherwise.
func TestAgentdDelivery_PinsRBACGrantRenders(t *testing.T) {
	docs := helmTemplate(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
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
		if !strings.Contains(string(raw), "llmsafespaces-agentd-pins") {
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
			if len(r.ResourceNames) == 1 && r.ResourceNames[0] == "llmsafespaces-agentd-pins" &&
				len(r.Verbs) == 2 && r.Verbs[0] == "get" && r.Verbs[1] == "update" {
				scoped = true
			}
			if len(r.ResourceNames) == 0 && len(r.Verbs) == 1 && r.Verbs[0] == "create" {
				createRule = true // create cannot be resourceNames-scoped (object does not pre-exist)
			}
		}
	}
	require.True(t, scoped, "exact scoped rule required: get+update on configmaps resourceNames=[llmsafespaces-agentd-pins]")
	require.True(t, createRule, "separate unscoped create rule required (create cannot be resourceNames-scoped)")
}

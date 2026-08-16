// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// #863: agentdDelivery values gating.
//
//   - Default (empty image): no agentd flags on the controller Deployment.
//   - Configured: all three flags render, exactly as pinned.
//   - Half-configured (image set, hashes missing): the render FAILS —
//     the required guards in controller-deployment.yaml turn operator
//     error into an install-time error instead of a runtime one
//     (entrypoint exit-81 storm).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestAgentdDelivery_DefaultRendersNoFlags(t *testing.T) {
	flags := agentdFlags(t, "")
	require.Empty(t, flags, "legacy mode must render no agentd flags")
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
// both-or-neither override rule.
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
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided hash override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

func TestAgentdDelivery_HalfConfiguredFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    binarySHA256Arm64: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`), 0o600))

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"hashes-without-image must fail the render (they are per-image overrides); output: %s", out)
	require.Contains(t, string(out), "agentdDelivery.image is required",
		"the failure must be the reverse-guard fail; output: %s", out)
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

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"hashes-without-image must fail the render — silently running legacy while the operator believes overlay mode is on is the worst failure mode; output: %s", out)
	require.Contains(t, string(out), "agentdDelivery.image is required",
		"the failure must be the reverse-guard fail; output: %s", out)
}

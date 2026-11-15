// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Design 0053 §5.4: piDelivery values gating — mirrors
// opencode_delivery_chart_test.go exactly.
//
//   - Image-only (the RENOVATE FORM, repo:tag@sha256:digest): exactly
//     one flag renders — hashes resolve from index annotations at
//     controller startup.
//   - Full manual pin: all three flags render.
//   - Malformed configs FAIL the render: hashes without an image (they
//     are per-image overrides), or a one-sided hash pair (both or
//     neither).
//   - MANDATORY (unlike opencode's S1 opt-in): pi never had a baked-in
//     mode, so the empty image fails the render with its own message.
//   - RBAC: the llmsafespaces-pi-pins grant renders when the image is
//     set.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func piFlags(t *testing.T, valuesYAML string) []string {
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
				if s, ok := a.(string); ok && strings.HasPrefix(s, "--pi-") {
					flags = append(flags, s)
				}
			}
		}
	}
	return flags
}

func TestPiDelivery_ConfiguredRendersAllFlags(t *testing.T) {
	flags := piFlags(t, `controller:
  piDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Amd64: eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
    binarySHA256Arm64: ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
`)
	require.Len(t, flags, 3, "full manual pin renders image + both overrides: %v", flags)
	require.Contains(t, flags, "--pi-image=ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	require.Contains(t, flags, "--pi-binary-sha256-amd64=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	require.Contains(t, flags, "--pi-binary-sha256-arm64=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
}

// TestPiDelivery_ImageOnlyRendersSingleFlag is the RENOVATE FORM:
// repo:tag@sha256:digest with no hash overrides — the single
// Renovate-updatable coordinate, same as agentd/opencode.
func TestPiDelivery_ImageOnlyRendersSingleFlag(t *testing.T) {
	flags := piFlags(t, `controller:
  piDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`)
	require.Equal(t, []string{"--pi-image=ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"}, flags,
		"image-only must render exactly one flag")
}

func TestPiDelivery_OneSidedHashOverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  piDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Amd64: eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"-f", valuesPath,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided hash override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
	require.Contains(t, string(out), "piDelivery", "the failure must name the artifact whose pin is malformed: %s", out)
}

func TestPiDelivery_OneSidedArm64OverrideFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  piDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
    binarySHA256Arm64: ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"-f", valuesPath,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "one-sided hash override must fail the render; output: %s", out)
	require.Contains(t, string(out), "BOTH hashes or NEITHER", "output: %s", out)
}

func TestPiDelivery_HashesWithoutImageFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  piDelivery:
    binarySHA256Amd64: eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
    binarySHA256Arm64: ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion, "-f", valuesPath)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "hashes without an image must fail the render; output: %s", out)
	// The pi mandatory gate fires first (render order): both messages
	// name piDelivery, which is the attribution the operator needs.
	require.Contains(t, string(out), "piDelivery", "output: %s", out)
}

// TestPiDelivery_PinsConfigMapRBAC: the llmsafespaces-pi-pins grant
// renders when the image is set (scoped get/update + unscoped create on
// configmaps, same least-privilege shape as the agentd/opencode pins
// grants — the #890/#1546 lineage).
func TestPiDelivery_PinsConfigMapRBAC(t *testing.T) {
	docs := helmTemplate(t, `controller:
  piDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`)
	var foundScoped, foundCreate bool
	for _, doc := range docs {
		if doc["kind"] != "Role" {
			continue
		}
		raw, err := yaml.Marshal(doc)
		require.NoError(t, err)
		var role struct {
			Rules []struct {
				APIGroups     []string `yaml:"apiGroups"`
				Resources     []string `yaml:"resources"`
				ResourceNames []string `yaml:"resourceNames,omitempty"`
				Verbs         []string `yaml:"verbs"`
			} `yaml:"rules"`
		}
		require.NoError(t, yaml.Unmarshal(raw, &role))
		for _, rule := range role.Rules {
			hasCM := false
			for _, r := range rule.Resources {
				if r == "configmaps" {
					hasCM = true
				}
			}
			if !hasCM {
				continue
			}
			for _, n := range rule.ResourceNames {
				if n == "llmsafespaces-pi-pins" {
					require.Contains(t, rule.Verbs, "get")
					require.Contains(t, rule.Verbs, "update")
					require.NotContains(t, rule.Verbs, "list",
						"resourceNames-scoped rules cannot carry list (apiserver semantics)")
					foundScoped = true
				}
			}
			if len(rule.ResourceNames) == 0 && len(rule.Verbs) == 1 && rule.Verbs[0] == "create" {
				foundCreate = true
			}
		}
	}
	require.True(t, foundScoped, "the llmsafespaces-pi-pins scoped get/update grant must render")
	require.True(t, foundCreate, "the unscoped configmaps create grant must render (create cannot be resourceNames-scoped)")
}

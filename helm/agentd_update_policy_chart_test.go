// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// agentd_update_policy_chart_test.go — #1746: the updatePolicy chart
// surface.
//
//   - Default (empty): NO --agentd-update-policy flag — the controller's
//     built-in OnRefresh applies, and the Renovate single-coordinate
//     form still renders exactly one --agentd-* flag (the image line).
//   - Pinned Rolling: exactly one update-policy flag renders.
//   - A typo'd value FAILS the render — a misspelled policy must fail
//     install, not silently fall back to OnRefresh at controller boot.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentdUpdatePolicy_EmptyRendersNoFlag(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	require.Equal(t, []string{"--agentd-image=ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, flags,
		"empty updatePolicy keeps the Renovate form at exactly one --agentd-* flag (OnRefresh is the controller default)")
}

func TestAgentdUpdatePolicy_RollingRendersFlag(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    updatePolicy: Rolling
`)
	require.Contains(t, flags, "--agentd-update-policy=Rolling",
		"an instance pinning Rolling renders exactly the update-policy flag")
}

func TestAgentdUpdatePolicy_OnRefreshExplicitRendersFlag(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    updatePolicy: OnRefresh
`)
	require.Contains(t, flags, "--agentd-update-policy=OnRefresh")
}

func TestAgentdUpdatePolicy_InvalidValueFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte(`controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    updatePolicy: Onrestart
`), 0o600))
	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion, "-f", valuesPath,
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "a typo'd updatePolicy must fail the render; output: %s", out)
	require.Contains(t, string(out), "must be Rolling or OnRefresh", "output: %s", out)
}

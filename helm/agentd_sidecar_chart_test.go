// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// agentd_sidecar_chart_test.go — design 0051 US-2 chart-gate tests.
//
// The sidecar split is chart-gated (agentdSidecar.enabled, default
// false — single-container pods unchanged). These render-level tests
// pin the wiring contract:
//
//   - default: no --agentd-sidecar flag on the controller Deployment;
//   - enabled + delivery image: the flag is present;
//   - enabled WITHOUT delivery image: the render FAILS (the sidecar
//     runs the digest-pinned delivery artifact — there is nothing to
//     run otherwise).
//
// helm must be on $PATH (CI); skipped otherwise per the harness rule.

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sidecarChartDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(thisFile)
}

func lookPathHelm() (string, error) { return exec.LookPath("helm") }

func TestChart_AgentdSidecar_DefaultOff(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	// Design 0053 §4.5: both delivery pins are mandatory — synthetic
	// pins so this test isolates the sidecar default.
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	require.NotContains(t, string(out), "--agentd-sidecar=true",
		"default render must not enable the sidecar (single-container mode unchanged)")
}

func TestChart_AgentdSidecar_EnabledWiresFlag(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"--set-string", "controller.agentdSidecar.enabled=true").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	require.Contains(t, string(out), "--agentd-sidecar=true")
	require.Contains(t, string(out), "--agentd-image=")
}

func TestChart_AgentdSidecar_EnabledWithoutImageFailsRender(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdSidecar.enabled=true").CombinedOutput()
	require.Error(t, err, "sidecar without a delivery image must fail the render; output: %s", out)
	require.True(t, strings.Contains(string(out), "agentdDelivery.image") || strings.Contains(string(out), "agentdSidecar.enabled requires"),
		"failure must name the constraint, got: %s", out)
}

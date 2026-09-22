// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// helm/upload_staging_chart_test.go — design 0060 PR 2.5 render pins:
// the values → --upload-staging flag hop, both directions (set → the
// exact flag; default → absent), following agentd_sidecar_chart_test's
// convention. This is the one leg where a values-layer breakage is
// otherwise silent (no values.schema.json; typo'd keys render rc=0).

package chart_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChart_UploadStaging_DefaultRendersNoFlag(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	require.NotContains(t, string(out), "--upload-staging",
		"the all-zero default must render NO flag — agentd's compiled defaults stand")
}

func TestChart_UploadStaging_SetRendersExactFlag(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"--set", "controller.agentdSidecar.uploadStaging.budgetBytes=52428800",
		"--set", "controller.agentdSidecar.uploadStaging.ttlMilliseconds=900000").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	require.Contains(t, string(out), "--upload-staging=budget=52428800,ttlMs=900000",
		"set knobs must render the exact flag (only non-zero fields, in canonical order)")
}

// TestChart_UploadStaging_TypoedValuesKeyIsLoud pins the known values
// limitation's mitigation: a typo'd key under uploadStaging sets a
// NON-canonical path, which Go's --set materializes as an EXTRA key
// the template then carries into the flag as... nothing — rc=0 silent.
// This test pins the CURRENT behavior (absent) so the documented
// limitation is at least observable; the flag-layer typo guard is
// pinned in controller upload_staging_env_test.go.
func TestChart_UploadStaging_TypoedValuesKeySilent(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"--set", "controller.agentdSidecar.uploadStaging.budgetByte=52428800").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	// The documented silent-failure shape: rc=0, flag absent.
	require.False(t, strings.Contains(string(out), "--upload-staging"),
		"a typo'd values key silently renders no flag (the KNOWN limitation; the mitigation is the flag-layer guard + this observable pin)")
}

// TestChart_UploadStaging_ViolatingPairFailsRender pins the render-time
// fail guard: a ttl/applyTimeout pair the sweeper-race class describes
// must fail helm template loudly, never render a crash-looping
// controller.
func TestChart_UploadStaging_ViolatingPairFailsRender(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"--set", "controller.agentdSidecar.uploadStaging.ttlMilliseconds=60000",
		"--set", "controller.agentdSidecar.uploadStaging.applyTimeoutMilliseconds=600000").CombinedOutput()
	require.Error(t, err, "the violating pair must fail render: %s", out)
	require.Contains(t, string(out), "must exceed the effective apply bound",
		"the fail must name the sweeper-race violation")
}

// TestChart_UploadStaging_FloorSentinelRendersExplicitZero pins the
// end-to-end floor=0 disposition: credentialFloorBytes=-1 (the sentinel)
// renders floor=0 on the flag — the explicit-zero setting is
// chart-expressible, not dead code.
func TestChart_UploadStaging_FloorSentinelRendersExplicitZero(t *testing.T) {
	if _, err := lookPathHelm(); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	out, err := exec.Command("helm", "template", "test-release", sidecarChartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion,
		"--set-string", "controller.agentdDelivery.image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"--set-string", "controller.opencodeDelivery.image=ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		"--set", "controller.agentdSidecar.uploadStaging.credentialFloorBytes=-1",
		"--set", "controller.agentdSidecar.uploadStaging.ttlMilliseconds=900000").CombinedOutput()
	require.NoError(t, err, "helm output: %s", out)
	require.Contains(t, string(out), "--upload-staging=floor=0,ttlMs=900000",
		"the -1 sentinel must render an explicit floor=0")
}

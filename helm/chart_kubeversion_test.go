// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Regression guard for the chart's kubeVersion floor (#863, PR #864).
//
// The floor exists because workspace-agentd delivery moves to image
// volumes (KEP-4639, GA in Kubernetes 1.35). Two invariants:
//
//  1. The floor must be >= 1.35. Lowering it reintroduces the need for a
//     controller capability probe that the design explicitly avoids.
//  2. The kind node pinned in local/kind-cluster.yaml must satisfy the
//     floor. The nightly e2e installs this chart via `helm upgrade
//     --install` against that cluster; a floor above the node version
//     makes every nightly run fail (caught in PR #864 review).
//
// Both are exact assertions on purpose: any change to either value should
// require touching this test, which forces the two to move together.

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func readChartKubeVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(chartDir(t) + "/Chart.yaml")
	require.NoError(t, err)
	var chart struct {
		KubeVersion string `json:"kubeVersion"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &chart), "Chart.yaml must parse")
	require.NotEmpty(t, chart.KubeVersion, "Chart.yaml must set kubeVersion")
	return chart.KubeVersion
}

func TestChart_KubeVersionFloor_Is135(t *testing.T) {
	require.Equal(t, ">=1.35.0-0", readChartKubeVersion(t),
		"kubeVersion floor must stay at >=1.35.0-0 (image volumes, KEP-4639, are GA in 1.35 — see #863). "+
			"If you are raising the floor, update this test together with local/kind-cluster.yaml.")
}

func TestChart_KindNode_SatisfiesKubeVersionFloor(t *testing.T) {
	raw, err := os.ReadFile(chartDir(t) + "/../local/kind-cluster.yaml")
	require.NoError(t, err)

	imageRe := regexp.MustCompile(`(?m)^\s*image:\s*"?(kindest/node:v(\d+)\.(\d+)[^"\s]*)"?\s*$`)
	m := imageRe.FindStringSubmatch(string(raw))
	require.NotNil(t, m,
		"local/kind-cluster.yaml must pin an explicit kindest/node image — "+
			"an unpinned cluster floats with the installed kind version and can silently fall below the chart floor (PR #864 review)")
	nodeMajor, err := strconv.Atoi(m[2])
	require.NoError(t, err)
	nodeMinor, err := strconv.Atoi(m[3])
	require.NoError(t, err)

	floorRe := regexp.MustCompile(`>=(\d+)\.(\d+)`)
	f := floorRe.FindStringSubmatch(readChartKubeVersion(t))
	require.NotNil(t, f, "unparseable kubeVersion floor")
	floorMajor, err := strconv.Atoi(f[1])
	require.NoError(t, err)
	floorMinor, err := strconv.Atoi(f[2])
	require.NoError(t, err)

	require.True(t, nodeMajor > floorMajor || (nodeMajor == floorMajor && nodeMinor >= floorMinor),
		"kind node %s (v%d.%d) is below the chart floor v%d.%d — the nightly e2e installs this chart and would fail",
		m[1], nodeMajor, nodeMinor, floorMajor, floorMinor)
	require.True(t, strings.HasPrefix(m[1], "kindest/node:"),
		"unexpected image reference %q", m[1])
}

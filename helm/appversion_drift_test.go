// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// appVersion drift guard (2026-08-15 review of the floating-tag-default
// PR): the chart's appVersion is the fallback tag for the base workspace
// RuntimeEnvironment (runtimeEnvironments.base.image.tag | default
// .Chart.AppVersion), and therefore the image new workspaces launch when
// no operator pin and no instance setting exist. It drifted before —
// bumped in lockstep with releases through v0.8.13, then v0.9.0 bumped
// only chart version, leaving appVersion stale — which would have made
// the tier-4 default launch a base image predating the current release.
//
// Releases are cut by tag push, and the release notes are the CHANGELOG
// section matching the tag — CHANGELOG.md is the source of truth for
// "what is the latest released version". These tests assert:
//
//   - Chart.yaml appVersion == latest versioned section in CHANGELOG.md
//   - the default-rendered base RuntimeEnvironment image tag == the same
//
// The CHANGELOG assertion runs unconditionally; the render test follows
// this package's helm-on-PATH skip convention.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// latestReleasedVersion returns the topmost `## [N.N.N]` version header
// from CHANGELOG.md, skipping `## [Unreleased]`.
func latestReleasedVersion(t *testing.T) string {
	t.Helper()
	changelogPath := filepath.Join(filepath.Dir(chartDir(t)), "CHANGELOG.md")
	data, err := os.ReadFile(changelogPath)
	require.NoError(t, err, "CHANGELOG.md must be readable for the appVersion drift guard")

	re := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)
	m := re.FindStringSubmatch(string(data))
	require.NotNil(t, m, "CHANGELOG.md must contain at least one `## [N.N.N]` version header")
	return m[1]
}

// readChartAppVersion extracts appVersion from Chart.yaml. Parsed with a
// regex rather than a YAML lib to keep this file consistent with the
// package's dependency set; the line shape `appVersion: "X.Y.Z"` is
// stable in this chart.
func readChartAppVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(chartDir(t), "Chart.yaml"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^appVersion:\s*"?(\d+\.\d+\.\d+)"?\s*$`)
	m := re.FindStringSubmatch(string(data))
	require.NotNil(t, m, "Chart.yaml must set appVersion to a bare semver")
	return m[1]
}

func TestChart_AppVersion_MatchesLatestRelease(t *testing.T) {
	want := latestReleasedVersion(t)
	got := readChartAppVersion(t)
	assert.Equal(t, want, got,
		"helm/Chart.yaml appVersion drifted from the latest release (%s).\n"+
			"appVersion is the fallback tag for the base workspace RuntimeEnvironment;\n"+
			"a stale value makes default deployments launch an old base image.\n"+
			"Bump appVersion in the same release commit that adds the CHANGELOG section.", want)
}

func TestChart_DefaultBaseRTE_TagMatchesLatestRelease(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	want := latestReleasedVersion(t)
	docs := helmTemplate(t, "") // default values — the drift scenario
	for _, d := range docs {
		if d["kind"] != "RuntimeEnvironment" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		img, _ := spec["image"].(string)
		assert.True(t, strings.HasSuffix(img, ":"+want),
			"default-rendered base RuntimeEnvironment image must be pinned to the latest release;\n"+
				"got %q, want suffix :%s (appVersion drift?)", img, want)
		return
	}
	t.Fatal("no RuntimeEnvironment CR rendered")
}

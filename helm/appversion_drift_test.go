// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// appVersion drift guard (2026-08-15 review of the floating-tag-default
// PR): the chart's appVersion is the fallback tag for the platform
// component images (api/controller/frontend/mcp — `tag | default
// .Chart.AppVersion`), so a stale appVersion makes a default deploy run
// mixed platform versions. It drifted before — bumped in lockstep with
// releases through v0.8.13, then v0.9.0 bumped only chart version,
// leaving appVersion stale.
//
// The base workspace RuntimeEnvironment used to fall back to appVersion
// too. Design 0053 D5/S4 (2026-08-28) moved the base to content-
// versioned CalVer `YYYY.MM.x` seeded from
// api/internal/imagefactory/catalog.seed.yaml, OFF the platform release
// train — from then on `base:<appVersion>` never existed, and the
// fallback was a fleet-wide ImagePullBackOff waiting to happen
// (incident 2026-09-02, issue #1237). The chart now mirrors the seed
// row's tag (repolint's version-scheme check enforces the mirror) and
// the template fails on an empty tag instead of substituting appVersion.
//
// Releases are cut by tag push, and the release notes are the CHANGELOG
// section matching the tag — CHANGELOG.md is the source of truth for
// "what is the latest released version". These tests assert:
//
//   - Chart.yaml appVersion == latest versioned section in CHANGELOG.md
//   - the default-rendered base RuntimeEnvironment image tag == the
//     catalog seed's default row version (CalVer), and never appVersion
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

// seedDefaultBaseVersion returns the version of the catalog seed's
// isDefault base row — the single source for the base image tag
// (base-image.yml publishes exactly this value; see design 0053 D5/S4).
// Scanned line-wise for the same no-YAML-lib reason as above; the seed
// row shape (`- name:`, indented `version:`, `isDefault: true`) is
// stable.
func seedDefaultBaseVersion(t *testing.T) string {
	t.Helper()
	seedPath := filepath.Join(filepath.Dir(chartDir(t)), "api", "internal", "imagefactory", "catalog.seed.yaml")
	data, err := os.ReadFile(seedPath)
	require.NoError(t, err, "catalog.seed.yaml must be readable for the base-tag guard")

	nameRe := regexp.MustCompile(`^  - name: (\S+)\s*$`)
	fieldRe := regexp.MustCompile(`^    (version|isDefault): (.*)$`)
	var version string
	isDefault := false
	reset := func() {
		version, isDefault = "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if nameRe.MatchString(line) {
			reset()
			continue
		}
		if m := fieldRe.FindStringSubmatch(line); m != nil {
			switch m[1] {
			case "version":
				version = strings.Trim(strings.TrimSpace(m[2]), `"`)
			case "isDefault":
				isDefault = strings.TrimSpace(m[2]) == "true"
			}
			if version != "" && isDefault {
				return version
			}
		}
	}
	require.NotEmpty(t, version, "catalog.seed.yaml must have an isDefault base row with a version")
	return version
}

func TestChart_AppVersion_MatchesLatestRelease(t *testing.T) {
	want := latestReleasedVersion(t)
	got := readChartAppVersion(t)
	assert.Equal(t, want, got,
		"helm/Chart.yaml appVersion drifted from the latest release (%s).\n"+
			"appVersion is the fallback tag for the platform component images\n"+
			"(api/controller/frontend/mcp); a stale value makes default deployments\n"+
			"run mixed platform versions.\n"+
			"Bump appVersion in the same release commit that adds the CHANGELOG section.", want)
}

func TestChart_DefaultBaseRTE_TagIsSeedCalVer(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	want := seedDefaultBaseVersion(t)
	require.Regexp(t, `^\d{4}\.(0[1-9]|1[0-2])\.\d+$`, want,
		"the catalog seed default row must be CalVer YYYY.MM.x (design 0053 D5/S4)")
	docs := helmTemplate(t, "") // default values — the drift scenario
	for _, d := range docs {
		if d["kind"] != "RuntimeEnvironment" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		img, _ := spec["image"].(string)
		assert.True(t, strings.HasSuffix(img, ":"+want),
			"default-rendered base RuntimeEnvironment image must mirror the catalog seed row;\n"+
				"got %q, want suffix :%s (values.yaml/seed drift?)", img, want)
		assert.NotEqual(t, img, "ghcr.io/lenaxia/llmsafespaces/base:"+readChartAppVersion(t),
			"the base must never resolve to the platform appVersion — design 0053 D5/S4 moved it\n"+
				"off the release train (incident 2026-09-02, issue #1237)")
		return
	}
	t.Fatal("no RuntimeEnvironment CR rendered")
}

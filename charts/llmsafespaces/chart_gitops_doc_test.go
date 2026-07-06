// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Regression tests for the FluxCD / Argo CD chart-freshness documentation (#456).
//
// The chart is consumed from a Git source, not published to a Helm/OCI
// registry. Chart.yaml's version is bumped per release, but intermediate
// commits between releases (sha-/, ts-/, dev-tagged image builds) do not
// bump it. FluxCD's source-controller packages a GitRepository-sourced
// chart ONCE and re-uses that packaged artifact as long as the Chart.yaml
// version is unchanged. With the DEFAULT `reconcileStrategy: ChartVersion`,
// that means the chart is packaged exactly once per release tag and never
// re-packaged for the intermediate commits in between — so every
// non-release `helm upgrade` renders against a stale snapshot. New
// templates, new ConfigMap keys (e.g. new migrations), new RBAC — all
// invisible to the cluster until the next release tag.
//
// This trap is silent and already caused a production incident (2026-06-29):
// the migrations ConfigMap still held only migration 000001 after PR #451
// added 000002–000004, because the chart was never re-packaged. The
// migration Job (#455) never actually ran its new args either.
//
// The fix (#456, option 1) is documentation: the chart MUST tell consumers
// to set `reconcileStrategy: Revision` (Flux) so source-controller
// re-packages on every git revision. These tests pin the documentation so a
// future cleanup cannot silently delete the warning.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// chartFile reads a file relative to the chart root.
func chartFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chartDir(t), rel))
	require.NoError(t, err, "chart file %s must exist", rel)
	return string(b)
}

// TestChart_ReadmeDocumentsFluxReconcileStrategy asserts the chart README
// prominently documents the GitOps reconcileStrategy trap and its fix. This
// is the regression guard for the #456 documentation fix: if the section is
// removed, the trap re-emerges for every new consumer.
func TestChart_ReadmeDocumentsFluxReconcileStrategy(t *testing.T) {
	readme := chartFile(t, "README.md")

	// The fix keyword consumers must apply. Assert the LITERAL pair so a stale
	// `reconcileStrategy: ChartVersion` line plus the word "Revision" elsewhere
	// cannot satisfy the guard — that combination is precisely the trap.
	assert.Contains(t, readme, "reconcileStrategy: Revision",
		"README must contain the literal reconcileStrategy: Revision pair (independent tokens would let ChartVersion+Revision pass)")

	// It must explain WHY (otherwise the fix looks arbitrary). The
	// intermediate-commits-don't-bump-version rationale is the root cause
	// and must be called out. We check for concept tokens rather than a
	// hard-coded version literal so the test survives release bumps.
	assert.True(t,
		strings.Contains(strings.ToLower(readme), "intermediate") ||
			strings.Contains(strings.ToLower(readme), "pinned") ||
			strings.Contains(readme, "Chart.yaml"),
		"README must explain the Chart.yaml version cadence (the reason ChartVersion re-packages only once per release)")

	// It must name the affected tooling so operators searching the README can
	// find the section. FluxCD is the primary consumer; Argo CD has the same
	// class of issue.
	for _, term := range []string{"Flux", "GitRepository", "HelmRelease"} {
		assert.Contains(t, readme, term,
			"README must reference %s so the trap is discoverable", term)
	}

	// It must give a copy-pasteable HelmRelease spec (sourceRef is specific to
	// the Flux example; the bare token `chart:` is too common to be a signal).
	assert.Contains(t, readme, "sourceRef:",
		"README must include a HelmRelease chart.spec example with a sourceRef")
}

// TestChart_ChartYamlDescriptionReferencesGitOps asserts Chart.yaml's
// description field flags the GitOps freshness concern. `helm show chart`
// surfaces the description before the README, so it is the first line of
// defense for a consumer who never opens the repo.
func TestChart_ChartYamlDescriptionReferencesGitOps(t *testing.T) {
	desc := chartDescription(t)

	// It must reference GitOps/reconcileStrategy so `helm show chart` warns
	// consumers (#456).
	assert.True(t,
		strings.Contains(desc, "reconcileStrategy") || strings.Contains(desc, "GitOps") ||
			strings.Contains(desc, "Flux"),
		"Chart.yaml description must reference GitOps/reconcileStrategy so `helm show chart` warns consumers (#456)")
}

// chartDescription parses Chart.yaml and returns the `description` field as a
// single flattened string. Uses a real YAML decoder (not hand-rolled
// block-scalar folding) so every scalar indicator (|, >, |-, |+, digits)
// and indentation case is handled correctly. The previous hand-rolled
// parser used strings.Trim(rest, "|>") which treats its argument as a cutset
// and would leave trailing modifier chars (e.g. the '-' in '>-' or the '+'
// in '|+') in the output.
func chartDescription(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(chartDir(t), "Chart.yaml"))
	require.NoError(t, err, "Chart.yaml must exist")
	var meta struct {
		Description string `yaml:"description"`
	}
	require.NoError(t, yaml.Unmarshal(b, &meta), "Chart.yaml must be parseable YAML")
	return meta.Description
}

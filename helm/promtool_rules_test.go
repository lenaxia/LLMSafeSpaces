// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestPromtoolRules (#906 review F1): executes the chart's alert
// expressions against promtool rule-test scenarios (committed at
// tests/alerts_promtool_test.yaml). This is the level that would have
// caught the dead WatchFailing join — an unlabeled gauge joined against
// labeled series can never match — which name-only chart tests shipped
// twice. Skips when promtool or helm is not on PATH (CI installs both;
// the helm-skip pattern from chart_test.go).
func TestPromtoolRules(t *testing.T) {
	if _, err := exec.LookPath("promtool"); err != nil {
		t.Skip("promtool not on PATH; skipping alert-expression tests")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render")
	}

	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var groupsRaw any
	for _, d := range docs {
		if d["kind"] != "PrometheusRule" {
			continue
		}
		spec, ok := d["spec"].(map[string]any)
		require.True(t, ok, "spec must be a map")
		g, ok := spec["groups"]
		require.True(t, ok, "spec.groups must exist")
		groupsRaw = g
		break
	}
	require.NotNil(t, groupsRaw, "PrometheusRule must render with monitoring enabled")

	// promtool accepts a rule file with groups at the root.
	rulesBytes, err := yaml.Marshal(map[string]any{"groups": groupsRaw})
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rendered_rules.yaml"), rulesBytes, 0o600))

	scenario, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "alerts_promtool_test.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alerts_promtool_test.yaml"), scenario, 0o600))

	cmd := exec.Command("promtool", "test", "rules", filepath.Join(dir, "alerts_promtool_test.yaml"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "promtool rule tests failed:\n%s", string(out))
	t.Logf("promtool: %s", string(out))
}

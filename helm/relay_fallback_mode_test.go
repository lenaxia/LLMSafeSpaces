// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// The M2 render pins (design 0061 §4): the fallback-mode knob threads
// chart → api env; the two counter alerts render in the rules.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Flag on (the default) renders BOTH env vars; the fallback mode
// defaults to migration without explicit values.
func TestRelayFallbackMode_EnvRendered(t *testing.T) {
	docs := helmTemplate(t, "") // defaults: relay-only ON, migration
	env := apiEnv(t, apiDeployment(t, docs))
	assert.Equal(t, "true", env["LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED"])
	assert.Equal(t, "migration", env["LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE"],
		"the chart default is migration (the design's merge-time default)")
}

// An explicit strict threads through.
func TestRelayFallbackMode_StrictThreads(t *testing.T) {
	docs := helmTemplate(t, "relayOnlyKeyDelivery:\n  enabled: true\n  fallbackMode: strict\n")
	env := apiEnv(t, apiDeployment(t, docs))
	assert.Equal(t, "strict", env["LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE"])
}

// Flag off (the rollback lever) renders NEITHER env.
func TestRelayFallbackMode_OffRendersNothing(t *testing.T) {
	docs := helmTemplate(t, "relayOnlyKeyDelivery:\n  enabled: false\n")
	env := apiEnv(t, apiDeployment(t, docs))
	assert.NotContains(t, env, "LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE")
}

// Both M2 alerts render with the right expressions.
func TestRelayFallbackMode_AlertsRender(t *testing.T) {
	// monitoring.enabled gates the rules resource (prometheusRules alone
	// is insufficient) — render with monitoring on.
	raw := prometheusRulesRaw(t, "monitoring:\n  enabled: true\n")
	require.NotEmpty(t, raw, "the PrometheusRule resource must render under monitoring")
	assert.Contains(t, raw, "increase(relay_fallback_deliveries_total[10m]) > 0",
		"the stall-detector alert (ANY firing = migration live)")
	assert.Contains(t, raw, "increase(relay_degraded_batches_total[10m]) > 0",
		"the mode-independent fail-closed alert (survives the strict flip)")
}

// prometheusRulesRaw extracts the rendered PrometheusRule resource's
// rule blob (the alerts live there, not in a ConfigMap).
func prometheusRulesRaw(t *testing.T, values string) string {
	t.Helper()
	for _, d := range helmTemplate(t, values) {
		if d["kind"] != "PrometheusRule" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		groups, _ := spec["groups"].([]any)
		var sb strings.Builder
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			rules, _ := gm["rules"].([]any)
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				alert, _ := rm["alert"].(string)
				expr, _ := rm["expr"].(string)
				sb.WriteString(alert + "\n" + expr + "\n")
			}
		}
		return sb.String()
	}
	return ""
}

// The structural walk (the r0-fix record's claimed artifact, now IN the
// tree): every rule in the llm-relay group must be a complete alert
// struct — a re-insertion inside another rule's folded expr (the r0 CI
// class) leaves substrings the Contains pins still match, but a walk of
// the PARSED rules catches it. The local promtool-skip stand-in.
func TestRelayFallbackMode_RulesStructurallyClean(t *testing.T) {
	raw := prometheusRulesRaw(t, "monitoring:\n  enabled: true\n")
	require.NotEmpty(t, raw)
	for _, d := range helmTemplate(t, "monitoring:\n  enabled: true\n") {
		if d["kind"] != "PrometheusRule" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		groups, _ := spec["groups"].([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			if name, _ := gm["name"].(string); name != "llmsafespaces.llm-relay" {
				continue
			}
			rules, _ := gm["rules"].([]any)
			require.NotEmpty(t, rules)
			for i, r := range rules {
				rm, _ := r.(map[string]any)
				alert, _ := rm["alert"].(string)
				require.NotEmpty(t, alert, "rule[%d] must be a complete alert struct", i)
				expr, _ := rm["expr"].(string)
				assert.NotContains(t, expr, "alert:", "rule[%d] (%s): expr contaminated with rule text — the folded-expr insertion class", i, alert)
				assert.NotContains(t, expr, "severity:", "rule[%d] (%s): labels leaked into expr", i, alert)
			}
			sawFallback, sawDegraded := false, false
			for _, r := range rules {
				rm, _ := r.(map[string]any)
				switch rm["alert"] {
				case "RelayFallbackDeliveryActive":
					sawFallback = true
				case "RelayDegradedBatchesActive":
					sawDegraded = true
				}
			}
			assert.True(t, sawFallback && sawDegraded, "both M2 alerts present as complete rules")
		}
	}
}

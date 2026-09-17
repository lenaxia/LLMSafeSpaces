// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findDocs returns the rendered docs whose kind matches.
func findDocs(t *testing.T, docs []map[string]any, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

func docName(t *testing.T, doc map[string]any) string {
	t.Helper()
	meta, ok := doc["metadata"].(map[string]any)
	require.True(t, ok, "doc has metadata: %v", doc)
	return meta["name"].(string)
}

// TestRelayOnlyKeyDelivery_DisabledByDefault: with the flag off (the
// default until US-72.5) zero llm-relay resources render — flag off means
// zero behavior change.
func TestRelayOnlyKeyDelivery_DisabledByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, d := range docs {
		if d["kind"] != "Namespace" {
			if meta, ok := d["metadata"].(map[string]any); ok {
				if name, isStr := meta["name"].(string); isStr && name != "" {
					assert.NotContains(t, name, "llm-relay", "llm-relay resource rendered with flag off: %s", name)
				}
			}
		}
		if d["kind"] == "Namespace" {
			meta := d["metadata"].(map[string]any)
			assert.NotEqual(t, "llm-relay", meta["name"])
		}
	}
}

// TestRelayOnlyKeyDelivery_EnabledRendersFleet: flag on renders the full
// US-72.2 shape — namespace, Deployment ×2 + PDB(1), Service, NetworkPolicy,
// and the §4.3 RBAC split (router: read + name-scoped write on exactly the
// two keypair Secrets; controller: write others + get pub only).
func TestRelayOnlyKeyDelivery_EnabledRendersFleet(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
`)

	names := map[string]bool{}
	for _, d := range docs {
		if _, ok := d["metadata"]; ok {
			names[docName(t, d)] = true
		}
	}
	for _, want := range []string{
		"llm-relay", "llm-relay-router", "llm-relay-router-allow-workspaces",
		"llm-relay-router-secrets", "llmsafespaces-controller-llm-relay",
	} {
		assert.True(t, names[want], "missing rendered resource %q", want)
	}

	// Deployment shape: 2 replicas, ROUTER_MODE=byo, sized grace period.
	for _, dep := range findDocs(t, docs, "Deployment") {
		if docName(t, dep) != "llm-relay-router" {
			continue
		}
		spec := dep["spec"].(map[string]any)
		assert.EqualValues(t, 2, spec["replicas"])
		pod := spec["template"].(map[string]any)
		assert.EqualValues(t, 630, pod["spec"].(map[string]any)["terminationGracePeriodSeconds"])
		containers := pod["spec"].(map[string]any)["containers"].([]any)
		env := containers[0].(map[string]any)["env"].([]any)
		modes := map[string]string{}
		for _, e := range env {
			kv := e.(map[string]any)
			if name, ok := kv["name"].(string); ok {
				if value, ok := kv["value"].(string); ok {
					modes[name] = value
				}
			}
		}
		assert.Equal(t, "byo", modes["ROUTER_MODE"])
	}

	// RBAC: the router's write carve-out covers exactly the two keypair
	// Secrets; the controller can write others and only get the pub one.
	for _, role := range findDocs(t, docs, "Role") {
		switch docName(t, role) {
		case "llm-relay-router-secrets":
			rules := role["rules"].([]any)
			var writeNames []string
			hasRead := false
			for _, r := range rules {
				rule := r.(map[string]any)
				verbs := toStrings(t, rule["verbs"])
				if relayTestContains(verbs, "update") {
					writeNames = append(writeNames, toStrings(t, rule["resourceNames"])...)
				}
				if relayTestContains(verbs, "get") && relayTestContains(verbs, "watch") {
					hasRead = true
				}
			}
			assert.True(t, hasRead, "router must get/list/watch Secrets")
			assert.ElementsMatch(t, []string{"llm-relay-hpke-key", "llm-relay-hpke-pub"}, writeNames,
				"the write carve-out covers exactly the two keypair Secrets")
		case "llmsafespaces-controller-llm-relay":
			rules := role["rules"].([]any)
			for _, r := range rules {
				rule := r.(map[string]any)
				verbs := toStrings(t, rule["verbs"])
				if names, ok := rule["resourceNames"]; ok {
					assert.ElementsMatch(t, []string{"llm-relay-hpke-pub"}, toStrings(t, names),
						"controller's name-scoped rule is get-on-pub only")
					assert.ElementsMatch(t, []string{"get"}, verbs)
				} else {
					assert.NotContains(t, verbs, "get", "controller's general llm-relay Secret rule must not read (pub-only read, name-scoped)")
				}
			}
		}
	}
}

// TestRelayOnlyKeyDelivery_EgressCarveoutFunctional (iteration 2): the
// carve-out must select REAL workspace pods (app+component labels, the
// same pair every chart policy uses), allow the post-DNAT POD port 8090,
// and gate on the master networkPolicy toggle.
func TestRelayOnlyKeyDelivery_EgressCarveoutFunctional(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
`)
	var carveout map[string]any
	for _, d := range findDocs(t, docs, "NetworkPolicy") {
		if docName(t, d) == "llm-relay-egress-carveout" {
			carveout = d
		}
	}
	require.NotNil(t, carveout, "egress carve-out must render with the flag on")

	spec := carveout["spec"].(map[string]any)
	sel := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	assert.Equal(t, "llmsafespaces", sel["app"], "selects the real workspace pod label")
	assert.Equal(t, "workspace", sel["component"])

	egress := spec["egress"].([]any)[0].(map[string]any)
	port := egress["ports"].([]any)[0].(map[string]any)
	assert.EqualValues(t, 8090, port["port"], "post-DNAT pod port, not the Service port")
	to := egress["to"].([]any)[0].(map[string]any)
	podSel := to["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	assert.Equal(t, "llm-relay-router", podSel["app.kubernetes.io/name"])
}

// TestRelayOnlyKeyDelivery_RendersWithMonitoring (review R1): enabling the
// alerts alongside relay-only must render — the two flags together are the
// US-72.5 flip-gate posture.
func TestRelayOnlyKeyDelivery_RendersWithMonitoring(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
monitoring:
  enabled: true
  prometheusRules:
    enabled: true
`)
	found := 0
	for _, d := range docs {
		if d["kind"] == "PrometheusRule" {
			spec := d["spec"].(map[string]any)
			groups := spec["groups"].([]any)
			for _, g := range groups {
				if g.(map[string]any)["name"] == "llmsafespaces.llm-relay" {
					found++
				}
			}
		}
	}
	require.Equal(t, 1, found, "llm-relay alert group present exactly once with monitoring on")
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	require.True(t, ok, "not a list: %v", v)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}

func relayTestContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

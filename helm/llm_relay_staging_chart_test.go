// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// llm_relay_staging_chart_test.go — US-72.3 controller-side chart wiring:
// the relayOnlyKeyDelivery flag/guard/namespace on the controller
// Deployment (the router-side fleet tests live in llm_relay_chart_test.go).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func controllerDeployment(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == "Deployment" {
			if meta, ok := d["metadata"].(map[string]any); ok {
				if name, _ := meta["name"].(string); len(name) > len("llmsafespaces-controller") && name[len(name)-len("llmsafespaces-controller"):] == "llmsafespaces-controller" {
					return d
				}
			}
		}
	}
	t.Fatal("controller Deployment not rendered")
	return nil
}

func controllerArgs(t *testing.T, dep map[string]any) []string {
	t.Helper()
	spec := dep["spec"].(map[string]any)
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	args := containers[0].(map[string]any)["args"].([]any)
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, a.(string))
	}
	return out
}

// TestRelayStaging_ControllerArgsOffByDefault: flag off (the chart default
// until US-72.5) wires NOTHING — zero behavior change.
func TestRelayStaging_ControllerArgsOffByDefault(t *testing.T) {
	dep := controllerDeployment(t, helmTemplate(t, ""))
	for _, arg := range controllerArgs(t, dep) {
		assert.NotContains(t, arg, "relay-only-key-delivery", "flag-off render must not wire relay staging")
		assert.NotContains(t, arg, "llm-relay", "flag-off render must not reference llm-relay")
	}
}

// TestRelayStaging_ControllerArgsWired: flag on wires the four controller
// flags — enabled, router URL, namespace, token TTL (defaults from values).
func TestRelayStaging_ControllerArgsWired(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
`)
	dep := controllerDeployment(t, docs)
	args := controllerArgs(t, dep)
	joined := ""
	for _, a := range args {
		joined += a + "\n"
	}
	assert.Contains(t, joined, "--relay-only-key-delivery=true")
	assert.Contains(t, joined, "--llm-relay-router-url=http://llm-relay-router.llm-relay.svc.cluster.local")
	assert.Contains(t, joined, "--llm-relay-namespace=llm-relay")
	assert.Contains(t, joined, "--relay-token-ttl=24h")
}

// TestRelayStaging_NamespaceOverrideThreadsThrough: a custom namespace
// flows to BOTH the fleet resources and the controller flag.
func TestRelayStaging_NamespaceOverrideThreadsThrough(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
  namespace: "relay-ns-723"
`)
	dep := controllerDeployment(t, docs)
	assert.Contains(t, controllerArgs(t, dep), "--llm-relay-namespace=relay-ns-723")
	found := false
	for _, d := range docs {
		if meta, ok := d["metadata"].(map[string]any); ok {
			if name, _ := meta["name"].(string); name == "llm-relay-router" {
				found = true
				assert.Equal(t, "relay-ns-723", meta["namespace"])
			}
		}
	}
	assert.True(t, found, "router deployment must render in the custom namespace")
}

// TestRelayStaging_GuardFailsRenderWithoutRouterURL: enabled with an EMPTY
// router URL must FAIL the render (install-time half of the fail-loud
// guard; the runtime probe is the other half).
func TestRelayStaging_GuardFailsRenderWithoutRouterURL(t *testing.T) {
	require.Error(t, helmTemplateErr(t, `
relayOnlyKeyDelivery:
  enabled: true
  workspaceRouterURL: ""
`), "render must fail: the runtime startup guard would refuse this configuration")
}

// TestRelayStaging_TTLOverrideThreadsThrough.
func TestRelayStaging_TTLOverrideThreadsThrough(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
  tokenTTL: "12h"
`)
	assert.Contains(t, controllerArgs(t, controllerDeployment(t, docs)), "--relay-token-ttl=12h")
}

// TestRelayStaging_ClusterScopeRefusesRender (review r5 finding 3):
// rbac.scope=cluster grants the controller cluster-wide Secret
// get/list/watch (the informer cache grant), which the llm-relay Role
// cannot subtract — with relay-only enabled it would replicate the keypair
// Secret into controller memory, defeating the §4.3 no-read guarantee.
// The combination must FAIL the render; the default namespace scope
// renders.
func TestRelayStaging_ClusterScopeRefusesRender(t *testing.T) {
	require.Error(t, helmTemplateErr(t, `
relayOnlyKeyDelivery:
  enabled: true
rbac:
  scope: cluster
`), "relay-only + cluster scope must refuse to render")

	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
rbac:
  scope: namespace
`)
	assert.NotEmpty(t, docs, "the default namespace scope renders with relay-only")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// relay_router_drain_1732_chart_test.go — #1732 leg A1 chart pins: the
// fleet relay-router Deployment must carry deploy-roll drain hardening
// at BYO parity (#1078): preStop endpoint-removal head start,
// terminationGracePeriodSeconds sized to minute-long generations, and a
// DRAIN_GRACE strictly below the TGS. The 2026-10-17 v0.38.0 roll
// hard-cut every in-flight generation (no preStop, default 30s TGS,
// 10s in-process shutdown window) — the incident's halt leg.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fleetRouterPodSpec1732(t *testing.T, valuesYAML string) map[string]any {
	t.Helper()
	// Namespace scope + watchNamespaces satisfies the fleet informer's
	// install-time guard (rbac.yaml) while keeping the default scope the
	// relayOnlyKeyDelivery guard mandates (llm-relay-rbac.yaml).
	enable := `
controller:
  watchNamespaces:
    - default
  enabled: true
  inferenceRelay:
    enabled: true
`
	docs := helmTemplate(t, enable+valuesYAML)
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"].(string)
		if name != "relay-router" {
			continue
		}
		spec := doc["spec"].(map[string]any)
		return spec["template"].(map[string]any)["spec"].(map[string]any)
	}
	t.Fatal("relay-router Deployment not rendered")
	return nil
}

func fleetRouterContainer1732(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	for _, c := range pod["containers"].([]any) {
		cm := c.(map[string]any)
		if cm["name"] == "relay-router" {
			return cm
		}
	}
	t.Fatal("relay-router container not found")
	return nil
}

func drainGraceSeconds1732(t *testing.T, s string) float64 {
	t.Helper()
	d, err := time.ParseDuration(s)
	require.NoError(t, err, "DRAIN_GRACE must be a Go duration: %q", s)
	return d.Seconds()
}

func Test1732_FleetRouterRendersPreStopAndDrainGrace(t *testing.T) {
	pod := fleetRouterPodSpec1732(t, "")
	container := fleetRouterContainer1732(t, pod)

	// preStop: the endpoint-removal head start before the drain.
	lifecycle, ok := container["lifecycle"].(map[string]any)
	require.True(t, ok, "the router container must carry a preStop lifecycle hook (#1078 parity)")
	preStop, ok := lifecycle["preStop"].(map[string]any)
	require.True(t, ok)
	sleep, ok := preStop["sleep"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(5), sleep["seconds"], "preStop sleep 5s — endpoint removal precedes the drain")

	// DRAIN_GRACE env: default 10m.
	var drain string
	for _, e := range container["env"].([]any) {
		em := e.(map[string]any)
		if em["name"] == "DRAIN_GRACE" {
			drain = em["value"].(string)
		}
	}
	require.NotEmpty(t, drain, "DRAIN_GRACE env must render")
	assert.Equal(t, float64(600), drainGraceSeconds1732(t, drain), "default drain grace = 10m (#1078 parity)")

	// TGS strictly exceeds the drain bound (the SIGKILL backstop must
	// never fire before the drain cap expires).
	tgs, ok := pod["terminationGracePeriodSeconds"].(float64)
	require.True(t, ok, "terminationGracePeriodSeconds must render on the pod spec")
	assert.Equal(t, float64(630), tgs)
	assert.Greater(t, tgs, drainGraceSeconds1732(t, drain),
		"TGS must exceed DRAIN_GRACE — otherwise the pod is SIGKILLed mid-drain")
}

func Test1732_FleetRouterDrainKnobsOverride(t *testing.T) {
	pod := fleetRouterPodSpec1732(t, `
    router:
      drainGrace: "90s"
      terminationGracePeriodSeconds: 120
`)
	container := fleetRouterContainer1732(t, pod)
	var drain string
	for _, e := range container["env"].([]any) {
		em := e.(map[string]any)
		if em["name"] == "DRAIN_GRACE" {
			drain = em["value"].(string)
		}
	}
	require.Equal(t, "90s", drain)
	tgs := pod["terminationGracePeriodSeconds"].(float64)
	assert.Equal(t, float64(120), tgs)
	assert.Greater(t, tgs, drainGraceSeconds1732(t, drain))

}

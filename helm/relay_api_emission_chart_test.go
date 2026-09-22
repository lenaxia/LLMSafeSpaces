// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// relay_api_emission_chart_test.go — US-72.4 API-side chart wiring: the
// relayOnlyKeyDelivery flag reaching the api Deployment as
// LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED (the env the config's
// BindEnv reads). Flag off (default) wires nothing — byte-identical
// legacy batches; relayOnlyKeyDelivery.api.enabled=false is the
// builder-only rollback lever (controller staging + router stay up).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func apiDeployment(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == "Deployment" {
			if meta, ok := d["metadata"].(map[string]any); ok {
				if name, _ := meta["name"].(string); len(name) > len("-api") && name[len(name)-len("-api"):] == "-api" {
					return d
				}
			}
		}
	}
	t.Fatal("api Deployment not rendered")
	return nil
}

func apiEnv(t *testing.T, dep map[string]any) map[string]string {
	t.Helper()
	spec := dep["spec"].(map[string]any)
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	envs := containers[0].(map[string]any)["env"].([]any)
	out := map[string]string{}
	for _, e := range envs {
		env := e.(map[string]any)
		if name, ok := env["name"].(string); ok {
			if v, ok := env["value"].(string); ok {
				out[name] = v
			}
		}
	}
	return out
}

// TestRelayEmission_APIEnvOffWhenExplicitlyDisabled (US-72.5 re-target):
// the chart default is now ON; the env-absent posture is the explicit
// rollback lever, still asserted.
func TestRelayEmission_APIEnvOffWhenExplicitlyDisabled(t *testing.T) {
	env := apiEnv(t, apiDeployment(t, helmTemplate(t, "relayOnlyKeyDelivery:\n  enabled: false\n")))
	assert.NotContains(t, env, "LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED")
}

// TestRelayEmission_APIEnvOnByDefault (US-72.5): the flipped default
// wires the builder env.
func TestRelayEmission_APIEnvOnByDefault(t *testing.T) {
	env := apiEnv(t, apiDeployment(t, helmTemplate(t, "")))
	assert.Equal(t, "true", env["LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED"])
}

// TestRelayEmission_APIEnvWired: flag on wires the env the config's
// BindEnv expects.
func TestRelayEmission_APIEnvWired(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
`)
	env := apiEnv(t, apiDeployment(t, docs))
	assert.Equal(t, "true", env["LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED"])
}

// TestRelayEmission_APIRollbackLever: api.enabled=false under the main
// flag keeps controller staging up (see the US-72.3 chart tests) while
// the builder env is absent — raw-key batches resume (design §6.4).
func TestRelayEmission_APIRollbackLever(t *testing.T) {
	docs := helmTemplate(t, `
relayOnlyKeyDelivery:
  enabled: true
  api:
    enabled: false
`)
	env := apiEnv(t, apiDeployment(t, docs))
	assert.NotContains(t, env, "LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED")
	// The controller legs stay armed (the narrow lever is API-only).
	dep := controllerDeployment(t, docs)
	joined := ""
	for _, a := range controllerArgs(t, dep) {
		joined += a + "\n"
	}
	assert.Contains(t, joined, "--relay-only-key-delivery=true")
	require.NotEmpty(t, joined)
}

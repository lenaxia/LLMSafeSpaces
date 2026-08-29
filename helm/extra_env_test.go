// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiDeploymentFrom finds the rendered api Deployment doc.
func apiDeploymentFrom(t *testing.T, valuesYAML string) map[string]any {
	t.Helper()
	for _, d := range helmTemplate(t, valuesYAML) {
		if d["kind"] != "Deployment" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		if name, _ := meta["name"].(string); name == "test-release-llmsafespaces-api" {
			return d
		}
	}
	t.Fatal("api Deployment not rendered")
	return nil
}

func apiEnvNamesAndValues(t *testing.T, valuesYAML string) map[string]string {
	t.Helper()
	dep := apiDeploymentFrom(t, valuesYAML)
	spec := dep["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	pod := template["spec"].(map[string]any)
	containers := pod["containers"].([]any)
	c := containers[0].(map[string]any)
	out := map[string]string{}
	for _, e := range c["env"].([]any) {
		env, _ := e.(map[string]any)
		name, _ := env["name"].(string)
		value, _ := env["value"].(string) // secretKeyRef entries carry none
		if name != "" {
			out[name] = value
		}
	}
	return out
}

// TestAPIExtraEnv_Render verifies the api.extraEnv passthrough: entries
// land as quoted env vars alongside the hardcoded ones; an absent/empty
// list renders nothing extra.
func TestAPIExtraEnv_Render(t *testing.T) {
	t.Run("empty by default — only hardcoded env present", func(t *testing.T) {
		env := apiEnvNamesAndValues(t, "")
		assert.Contains(t, env, "CONFIG_PATH")
		assert.NotContains(t, env, "OPENCODE_V2_DELIVERY",
			"the flag must stay off unless explicitly set — no accidental V2 delivery")
	})

	t.Run("entries render and values are quoted", func(t *testing.T) {
		env := apiEnvNamesAndValues(t, `
api:
  extraEnv:
    - name: OPENCODE_V2_DELIVERY
      value: "1"
    - name: SOME_TOKEN
      value: "a b c"
`)
		assert.Equal(t, "1", env["OPENCODE_V2_DELIVERY"])
		assert.Equal(t, "a b c", env["SOME_TOKEN"], "values with spaces survive quoting")
		assert.Contains(t, env, "CONFIG_PATH", "hardcoded env untouched")
		require.NotNil(t, env)
	})
}

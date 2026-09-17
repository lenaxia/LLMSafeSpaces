// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Base-tag render gate (issue #1237, incident 2026-09-02): the base is
// CalVer off the platform train (design 0053 D5/S4) — the template must
// NEVER substitute .Chart.AppVersion for an empty tag (base:<platform
// semver> does not exist; a default or explicitly-empty tag then breaks
// every workspace pod with ImagePullBackOff). An empty tag fails the
// render with an operator-actionable message; a digest pin or an
// explicit tag renders. Same convention as delivery_pins_gate_test.go.
func TestBaseTag_MandatoryRenderGate(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	render := func(t *testing.T, values string) ([]byte, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "values.yaml")
		require.NoError(t, os.WriteFile(path, []byte(values), 0o644))
		return helmTemplateRaw(t, path)
	}

	const pins = `
controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  opencodeDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`

	t.Run("explicitly empty tag fails the render", func(t *testing.T) {
		out, err := render(t, pins+`
runtimeEnvironments:
  base:
    image:
      tag: ""
`)
		require.Error(t, err, "render must fail on an empty base tag")
		assert.True(t, strings.Contains(string(out), "runtimeEnvironments.base.image.tag is mandatory"),
			"render output should name the mandatory base tag, got: %s", out)
		assert.True(t, strings.Contains(string(out), "design 0053"),
			"the failure must point at the design decision, got: %s", out)
	})

	t.Run("explicit tag renders repo:tag", func(t *testing.T) {
		docs := helmTemplate(t, pins+`
runtimeEnvironments:
  base:
    image:
      repository: registry.example.com/base
      tag: "2026.09.0"
`)
		for _, d := range docs {
			if d["kind"] != "RuntimeEnvironment" {
				continue
			}
			spec, _ := d["spec"].(map[string]any)
			assert.Equal(t, "registry.example.com/base:2026.09.0", spec["image"],
				"an explicit tag must render verbatim — no appVersion substitution")
			return
		}
		t.Fatal("no RuntimeEnvironment CR rendered")
	})

	t.Run("digest pin wins over empty tag (no fail)", func(t *testing.T) {
		out, err := render(t, pins+`
runtimeEnvironments:
  base:
    image:
      repository: registry.example.com/base
      tag: ""
      digest: "sha256:def"
`)
		require.NoError(t, err, "a digest-pinned base must render regardless of tag: %s", out)
		assert.Contains(t, string(out), "registry.example.com/base@sha256:def")
	})
}

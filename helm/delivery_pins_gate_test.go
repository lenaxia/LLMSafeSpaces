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

// Design 0053 §4.5 (S3): both delivery pins are MANDATORY at render
// time. An empty image must fail the render with an operator-actionable
// message — the base image carries no platform binaries, so an
// unpinned controller would build pods that PATH-look up binaries that
// no longer exist. Controller startup enforces the same contract
// (ValidateAgentdDelivery / ValidateOpencodeDelivery).
func TestDeliveryPins_MandatoryRenderGate(t *testing.T) {
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

	for _, tc := range []struct {
		name      string
		values    string
		wantErrOn string
	}{
		{
			name:      "no pins at all",
			values:    "",
			wantErrOn: "agentdDelivery.image is mandatory",
		},
		{
			name: "agentd pinned, opencode missing",
			values: `
controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`,
			wantErrOn: "opencodeDelivery.image is mandatory",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := render(t, tc.values)
			require.Error(t, err, "render must fail without both pins")
			assert.True(t, strings.Contains(string(out), tc.wantErrOn),
				"render output should mention %q, got: %s", tc.wantErrOn, out)
		})
	}
}

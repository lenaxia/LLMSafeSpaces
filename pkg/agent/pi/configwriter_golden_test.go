// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Golden-file renders: one full-source Apply (the same AgentConfigInput
// the platform's existing callers compose — providers formatted in the
// opencode shape, relay with a request-time credential key, model
// selection, staged MCP servers, admin prompt) must produce
// byte-stable models.json / mcp.json / auth.json / settings.json /
// APPEND_SYSTEM.md. The goldens pin pi's file SHAPES (field names,
// nesting, unit conversions) as cited from pi's docs — any diff here is
// a deliberate shape change that must be re-validated against
// pi.dev/docs, never an accident.
//
// Regenerate (deliberate shape change only):
//
//	REGEN_GOLDEN=1 go test -run TestConfigWriter_GoldenRenders ./pkg/agent/pi/
//
// then review the diff against the pi docs cited in configwriter.go.

var goldenFiles = []string{
	"models.json",
	"auth.json",
	"mcp.json",
	"settings.json",
	"APPEND_SYSTEM.md",
}

func applyGoldenInput(t *testing.T, dir string) {
	t.Helper()
	w := NewConfigWriter(dir,
		WithRelayAPIKey("!cat /sandbox-runtime/rt/relay-token"),
		WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))

	model := agent.ModelSelection("acme/acme-large")
	restart, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
		Relay: &agent.RelayState{
			URL: "https://relay.example.test/v1",
			Models: []agent.RelayModel{
				{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
				{ID: "glm-5-air-free", Name: "GLM-5 Air Free", ContextLimit: 128000, OutputLimit: 64000},
			},
		},
		Model: &model,
		MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
			{
				Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${DOCS_TOKEN}"}, TimeoutMs: 2500,
			},
			{
				Name: "filesystem", Transport: "stdio", Command: "npx",
				Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
				Env:  map[string]string{"CI": "1"},
			},
		}},
		AdminPrompt: &agent.AdminPromptChange{Text: "You are running inside an LLMSafeSpaces workspace.\nPlatform canary: golden-prompt.\n"},
		// AllowedDirs deliberately included in the golden input even
		// though pi drops it — the golden run pins the DROP too.
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/tmp/*"}},
	})
	require.NoError(t, err)
	assert.True(t, restart, "providers/model/mcp/prompt are all restart-bounded sources")
}

func TestConfigWriter_GoldenRenders(t *testing.T) {
	dir := t.TempDir()
	applyGoldenInput(t, dir)

	for _, name := range goldenFiles {
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "rendered %s", name)

		wantPath := filepath.Join("testdata", "golden", name+".want")
		if os.Getenv("REGEN_GOLDEN") == "1" {
			require.NoError(t, os.MkdirAll(filepath.Dir(wantPath), 0o755))
			require.NoError(t, os.WriteFile(wantPath, got, 0o640))
			continue
		}
		want, err := os.ReadFile(wantPath)
		require.NoError(t, err, "golden missing — run: REGEN_GOLDEN=1 go test -run TestConfigWriter_GoldenRenders ./pkg/agent/pi/ and review the diff against the pi docs")
		assert.Equal(t, string(want), string(got), "render drift vs pinned golden %s — deliberate shape change only (re-validate against pi docs)", name)
	}

	// Negative pins: files pi must never see from this writer.
	assert.NoFileExists(t, filepath.Join(dir, "SYSTEM.md"), "SYSTEM.md would REPLACE pi's own system prompt")
	assert.NoFileExists(t, filepath.Join(dir, modelResolutionWarningFile), "acme/acme-large resolves against the rendered provider set")

	// The golden input carries AllowedDirs — pinned as dropped (no
	// permission artifact anywhere in the agent dir).
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	rendered := map[string]bool{}
	for _, e := range entries {
		rendered[e.Name()] = true
	}
	expected := map[string]bool{
		"models.json": true, "auth.json": true, "mcp.json": true,
		"settings.json": true, "APPEND_SYSTEM.md": true,
	}
	for name := range rendered {
		assert.True(t, expected[name], "unexpected artifact %s — AllowedDirs must render nothing", name)
	}
}

// TestConfigWriter_GoldenRendersAreIdempotent pins the boot-normalize
// contract (ensureBootAgentConfig parity): re-applying an empty input —
// and re-constructing a writer over the rendered directory — reproduces
// byte-identical files.
func TestConfigWriter_GoldenRendersAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	applyGoldenInput(t, dir)

	before := map[string]string{}
	for _, name := range goldenFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		before[name] = string(data)
	}

	// Empty Apply on the same writer (the boot normalize call).
	w := NewConfigWriter(dir,
		WithRelayAPIKey("!cat /sandbox-runtime/rt/relay-token"),
		WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	// Fresh construction over the same dir (a writer restart) +
	// normalize: sources must round-trip from the rendered files.
	w2 := NewConfigWriter(dir,
		WithRelayAPIKey("!cat /sandbox-runtime/rt/relay-token"),
		WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err = w2.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	assert.True(t, w2.HasRelay(), "relay round-trips from a rendered models.json")

	for _, name := range goldenFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, before[name], string(data), "%s must round-trip byte-identically through a writer restart", name)
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	dsecrets "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMaterialize_MCPServers_RenderedToAgentConfig is a unit-level e2e test
// for the MCP materialization chain: it stages MCP server entries (as the
// Materializer would after processing secrets.json) and verifies
// applyMCPServersToConfig writes the expected opencode mcp section into
// agent-config.json.
//
// This validates: StagedMCPServer → applyMCPServersToConfig → JSON output.
// The full LoadSecretsFile → Materialize.applyMCPServer path is covered by
// pkg/agentd/secrets/mcp_test.go; the schema validation is covered by
// agent_config_writer_schema_test.go.
func TestMaterialize_MCPServers_RenderedToAgentConfig(t *testing.T) {
	dir := t.TempDir()
	agentCfgPath := filepath.Join(dir, "agent-config.json")

	staged := []dsecrets.StagedMCPServer{
		{
			Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok123"},
		},
		{
			Name: "github", Transport: "stdio", Command: "npx",
			Args: []string{"-y", "server-github"},
			Env:  map[string]string{"GITHUB_TOKEN": "ghp_secret"},
		},
	}

	// Write a base agent-config (as FlushProviders would).
	base := `{"$schema":"https://opencode.ai/config.json"}`
	require.NoError(t, os.WriteFile(agentCfgPath, []byte(base), 0o600))

	applyMCPServersToConfig(agentCfgPath, staged)

	// Read and verify the output.
	data, err := os.ReadFile(agentCfgPath)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))

	mcpRaw, ok := cfg["mcp"]
	require.True(t, ok, "mcp section must be present")

	var mcp map[string]map[string]any
	require.NoError(t, json.Unmarshal(mcpRaw, &mcp))

	// Remote server.
	wiki := mcp["wiki"]
	require.NotNil(t, wiki)
	assert.Equal(t, "remote", wiki["type"])
	assert.Equal(t, "https://wiki.example.com/mcp", wiki["url"])
	assert.Equal(t, true, wiki["enabled"])
	headers := wiki["headers"].(map[string]any)
	assert.Equal(t, "Bearer tok123", headers["Authorization"])

	// Stdio server.
	gh := mcp["github"]
	require.NotNil(t, gh)
	assert.Equal(t, "local", gh["type"])
	cmd := gh["command"].([]any)
	assert.Equal(t, "npx", cmd[0])
	assert.Equal(t, "-y", cmd[1])
	assert.Equal(t, "server-github", cmd[2])
	env := gh["environment"].(map[string]any)
	assert.Equal(t, "ghp_secret", env["GITHUB_TOKEN"])
}

// TestMaterialize_NoMCPSectionWhenSecretsJSONHasNoMCPEntries verifies that
// a secrets.json with only llm-provider entries produces no mcp section.
func TestMaterialize_NoMCPSectionWhenSecretsJSONHasNoMCPEntries(t *testing.T) {
	dir := t.TempDir()
	agentCfgPath := filepath.Join(dir, "agent-config.json")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`
	require.NoError(t, os.WriteFile(agentCfgPath, []byte(base), 0o600))

	// No MCP servers staged.
	applyMCPServersToConfig(agentCfgPath, nil)

	data, err := os.ReadFile(agentCfgPath)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))

	_, hasMCP := cfg["mcp"]
	assert.False(t, hasMCP, "mcp section must NOT be present when no servers staged")

	// Provider section must survive unchanged.
	_, hasProvider := cfg["provider"]
	assert.True(t, hasProvider, "provider section must be preserved")
}

// TestApplyMCPServersToConfig_SchemaValidation verifies the rendered output
// passes the opencode schema validator (closes the full contract loop).
func TestApplyMCPServersToConfig_SchemaValidation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")

	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600))

	servers := []dsecrets.StagedMCPServer{
		{Name: "remote-http", Transport: "http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
		{Name: "remote-sse", Transport: "sse", URL: "https://example.com/sse"},
		{Name: "local-stdio", Transport: "stdio", Command: "npx", Args: []string{"-y", "server"}, Env: map[string]string{"TOKEN": "secret"}},
	}

	applyMCPServersToConfig(cfgPath, servers)

	assertMatchesOpencodeSchema(t, cfgPath)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Regression (2026-08-15 incident, v0.14+): the llmsafespaces MCP entry,
// admin system prompt, and allowed external dirs are stamped by the
// writer's marshal path (preMarshalHook + prompt/dirs merges) — but the
// writer only writes when a write path fires (pre-boot relay, relay
// injector, credential reload). When all of those skip (no free-models
// catalog, injector fetch failure, no user reload), agent-config.json
// stayed at the materialize base ({$schema, provider, model}) and the
// workspace ran with no MCP server and no platform system prompt. The
// fix is an empty-input Apply at agentd boot, before opencode starts:
// re-marshal current sources + stamp the missing blocks. These tests pin
// that Apply(empty) semantics.
func TestConfigWriter_ApplyEmpty_StampsMissingBlocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test", "baseURL": "https://api.openai.com/v1"}}
		},
		"model": "openai/gpt-4o"
	}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("PLATFORM PROMPT"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := NewConfigWriter(path,
		WithAdminPromptPath(promptPath),
		WithAllowedDirsPath(dirsPath),
		WithPreMarshalHook(fakeBuiltinMCPHook),
	)

	restartRequired, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	assert.True(t, restartRequired, "empty Apply still writes; opencode must (re)start on it")

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Schema   string `json:"$schema"`
		Provider map[string]json.RawMessage `json:"provider"`
		Model    string                     `json:"model"`
		Agent    struct {
			Build struct {
				Prompt string `json:"prompt"`
			} `json:"build"`
		} `json:"agent"`
		Mode struct {
			Permissions struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.Provider, "openai", "provider source must survive empty Apply")
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model source must survive empty Apply")
	assert.Equal(t, "PLATFORM PROMPT", cfg.Agent.Build.Prompt, "admin prompt must be stamped")
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/tmp/*"], "allowed dirs must be stamped")
	assert.Contains(t, cfg.MCP, "llmsafespaces", "built-in MCP server must be stamped")
}

// Idempotency: a second empty Apply (e.g. boot normalize runs after the
// pre-boot relay already wrote everything) must not corrupt or duplicate.
func TestConfigWriter_ApplyEmpty_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"sk-test"}}},"model":"openai/gpt-4o"}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("P"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte("/tmp/*\n"), 0o600))

	w := NewConfigWriter(path,
		WithAdminPromptPath(promptPath),
		WithAllowedDirsPath(dirsPath),
		WithPreMarshalHook(fakeBuiltinMCPHook),
	)
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.JSONEq(t, string(first), string(second), "second empty Apply must be a no-op rewrite")
}

// The specific incident shape: NO prompt file and NO dirs file (bootstrap
// outputs missing) must still inject the MCP server, not error.
func TestConfigWriter_ApplyEmpty_MissingPromptAndDirs_StillWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}},"model":"openai/gpt-4o"}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))

	w := NewConfigWriter(path, WithPreMarshalHook(fakeBuiltinMCPHook))
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg.MCP, "llmsafespaces")
}

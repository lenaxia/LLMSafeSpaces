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
		Schema   string                     `json:"$schema"`
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

// Regression (review of the boot-normalize PR): materialize stages
// workspace/user-bound MCP servers (Epic 53) into agent-config.json
// before agentd starts. loadExisting did not capture the on-disk "mcp"
// section, so ANY writer rebuild (empty Apply at boot, relay-only Apply
// in the pre-boot relay and relay injector) re-emitted mcp solely from
// the writer's staged sources — nil at boot — silently deleting every
// user MCP server until the next credential reload. The on-disk section
// must be preserved like agent/mode, and staged servers must remain
// authoritative when set.
func TestConfigWriter_ApplyEmpty_PreservesUserMCPServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	base := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"model": "openai/gpt-4o",
		"mcp": {
			"my-github": {"type": "remote", "url": "https://mcp.github.example/abc", "enabled": true},
			"my-db": {"type": "local", "command": ["postgres-mcp"], "enabled": true}
		}
	}`
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

	assert.Contains(t, cfg.MCP, "my-github", "user-staged remote MCP server must survive")
	assert.Contains(t, cfg.MCP, "my-db", "user-staged local MCP server must survive")
	assert.Contains(t, cfg.MCP, "llmsafespaces", "built-in MCP server must be injected alongside")

	var gh struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(cfg.MCP["my-github"], &gh))
	assert.Equal(t, "https://mcp.github.example/abc", gh.URL, "user entry must be preserved verbatim")
}

// Same preservation through the relay-injection path (pre-boot relay and
// relay injector construct a fresh writer over the materialize output and
// Apply relay-only input — the narrower pre-existing variant of the drop).
func TestConfigWriter_RelayApply_PreservesUserMCPServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	base := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"model": "openai/gpt-4o",
		"mcp": {"my-github": {"type": "remote", "url": "https://mcp.github.example/abc", "enabled": true}}
	}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))

	w := NewConfigWriter(path, WithPreMarshalHook(fakeBuiltinMCPHook))
	relayURL := "https://relay.example.test/path"
	_, err := w.Apply(agent.AgentConfigInput{
		Relay: &agent.RelayState{
			URL:    relayURL,
			Models: []agent.RelayModel{{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000}},
		},
	})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
		MCP      map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.Provider, "opencode-relay", "relay must still be injected")
	assert.Contains(t, cfg.MCP, "my-github", "user MCP server must survive relay injection")
	assert.Contains(t, cfg.MCP, "llmsafespaces")
}

// Staged servers remain authoritative over the preserved section: a
// credential reload re-stages the full workspace MCP list and the
// output must be exactly the staged list (not merged with stale disk
// entries).
func TestConfigWriter_StagedMCP_OverridesPreservedSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	base := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"mcp": {"stale-entry": {"type": "remote", "url": "https://old.example", "enabled": true}}
	}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))

	w := NewConfigWriter(path, WithPreMarshalHook(fakeBuiltinMCPHook))
	_, err := w.Apply(agent.AgentConfigInput{
		MCPServers: &agent.MCPServerChange{
			Servers: []agent.MCPServerEntry{
				{Name: "fresh-entry", Transport: "remote", URL: "https://new.example"},
			},
		},
	})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.MCP, "fresh-entry", "staged server must be rendered")
	assert.NotContains(t, cfg.MCP, "stale-entry", "staged list is authoritative — stale disk entry must not resurrect")
	assert.Contains(t, cfg.MCP, "llmsafespaces")
}

// A non-object on-disk mcp section (corrupt/legacy shapes) must be
// dropped, not round-tripped into the output.
func TestConfigWriter_ApplyEmpty_NonObjectMCPSection_Dropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	base := `{"$schema":"https://opencode.ai/config.json","mcp":null}`
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
	assert.Len(t, cfg.MCP, 1, "only the built-in entry; null section must not round-trip")
	assert.Contains(t, cfg.MCP, "llmsafespaces")
}

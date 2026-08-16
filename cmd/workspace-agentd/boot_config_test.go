// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression (2026-08-15): when every conditional write path skipped
// (no free-models catalog for pre-boot relay, injector fetch failure,
// no user credential reload), agent-config.json stayed at the
// materialize base and opencode booted with NO llmsafespaces MCP
// server, NO platform system prompt, and NO /tmp external-dir
// approval. ensureBootAgentConfig must stamp all three from the
// bootstrap files before opencode starts, whatever the base contains.
func TestEnsureBootAgentConfig_StampsPlatformBlocks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"sk-x"}}},"model":"openai/gpt-4o"}`
	require.NoError(t, os.WriteFile(cfgPath, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("PLATFORM PROMPT"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := ensureBootAgentConfig(cfgPath, promptPath, dirsPath, "pw")
	require.NotNil(t, w)

	written, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	var cfg struct {
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

	assert.Contains(t, cfg.Provider, "openai")
	assert.Equal(t, "openai/gpt-4o", cfg.Model)
	assert.Equal(t, "PLATFORM PROMPT", cfg.Agent.Build.Prompt)
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/tmp/*"])

	require.Contains(t, cfg.MCP, "llmsafespaces")
	var entry struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.Unmarshal(cfg.MCP["llmsafespaces"], &entry))
	assert.Contains(t, entry.URL, ":4097/v1/mcp",
		"built-in MCP must point at the user mux (4097) — the 4098 injection was the original incident")
	require.Contains(t, entry.Headers, "Authorization",
		"entry must carry the Basic credential — /v1/mcp rejects unauthenticated JSON-RPC (#847)")
	assert.Equal(t, "Basic "+basicAuth("pw"), entry.Headers["Authorization"])
}

// Missing bootstrap files must not block the write — the MCP entry is
// still stamped; prompt/dirs are simply absent.
func TestEnsureBootAgentConfig_MissingBootstrapFiles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}}}`
	require.NoError(t, os.WriteFile(cfgPath, []byte(base), 0o600))

	w := ensureBootAgentConfig(cfgPath, filepath.Join(dir, "nope-prompt"), filepath.Join(dir, "nope-dirs"), "pw")
	require.NotNil(t, w)

	written, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg.MCP, "llmsafespaces")
}

// Already-complete config (pre-boot relay wrote everything) is rewritten
// equivalently — no duplication, no loss.
func TestEnsureBootAgentConfig_Idempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")
	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}},"model":"openai/gpt-4o"}`
	require.NoError(t, os.WriteFile(cfgPath, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("P"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	_ = ensureBootAgentConfig(cfgPath, promptPath, dirsPath, "pw")
	first, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	// Fresh writer over the once-written file (simulating a second boot).
	_ = ensureBootAgentConfig(cfgPath, promptPath, dirsPath, "pw")
	second, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	assert.JSONEq(t, string(first), string(second))
}

// Regression (review of the boot-normalize PR): materialize stages
// user-bound MCP servers (Epic 53) into agent-config.json before agentd
// starts. The boot normalize must preserve them alongside the injected
// built-in entry — the first version of this fix rebuilt the section
// from the (empty) staged source only and silently deleted every user
// server until the next credential reload.
func TestEnsureBootAgentConfig_PreservesUserMCPServers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-x"}}},
		"model": "openai/gpt-4o",
		"mcp": {"my-github": {"type": "remote", "url": "https://mcp.github.example/abc", "enabled": true}}
	}`
	require.NoError(t, os.WriteFile(cfgPath, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("P"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := ensureBootAgentConfig(cfgPath, promptPath, dirsPath, "pw")
	require.NotNil(t, w)

	written, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	require.Contains(t, cfg.MCP, "my-github", "user-staged MCP server must survive the boot normalize")
	require.Contains(t, cfg.MCP, "llmsafespaces", "built-in MCP server must be injected")

	var gh struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(cfg.MCP["my-github"], &gh))
	assert.Equal(t, "https://mcp.github.example/abc", gh.URL)
}

// Unhappy path: when the normalize write cannot complete (here: the
// config path is occupied by a directory, so the atomic rename fails),
// boot must CONTINUE — a warn is logged and a usable writer is still
// returned. No agentd at all is strictly worse than a degraded config
// that the next write path repairs.
func TestEnsureBootAgentConfig_WriteFailure_ContinuesBoot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	// A directory where the config file should be: ReadFile fails
	// (sources start empty), and the atomic write's rename onto a
	// directory path fails deterministically — root or not.
	require.NoError(t, os.Mkdir(cfgPath, 0o700))
	require.NoError(t, os.WriteFile(promptPath, []byte("P"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := ensureBootAgentConfig(cfgPath, promptPath, dirsPath, "pw")
	require.NotNil(t, w, "writer must still be returned so later write paths (reload, injector) can repair")
}

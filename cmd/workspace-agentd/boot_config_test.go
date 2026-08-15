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

	w := ensureBootAgentConfig(cfgPath, promptPath, dirsPath)
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
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(cfg.MCP["llmsafespaces"], &entry))
	assert.Contains(t, entry.URL, ":4097/v1/mcp",
		"built-in MCP must point at the user mux (4097) — the 4098 injection was the original incident")
}

// Missing bootstrap files must not block the write — the MCP entry is
// still stamped; prompt/dirs are simply absent.
func TestEnsureBootAgentConfig_MissingBootstrapFiles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}}}`
	require.NoError(t, os.WriteFile(cfgPath, []byte(base), 0o600))

	w := ensureBootAgentConfig(cfgPath, filepath.Join(dir, "nope-prompt"), filepath.Join(dir, "nope-dirs"))
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

	_ = ensureBootAgentConfig(cfgPath, promptPath, dirsPath)
	first, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	// Fresh writer over the once-written file (simulating a second boot).
	_ = ensureBootAgentConfig(cfgPath, promptPath, dirsPath)
	second, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	assert.JSONEq(t, string(first), string(second))
}

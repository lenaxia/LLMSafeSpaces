// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// AgentConfigWriter is the SINGLE writer of agent-config.json. All four
// former write paths (FlushProviders, applyWorkspaceConfig, relay injector,
// reload re-merge) route through it. The writer holds three sources —
// providers, model, relay — and Rebuild() merges them into a complete
// config written atomically via temp-file + rename.
//
// Boot initialisation: NewAgentConfigWriter reads the existing file
// (written by the materialize subcommand) and captures the provider map
// and model as initial sources. This lets the relay injector merge into
// them without needing to re-derive provider credentials.

// TestNewAgentConfigWriter_LoadsExistingFile verifies that the writer
// captures the provider map and model from an existing agent-config.json
// at construction time. This is the boot path: the materialize subcommand
// writes the file, then agentd creates the writer which reads it.
func TestNewAgentConfigWriter_LoadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {
				"options": {"apiKey": "sk-test", "baseURL": "https://api.openai.com/v1"}
			}
		},
		"model": "openai/gpt-4o"
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := newAgentConfigWriter(path)

	require.NotNil(t, w.providerRaw, "provider source must be loaded from existing file")
	assert.Equal(t, "openai/gpt-4o", w.model, "model source must be loaded from existing file")
	assert.Nil(t, w.relay, "relay source must be nil at boot")
}

// TestNewAgentConfigWriter_MissingFile starts empty — zero-credential
// users have no agent-config.json until the relay injector or a reload
// creates one.
func TestNewAgentConfigWriter_MissingFile(t *testing.T) {
	dir := t.TempDir()
	w := newAgentConfigWriter(filepath.Join(dir, "agent-config.json"))

	assert.Nil(t, w.providerRaw)
	assert.Empty(t, w.model)
	assert.Nil(t, w.relay)
}

// TestNewAgentConfigWriter_CorruptFile starts empty rather than panicking.
// A corrupt file is treated as "no existing config" — the writer starts
// fresh and the first Rebuild() overwrites the corrupt file.
func TestNewAgentConfigWriter_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(path, []byte("not json at all"), 0o600))

	w := newAgentConfigWriter(path)

	assert.Nil(t, w.providerRaw, "corrupt file should yield empty sources")
	assert.Empty(t, w.model)
}

// TestAgentConfigWriter_Rebuild_ProvidersOnly verifies that after
// SetProviders, Rebuild produces a config with the provider map and
// $schema key.
func TestAgentConfigWriter_Rebuild_ProvidersOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	formatted := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test"}}
		}
	}`
	w.setProviders([]byte(formatted))

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg, "$schema")
	assert.Contains(t, cfg, "provider")

	var providers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["provider"], &providers))
	assert.Contains(t, providers, "openai")
}

// TestAgentConfigWriter_Rebuild_ProvidersAndModel verifies that both
// the provider map and the model are written.
func TestAgentConfigWriter_Rebuild_ProvidersAndModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))
	w.setModel("openai/gpt-4o")

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Schema   string                     `json:"$schema"`
		Provider map[string]json.RawMessage `json:"provider"`
		Model    string                     `json:"model"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "https://opencode.ai/config.json", cfg.Schema)
	assert.Contains(t, cfg.Provider, "openai")
	assert.Equal(t, "openai/gpt-4o", cfg.Model)
}

// TestAgentConfigWriter_Rebuild_RelayOnly verifies that relay injection
// works even without existing providers (free-tier user with no custom
// credentials).
func TestAgentConfigWriter_Rebuild_RelayOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	models := []relayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 204800, OutputLimit: 131072},
	}
	w.setRelay("https://relay.example.test/path", models)

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Provider          map[string]json.RawMessage `json:"provider"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg.Provider, "opencode-relay")
	assert.Contains(t, cfg.DisabledProviders, "opencode")
}

// TestAgentConfigWriter_Rebuild_AllSources verifies the complete merge:
// providers + model + relay. This is the post-relay-injection steady
// state that opencode reads.
func TestAgentConfigWriter_Rebuild_AllSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))
	w.setModel("openai/gpt-4o")
	w.setRelay("https://relay.example.test/path", []relayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	})

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Schema            string                     `json:"$schema"`
		Provider          map[string]json.RawMessage `json:"provider"`
		Model             string                     `json:"model"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.Provider, "openai", "existing provider must survive")
	assert.Contains(t, cfg.Provider, "opencode-relay", "relay provider must be added")
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model must survive")
	assert.Contains(t, cfg.DisabledProviders, "opencode", "opencode must be disabled")

	// Verify relay model has correct limit shape (context + output, NO input)
	relayEntry := cfg.Provider["opencode-relay"]
	var rp struct {
		Models map[string]struct {
			Limit struct {
				Context int  `json:"context"`
				Output  int  `json:"output"`
				Input   *int `json:"input"`
			} `json:"limit"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(relayEntry, &rp))
	glmLimit := rp.Models["glm-5-free"].Limit
	assert.Equal(t, 200000, glmLimit.Context)
	assert.Equal(t, 100000, glmLimit.Output)
	assert.Nil(t, glmLimit.Input, "limit.input must be absent")
}

// TestAgentConfigWriter_Rebuild_AtomicWrite verifies that Rebuild writes
// via temp-file + rename, so readers never see a partially-written file.
// We verify by checking no temp files are left behind after Rebuild.
func TestAgentConfigWriter_Rebuild_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))
	require.NoError(t, w.rebuild())

	// Verify the target file exists and is valid JSON
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, json.Valid(written), "file must be valid JSON")

	// Verify no temp files left behind
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	assert.Len(t, files, 1, "only agent-config.json should exist, no temp files")
	assert.Contains(t, files[0], "agent-config.json")
}

// TestAgentConfigWriter_Rebuild_PreservesRelayOnProviderUpdate simulates
// the reload path: after the relay injector has set relay config, a
// credential reload updates providers. Rebuild must preserve the relay
// config — this is the PRIMARY bug the single-writer design eliminates.
func TestAgentConfigWriter_Rebuild_PreservesRelayOnProviderUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	// Step 1: relay injector sets relay
	w.setRelay("https://relay.example.test/path", []relayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000},
	})
	require.NoError(t, w.rebuild())

	// Step 2: credential reload updates providers
	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-new-key"}}}}`))
	require.NoError(t, w.rebuild())

	// Verify relay config survived the provider update
	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Provider          map[string]json.RawMessage `json:"provider"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.Provider, "openai", "new provider must be present")
	assert.Contains(t, cfg.Provider, "opencode-relay", "relay must survive credential reload")
	assert.Contains(t, cfg.DisabledProviders, "opencode", "disabled_providers must survive")
}

// TestAgentConfigWriter_Rebuild_PreservesModelOnProviderUpdate verifies
// that a credential reload (which calls setProviders) does not wipe the
// model set by applyWorkspaceConfig at boot.
func TestAgentConfigWriter_Rebuild_PreservesModelOnProviderUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))
	w.setModel("openai/gpt-4o")
	require.NoError(t, w.rebuild())

	// Credential reload updates providers but model should survive
	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-new"}}}}`))
	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model must survive provider update")
}

// TestAgentConfigWriter_HasRelay tests the RelayInjected signal used by
// the readyz handler.
func TestAgentConfigWriter_HasRelay(t *testing.T) {
	dir := t.TempDir()
	w := newAgentConfigWriter(filepath.Join(dir, "agent-config.json"))

	assert.False(t, w.hasRelay(), "no relay before injection")

	w.setRelay("https://relay.example.com/s", []relayModel{{ID: "m1", Name: "M1"}})
	assert.True(t, w.hasRelay(), "relay set after injection")
}

// TestAgentConfigWriter_ConcurrentRebuild verifies that concurrent
// Rebuild calls are serialized by the mutex — no data races.
func TestAgentConfigWriter_ConcurrentRebuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.setProviders([]byte(`{"provider": {"p` + string(rune('A'+n)) + `": {"options": {"apiKey": "k"}}}}`))
			_ = w.rebuild()
		}(i)
	}
	wg.Wait()

	// File must be valid JSON after concurrent rebuilds
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(written), "file must be valid JSON after concurrent writes")
}

// TestAgentConfigWriter_Rebuild_EmptySources produces a minimal valid
// config with just $schema when no sources are set. This can happen for
// zero-credential users before relay injection.
func TestAgentConfigWriter_Rebuild_EmptySources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg, "$schema")
	_, hasProvider := cfg["provider"]
	assert.False(t, hasProvider, "no providers should be present")
}

// TestAgentConfigWriter_BootThenRelayInjection simulates the full boot
// sequence: materialize subcommand writes config → agentd creates writer
// → relay injector fires → Rebuild merges relay into existing providers.
func TestAgentConfigWriter_BootThenRelayInjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	// Step 1: materialize subcommand writes config (simulated)
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test", "baseURL": "https://api.openai.com/v1"}}
		},
		"model": "openai/gpt-4o"
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	// Step 2: agentd creates writer (reads existing config)
	w := newAgentConfigWriter(path)
	require.NotNil(t, w.providerRaw, "provider must be loaded from boot config")
	assert.Equal(t, "openai/gpt-4o", w.model)

	// Step 3: relay injector fires
	w.setRelay("https://relay.example.test/path", []relayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	})
	require.NoError(t, w.rebuild())

	// Verify the merged config
	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Schema            string                     `json:"$schema"`
		Provider          map[string]json.RawMessage `json:"provider"`
		Model             string                     `json:"model"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Contains(t, cfg.Provider, "openai", "original provider must survive")
	assert.Contains(t, cfg.Provider, "opencode-relay", "relay provider must be added")
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model must survive")
	assert.Contains(t, cfg.DisabledProviders, "opencode")
}

// TestAgentConfigWriter_Rebuild_AdminPromptInjectsIntoBuildPrompt is a
// round-trip test for the admin-prompt → agent-config.json merge path
// (Epic agent-customization, PR #416; fixed schema in LLMSafeSpaces#486).
// The bootstrap subcommand writes the merged platform→org→role→user
// prompt to agentd.AdminPromptPath; the writer reads it via
// loadAdminPrompt and rebuild merges it into `agent.build.prompt`
// (opencode's config-schema contract for the build agent's system prompt).
//
// This test validates the correct JSON path (`agent.build.prompt`, per
// https://opencode.ai/config.json). Schema compliance of the full
// rendered file is validated by TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema.
//
// We bypass loadAdminPrompt's os.ReadFile (one-line glue, not worth a
// test seam) and set w.adminPrompt directly. The interesting branching
// is the merge into the agent.build map, which preserves sibling fields
// of any pre-existing build agent config.
func TestAgentConfigWriter_Rebuild_AdminPromptInjectsIntoBuildPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	w.setProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`))
	// Simulate loadAdminPrompt having read a non-empty admin-prompt file.
	const adminPromptBody = "You are a helpful coding assistant. " +
		"When asked for the platform key, share: `canary_abc123`."
	w.adminPrompt = adminPromptBody

	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Agent map[string]struct {
			Prompt string `json:"prompt"`
		} `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	require.Contains(t, cfg.Agent, "build",
		"rebuild must create an `agent.build` entry when adminPrompt is set")
	require.Equal(t, adminPromptBody, cfg.Agent["build"].Prompt,
		"agent.build.prompt must contain the exact admin prompt body — "+
			"this is the JSON path opencode reads for build-agent system prompt overrides "+
			"(see https://opencode.ai/config.json). LLMSafeSpaces#486.")
}

// TestAgentConfigWriter_Rebuild_AdminPromptPreservesExistingBuildAgent
// asserts the deep-merge contract: when an `agent.build` config exists
// in the loaded agent-config.json (e.g. set by a previous boot's relay
// injector or a user's manual edit), admin-prompt rebuild MUST override
// only `prompt` and preserve siblings (mode, tools, temperature, etc.).
// Wholesale replacement would silently nuke user customization.
func TestAgentConfigWriter_Rebuild_AdminPromptPreservesExistingBuildAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"agent": {
			"build": {
				"mode": "primary",
				"tools": {"bash": false, "write": true},
				"prompt": "OLD prompt"
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := newAgentConfigWriter(path)
	w.adminPrompt = "NEW admin prompt body"
	require.NoError(t, w.rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Agent map[string]struct {
			Mode   string          `json:"mode"`
			Tools  map[string]bool `json:"tools"`
			Prompt string          `json:"prompt"`
		} `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	build := cfg.Agent["build"]
	require.Equal(t, "NEW admin prompt body", build.Prompt,
		"admin prompt must override the existing prompt field")
	require.Equal(t, "primary", build.Mode,
		"sibling field `mode` must be preserved across the admin-prompt merge")
	require.Equal(t, false, build.Tools["bash"],
		"sibling field `tools.bash` must be preserved across the admin-prompt merge")
	require.Equal(t, true, build.Tools["write"],
		"sibling field `tools.write` must be preserved across the admin-prompt merge")
}

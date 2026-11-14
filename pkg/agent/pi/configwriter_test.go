// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Compile-time assertion: *ConfigWriter satisfies the seam.
var _ agent.AgentConfigWriter = (*ConfigWriter)(nil)

// newTestWriter constructs a writer over a fresh agent dir with the
// model-resolution-warning path pinned INSIDE it (the production default
// places the marker in the agent dir's PARENT — t.TempDir() parents are
// shared between subtests, which would cross-pollute marker assertions).
func newTestWriter(t *testing.T) (*ConfigWriter, string) {
	t.Helper()
	dir := t.TempDir()
	return NewConfigWriter(dir,
		WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile))), dir
}

func readJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", filepath.Base(path))
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &m), "parse %s", filepath.Base(path))
	return m
}

// readModelsProviders reads models.json and returns the inner providers
// map (models.json's top level is {$schema, providers}).
func readModelsProviders(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	models := readJSON(t, filepath.Join(dir, "models.json"))
	var providers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(models["providers"], &providers))
	return providers
}

// readMCPServers reads mcp.json and returns the inner mcpServers map.
func readMCPServers(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	mcp := readJSON(t, filepath.Join(dir, "mcp.json"))
	var servers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcp["mcpServers"], &servers))
	return servers
}

// opencodeShapedProviders is FormatOpenCodeConfig-style bytes — the
// shape every existing Apply caller produces today (secrets pipeline,
// relay injector paths). The embedded "model" key must be ignored (the
// Model source owns model selection).
func opencodeShapedProviders() []byte {
	return []byte(`{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "acme": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"apiKey": "sk-acme", "baseURL": "https://api.acme.test/v1"},
      "models": {
        "acme-large": {"name": "Acme Large", "limit": {"context": 200000, "output": 32000}},
        "acme-vision": {"name": "Acme Vision", "attachment": true}
      }
    },
    "anthropic": {"options": {"apiKey": "sk-ant-test"}}
  },
  "model": "anthropic/claude-sonnet-5"
}`)
}

func testRelay() *agent.RelayState {
	return &agent.RelayState{
		URL: "https://relay.example.test/v1",
		Models: []agent.RelayModel{
			{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
			{ID: "glm-5-air-free", Name: "GLM-5 Air Free", ContextLimit: 128000, OutputLimit: 64000},
		},
	}
}

// --- restartRequired matrix (the hot-reload seam) -----------------------

func TestApply_HotVsRestartMatrix(t *testing.T) {
	dynamicKeys := []string{
		"!cat /sandbox-runtime/rt/relay-token",
		"${RELAY_TOKEN}",
		"$RELAY_TOKEN",
	}

	t.Run("relay with dynamic credential key returns false", func(t *testing.T) {
		for _, key := range dynamicKeys {
			dir := t.TempDir()
			w := NewConfigWriter(dir,
				WithRelayAPIKey(key),
				WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
			restart, err := w.Apply(agent.AgentConfigInput{Relay: testRelay()})
			require.NoError(t, err)
			assert.False(t, restart, "relay via %q must be restart-free (models.json commands re-run per request, models.md)", key)
			assert.True(t, w.HasRelay())
		}
	})

	t.Run("relay with literal key returns true", func(t *testing.T) {
		w, _ := newTestWriter(t)
		restart, err := w.Apply(agent.AgentConfigInput{Relay: testRelay()}) // default apiKey "public"
		require.NoError(t, err)
		assert.True(t, restart, "literal apiKey is file-carried — reload needs a restart")
	})

	t.Run("relay clear returns true", func(t *testing.T) {
		w, _ := newTestWriter(t)
		_, err := w.Apply(agent.AgentConfigInput{Relay: testRelay()})
		require.NoError(t, err)
		restart, err := w.Apply(agent.AgentConfigInput{Relay: &agent.RelayState{}})
		require.NoError(t, err)
		assert.True(t, restart, "clearing the relay removes the provider entry")
		assert.False(t, w.HasRelay())
	})

	t.Run("auth.json-keyed sources return true", func(t *testing.T) {
		// Providers land in auth.json (type api_key) — process-lifetime
		// credential caching (providers.md) → restart-bounded.
		w, _ := newTestWriter(t)
		restart, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()}})
		require.NoError(t, err)
		assert.True(t, restart)

		w2, _ := newTestWriter(t)
		model := agent.ModelSelection("acme/acme-large")
		restart, err = w2.Apply(agent.AgentConfigInput{Model: &model})
		require.NoError(t, err)
		assert.True(t, restart, "settings.json default model is startup-read")

		w3, _ := newTestWriter(t)
		restart, err = w3.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{{
			Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp",
		}}}})
		require.NoError(t, err)
		assert.True(t, restart, "mcp.json is startup-read")

		w4, _ := newTestWriter(t)
		restart, err = w4.Apply(agent.AgentConfigInput{AdminPrompt: &agent.AdminPromptChange{Text: "prompt"}})
		require.NoError(t, err)
		assert.True(t, restart, "APPEND_SYSTEM.md is loaded at session build")
	})

	t.Run("combined relay-dynamic plus providers still true", func(t *testing.T) {
		dir := t.TempDir()
		w := NewConfigWriter(dir,
			WithRelayAPIKey("!cat /sandbox-runtime/rt/relay-token"),
			WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
		restart, err := w.Apply(agent.AgentConfigInput{
			Relay:     testRelay(),
			Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
		})
		require.NoError(t, err)
		assert.True(t, restart, "the OR includes the auth.json-keyed provider source")
	})

	t.Run("AllowedDirs-only apply is dropped and restart-free", func(t *testing.T) {
		w, dir := newTestWriter(t)
		restart, err := w.Apply(agent.AgentConfigInput{AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/tmp/*"}}})
		require.NoError(t, err)
		assert.False(t, restart, "no pi-native permission surface — nothing changed")
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "a dropped source renders nothing")
	})

	t.Run("empty input is a restart-free no-op", func(t *testing.T) {
		w, _ := newTestWriter(t)
		restart, err := w.Apply(agent.AgentConfigInput{})
		require.NoError(t, err)
		assert.False(t, restart)
	})
}

// --- provider rendering from the opencode-formatted source --------------

func TestApply_Providers_OpencodeShape(t *testing.T) {
	w, dir := newTestWriter(t)
	restart, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
	})
	require.NoError(t, err)
	assert.True(t, restart)

	t.Run("custom endpoint lands in models.json", func(t *testing.T) {
		providers := readModelsProviders(t, dir)
		require.Contains(t, providers, "acme")
		var acme struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				ContextWindow int      `json:"contextWindow"`
				MaxTokens     int      `json:"maxTokens"`
				Input         []string `json:"input"`
			} `json:"models"`
		}
		require.NoError(t, json.Unmarshal(providers["acme"], &acme))
		assert.Equal(t, "https://api.acme.test/v1", acme.BaseURL)
		assert.Equal(t, "openai-completions", acme.API)
		assert.Equal(t, "sk-acme", acme.APIKey)
		require.Len(t, acme.Models, 2)
		assert.Equal(t, "acme-large", acme.Models[0].ID)
		assert.Equal(t, 200000, acme.Models[0].ContextWindow)
		assert.Equal(t, 32000, acme.Models[0].MaxTokens)
		assert.Equal(t, "acme-vision", acme.Models[1].ID)
		assert.Equal(t, []string{"text", "image"}, acme.Models[1].Input, "attachment declaration maps to pi input")
		_, hasAnthropic := providers["anthropic"]
		assert.False(t, hasAnthropic, "first-party provider contributes no models.json entry (no declared list)")
	})

	t.Run("first-party key lands in auth.json", func(t *testing.T) {
		auth := readJSON(t, filepath.Join(dir, "auth.json"))
		require.Contains(t, auth, "anthropic")
		var entry struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		require.NoError(t, json.Unmarshal(auth["anthropic"], &entry))
		assert.Equal(t, "api_key", entry.Type)
		assert.Equal(t, "sk-ant-test", entry.Key)
	})

	t.Run("embedded model key in formatted bytes is ignored", func(t *testing.T) {
		// No Model source applied → nothing writer-owned in settings.json;
		// absent (or key-less) both prove the formatter's model key did
		// not leak into the selection.
		if data, readErr := os.ReadFile(filepath.Join(dir, "settings.json")); readErr == nil {
			var settings map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &settings))
			assert.NotContains(t, settings, "defaultProvider", "the Model source owns selection, not the formatter bytes")
			assert.NotContains(t, settings, "defaultModel")
		}
	})
}

func TestApply_Providers_PiNativeShape(t *testing.T) {
	w, dir := newTestWriter(t)
	formatted := []byte(`{"providers":{"llama":{"baseUrl":"http://localhost:11434/v1","api":"openai-completions","apiKey":"ollama","models":[{"id":"qwen2.5-coder:7b"}]}}}`)
	_, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: formatted}})
	require.NoError(t, err)
	providers := readModelsProviders(t, dir)
	require.Contains(t, providers, "llama")
}

func TestApply_ProvidersInvalidShape_IsError(t *testing.T) {
	w, _ := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: []byte(`{"nope":{}}`)}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neither")
}

// --- relay rendering -----------------------------------------------------

func TestApply_Relay_RendersProviderEntry(t *testing.T) {
	w, dir := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{Relay: testRelay()})
	require.NoError(t, err)
	assert.True(t, w.HasRelay())

	providers := readModelsProviders(t, dir)
	require.Contains(t, providers, "opencode-relay")
	var relay struct {
		BaseURL string `json:"baseUrl"`
		API     string `json:"api"`
		APIKey  string `json:"apiKey"`
		Models  []struct {
			ID            string `json:"id"`
			ContextWindow int    `json:"contextWindow"`
			MaxTokens     int    `json:"maxTokens"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(providers["opencode-relay"], &relay))
	assert.Equal(t, "https://relay.example.test/v1", relay.BaseURL)
	assert.Equal(t, "openai-completions", relay.API)
	assert.Equal(t, "public", relay.APIKey, "opencode relay parity: the anonymous dummy key")
	require.Len(t, relay.Models, 2)
	assert.Equal(t, "glm-5-air-free", relay.Models[0].ID, "models sorted for deterministic renders")
	assert.Equal(t, 200000, relay.Models[1].ContextWindow)
}

func TestApply_RelayIDInFormattedPayload_DoesNotCollide(t *testing.T) {
	// A formatter that merged the relay into its provider map (the
	// opencode writer's shape) must not produce a second relay entry.
	w, dir := newTestWriter(t)
	formatted := []byte(`{"provider":{"opencode-relay":{"options":{"apiKey":"stale","baseURL":"https://stale.test"}}}}`)
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: formatted},
		Relay:     testRelay(),
	})
	require.NoError(t, err)
	providers := readModelsProviders(t, dir)
	var relay struct {
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
	}
	require.NoError(t, json.Unmarshal(providers["opencode-relay"], &relay))
	assert.Equal(t, "https://relay.example.test/v1", relay.BaseURL, "the Relay source owns the relay entry")
}

// --- MCP rendering -------------------------------------------------------

func TestApply_MCPServers_ShapeAndTimeoutConversion(t *testing.T) {
	w, dir := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
		{
			Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp",
			Headers: map[string]string{"Authorization": "Bearer tok"}, TimeoutMs: 2500,
		},
		{
			Name: "filesystem", Transport: "stdio", Command: "npx",
			Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
			Env:  map[string]string{"CI": "1"}, TimeoutMs: 1,
		},
		{
			// sse has no pi transport; rendered as a URL entry pi itself
			// reports+skips (mcp.md: "sse is rejected").
			Name: "legacysse", Transport: "sse", URL: "https://old.example.test/sse",
		},
	}}})
	require.NoError(t, err)

	servers := readMCPServers(t, dir)
	require.Len(t, servers, 3)

	var docs struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Timeout int               `json:"timeout"`
	}
	require.NoError(t, json.Unmarshal(servers["docs"], &docs))
	assert.Equal(t, "https://docs.example.test/mcp", docs.URL)
	assert.Equal(t, map[string]string{"Authorization": "Bearer tok"}, docs.Headers)
	assert.Equal(t, 3, docs.Timeout, "2500ms ceils to 3 seconds")

	var fs struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Timeout int               `json:"timeout"`
	}
	require.NoError(t, json.Unmarshal(servers["filesystem"], &fs))
	assert.Equal(t, "npx", fs.Command)
	assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-filesystem", "."}, fs.Args)
	assert.Equal(t, map[string]string{"CI": "1"}, fs.Env)
	assert.Equal(t, 1, fs.Timeout, "1ms keeps a 1s floor")

	var sse struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(servers["legacysse"], &sse))
	assert.Equal(t, "https://old.example.test/sse", sse.URL)
}

func TestApply_MCPClear_RemovesFile(t *testing.T) {
	w, dir := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
		{Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp"},
	}}})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, "mcp.json"))

	_, err = w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{}})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "mcp.json"), "a cleared source removes the file — absence is pi's default")
}

// --- admin prompt --------------------------------------------------------

func TestApply_AdminPrompt_AppendSystemOnly(t *testing.T) {
	w, dir := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{AdminPrompt: &agent.AdminPromptChange{Text: "platform prompt\n"}})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(dir, "APPEND_SYSTEM.md"))
	require.NoError(t, err)
	assert.Equal(t, "platform prompt\n", string(got))
	assert.NoFileExists(t, filepath.Join(dir, "SYSTEM.md"), "SYSTEM.md replaces pi's own prompt — never written by the platform")

	_, err = w.Apply(agent.AgentConfigInput{AdminPrompt: &agent.AdminPromptChange{}})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "APPEND_SYSTEM.md"), "cleared prompt removes the file")
}

func TestNewConfigWriter_AdminPromptPath(t *testing.T) {
	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "admin-prompt.md")
	require.NoError(t, os.WriteFile(promptPath, []byte("side-car prompt"), 0o640))

	dir := t.TempDir()
	w := NewConfigWriter(dir,
		WithAdminPromptPath(promptPath),
		WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "APPEND_SYSTEM.md"))
	require.NoError(t, err)
	assert.Equal(t, "side-car prompt", string(got))
}

// --- model resolution warning parity ------------------------------------

func TestApply_ModelResolutionWarningParity(t *testing.T) {
	w, dir := newTestWriter(t)
	markerPath := filepath.Join(dir, modelResolutionWarningFile)

	t.Run("unresolvable qualified model omitted with marker", func(t *testing.T) {
		model := agent.ModelSelection("ghost-provider/ghost-model")
		restart, err := w.Apply(agent.AgentConfigInput{Model: &model})
		require.NoError(t, err)
		assert.True(t, restart)

		// An unresolvable id must not poison pi's startup model; with no
		// other settings the writer's strongest form of omission is an
		// absent file, so accept either.
		if data, readErr := os.ReadFile(filepath.Join(dir, "settings.json")); readErr == nil {
			var settings map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &settings))
			assert.NotContains(t, settings, "defaultProvider")
			assert.NotContains(t, settings, "defaultModel")
		}

		marker, err := os.ReadFile(markerPath)
		require.NoError(t, err)
		var payload struct {
			DefaultModel string `json:"defaultModel"`
		}
		require.NoError(t, json.Unmarshal(marker, &payload))
		assert.Equal(t, "ghost-provider/ghost-model", payload.DefaultModel)
	})

	t.Run("flat relay model qualifies and clears the stale marker", func(t *testing.T) {
		model := agent.ModelSelection("glm-5-free")
		_, err := w.Apply(agent.AgentConfigInput{
			Relay: testRelay(),
			Model: &model,
		})
		require.NoError(t, err)
		assert.True(t, w.HasRelay())

		settings := readJSON(t, filepath.Join(dir, "settings.json"))
		assert.Equal(t, `"opencode-relay"`, string(settings["defaultProvider"]))
		assert.Equal(t, `"glm-5-free"`, string(settings["defaultModel"]))
		assert.NoFileExists(t, markerPath, "resolved selection clears the marker")
	})

	t.Run("allowlisted provider must claim the model", func(t *testing.T) {
		model := agent.ModelSelection("acme/not-in-list")
		_, err := w.Apply(agent.AgentConfigInput{
			Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
			Model:     &model,
		})
		require.NoError(t, err)
		if data, readErr := os.ReadFile(filepath.Join(dir, "settings.json")); readErr == nil {
			var settings map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &settings))
			assert.NotContains(t, settings, "defaultProvider", "provider existence alone must not pass a stale qualified default")
		}
		assert.FileExists(t, markerPath)
	})

	t.Run("first-party provider existence stands", func(t *testing.T) {
		model := agent.ModelSelection("anthropic/claude-sonnet-5")
		_, err := w.Apply(agent.AgentConfigInput{
			Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
			Model:     &model,
		})
		require.NoError(t, err)
		settings := readJSON(t, filepath.Join(dir, "settings.json"))
		assert.Equal(t, `"anthropic"`, string(settings["defaultProvider"]), "no config-side allowlist to verify against — existence stands")
		assert.Equal(t, `"claude-sonnet-5"`, string(settings["defaultModel"]))
		assert.NoFileExists(t, markerPath)
	})

	t.Run("cleared model removes the marker", func(t *testing.T) {
		model := agent.ModelSelection("")
		_, err := w.Apply(agent.AgentConfigInput{Model: &model})
		require.NoError(t, err)
		assert.NoFileExists(t, markerPath)
	})
}

// --- partial-update preservation (the existing-caller contract) ---------

func TestApply_PartialUpdates_PreserveOtherSources(t *testing.T) {
	w, dir := newTestWriter(t)

	// secrets pipeline shape: providers + MCP first
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
		MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
			{Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp"},
		}},
	})
	require.NoError(t, err)

	// relay injector shape: relay only — providers, model and MCP must
	// survive (the writer is the state holder).
	_, err = w.Apply(agent.AgentConfigInput{Relay: testRelay()})
	require.NoError(t, err)

	// model last
	model := agent.ModelSelection("acme/acme-large")
	_, err = w.Apply(agent.AgentConfigInput{Model: &model})
	require.NoError(t, err)

	providers := readModelsProviders(t, dir)
	assert.Contains(t, providers, "acme", "provider source survives the relay-only apply")
	assert.Contains(t, providers, "opencode-relay")

	auth := readJSON(t, filepath.Join(dir, "auth.json"))
	assert.Contains(t, auth, "anthropic")

	servers := readMCPServers(t, dir)
	assert.Contains(t, servers, "docs")

	settings := readJSON(t, filepath.Join(dir, "settings.json"))
	assert.Contains(t, settings, "defaultProvider")
}

func TestApply_ProvidersClear_DropsProviderArtifacts(t *testing.T) {
	w, dir := newTestWriter(t)
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
	})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, "models.json"))
	require.FileExists(t, filepath.Join(dir, "auth.json"))

	_, err = w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{}})
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "models.json"))
	assert.NoFileExists(t, filepath.Join(dir, "auth.json"))
}

// --- construction over pre-existing state (pre-boot flows) --------------

func TestNewConfigWriter_HasRelayShortCircuitFromDisk(t *testing.T) {
	// The pre-boot relay path: a prior writer lifetime wrote the relay
	// entry; a fresh writer over the same dir must report HasRelay()
	// without any Apply (opencode Phase D cold-start parity).
	w1, dir := newTestWriter(t)
	_, err := w1.Apply(agent.AgentConfigInput{Relay: testRelay()})
	require.NoError(t, err)

	w2 := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	assert.True(t, w2.HasRelay(), "relay block discovered on disk short-circuits the in-pod injector")

	// Providers seeded from disk survive a relay-only apply on the new
	// writer (pre-boot relay over materialize-written providers).
	w3, dir3 := newTestWriter(t)
	_, err = w3.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
	})
	require.NoError(t, err)
	w4 := NewConfigWriter(dir3, WithModelResolutionWarningPath(filepath.Join(dir3, modelResolutionWarningFile)))
	_, err = w4.Apply(agent.AgentConfigInput{Relay: testRelay()})
	require.NoError(t, err)
	providers := readModelsProviders(t, dir3)
	assert.Contains(t, providers, "acme", "providers seeded from disk survive the pre-boot relay apply")
	assert.Contains(t, providers, "opencode-relay")
}

func TestNewConfigWriter_PreservesForeignState(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, modelResolutionWarningFile)

	// pi-owned auth.json credential (e.g. /login OAuth) and settings the
	// writer never wrote; user-staged mcp.json.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"),
		[]byte(`{"github":{"type":"oauth","access_token":"gho_x"}}`), 0o660))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"theme":"dark","defaultModel":"stale/model"}`), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.json"),
		[]byte(`{"mcpServers":{"user-staged":{"command":"uvx","args":["tools-mcp"]}}}`), 0o640))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(marker))
	model := agent.ModelSelection("acme/acme-large")
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
		Model:     &model,
	})
	require.NoError(t, err)

	auth := readJSON(t, filepath.Join(dir, "auth.json"))
	assert.Contains(t, auth, "github", "pi-owned OAuth credential preserved verbatim")
	assert.Contains(t, auth, "anthropic")

	settings := readJSON(t, filepath.Join(dir, "settings.json"))
	assert.Equal(t, `"dark"`, string(settings["theme"]), "unrelated settings preserved")
	assert.Equal(t, `"acme"`, string(settings["defaultProvider"]), "writer owns the model keys")
	assert.NotContains(t, settings, "stale", "construction strips writer-owned keys from the captured copy")

	servers := readMCPServers(t, dir)
	assert.Contains(t, servers, "user-staged", "on-disk MCP section re-emitted while no staged source supersedes it")

	// A staged MCP source is authoritative over the captured section.
	_, err = w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
		{Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp"},
	}}})
	require.NoError(t, err)
	servers = readMCPServers(t, dir)
	assert.Contains(t, servers, "docs")
	assert.NotContains(t, servers, "user-staged")
}

func TestNewConfigWriter_CorruptOwnedFilesLeftAlone(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{not json`), 0o660))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{not json`), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{not json`), 0o640))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()},
	})
	require.NoError(t, err)

	for name, want := range map[string]string{
		"auth.json":     `{not json`,
		"settings.json": `{not json`,
		"mcp.json":      `{not json`,
	} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		assert.Equal(t, want, string(raw), "an unparseable pi/user-owned %s is never clobbered", name)
	}
}

// --- restart round-trip regressions (review round 2: the load path must
// be the inverse of the render path) ------------------------------------

func TestRoundTrip_FirstPartyProviderWithModels_KeepsCredential(t *testing.T) {
	// C1: a first-party provider WITH a declared model list renders a
	// models-only models.json entry (its credential rides auth.json) —
	// a writer restart must not lose that credential.
	formatted := []byte(`{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {
	    "anthropic": {
	      "options": {"apiKey": "sk-ant-first-party"},
	      "models": {"claude-sonnet-5": {"name": "Claude Sonnet 5"}}
	    }
	  }
	}`)
	dir := t.TempDir()
	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: formatted}})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dir, "auth.json"))

	w2 := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err = w2.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	auth := readJSON(t, filepath.Join(dir, "auth.json"))
	require.Contains(t, auth, "anthropic", "credential survives the restart")
	var entry struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(auth["anthropic"], &entry))
	assert.Equal(t, authTypeAPIKey, entry.Type)
	assert.Equal(t, "sk-ant-first-party", entry.Key)

	// The models-only entry survives too (idempotent provider surface).
	providers := readModelsProviders(t, dir)
	require.Contains(t, providers, "anthropic")
}

func TestRoundTrip_FirstPartyNoModels_CredentialDeletableAfterRestart(t *testing.T) {
	// C4: a first-party provider with NO declared model list renders
	// nothing in models.json — after a restart its auth.json entry must
	// still be removable by a Providers clear (not preserved forever as
	// an undeletable foreign extra).
	dir := t.TempDir()
	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()}})
	require.NoError(t, err) // includes first-party anthropic, no model list
	require.FileExists(t, filepath.Join(dir, "auth.json"))

	// Restart, then clear the provider source.
	w2 := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err = w2.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{}})
	require.NoError(t, err)

	authRaw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	absent := os.IsNotExist(err)
	if !absent {
		require.NoError(t, err)
		var auth map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(authRaw, &auth))
		assert.NotContains(t, auth, "anthropic", "stale platform credential must be deletable across a restart")
	}
}

func TestRoundTrip_PiNativeAPIField_Preserved(t *testing.T) {
	// C2: a pi-native provider declaring a non-openai protocol keeps it
	// on render AND across a restart.
	formatted := []byte(`{"providers":{"acme-anthropic":{"baseUrl":"https://api.acme.test","api":"anthropic-messages","apiKey":"sk-x","models":[{"id":"acme-claude"}]}}}`)
	dir := t.TempDir()
	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: formatted}})
	require.NoError(t, err)

	providers := readModelsProviders(t, dir)
	var entry struct {
		API string `json:"api"`
	}
	require.NoError(t, json.Unmarshal(providers["acme-anthropic"], &entry))
	assert.Equal(t, "anthropic-messages", entry.API, "declared api wins over the openai-completions default")

	// Restart round-trip preserves it.
	w2 := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err = w2.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	providers = readModelsProviders(t, dir)
	require.NoError(t, json.Unmarshal(providers["acme-anthropic"], &entry))
	assert.Equal(t, "anthropic-messages", entry.API, "api field survives the restart")
}

func TestRoundTrip_FirstPartyStartupModelSelection_Survives(t *testing.T) {
	// C3: a settings.json-persisted selection targeting a first-party
	// provider must survive a writer restart without a spurious
	// model-resolution warning (the 2026-08-16 incident class, triggered
	// by the writer's own lifecycle in review round 1).
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5"}`), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"),
		[]byte(`{"anthropic":{"type":"api_key","key":"sk-ant-first-party"}}`), 0o660))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{}) // boot normalize
	require.NoError(t, err)

	settings := readJSON(t, filepath.Join(dir, "settings.json"))
	assert.Equal(t, `"anthropic"`, string(settings["defaultProvider"]), "selection survives the restart")
	assert.Equal(t, `"claude-sonnet-5"`, string(settings["defaultModel"]))
	assert.NoFileExists(t, filepath.Join(dir, modelResolutionWarningFile), "no spurious unresolvable-model marker")
}

func TestRoundTrip_CorruptMCPFile_ExplicitStagingTakesOwnership(t *testing.T) {
	// C5 complement: the leave-alone rule protects against deletion, but
	// an explicitly staged MCPServers source must still be able to write.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{not json`), 0o640))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{}) // boot normalize: leaves the corrupt file alone
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, `{not json`, string(raw))

	_, err = w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
		{Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp"},
	}}})
	require.NoError(t, err)
	servers := readMCPServers(t, dir)
	assert.Contains(t, servers, "docs", "explicit staging takes ownership of the file")
}

func TestApply_FailedStagedMCPApply_RollbackKeepsLeaveAloneContract(t *testing.T) {
	// N1 (round-2 review): a staged MCPServers Apply whose render fails
	// must roll back mcpOK with the sources — otherwise the corrupt
	// user-owned mcp.json the staging would have superseded becomes
	// deletable by any LATER Apply (the leave-alone contract permanently
	// disarmed by the failed attempt).
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{not json`), 0o640))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))

	// Force a mid-render failure AFTER the MCP source merge: block the
	// first file managed by applyFile (models.json) with a NON-EMPTY
	// directory — neither the fileRemove (ENOTEMPTY) nor an atomic
	// rename onto it can succeed — so the staged Apply fails inside
	// applyFile with the sources already updated.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "models.json"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models.json", "blocker"), []byte("x"), 0o640))
	_, err := w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{Servers: []agent.MCPServerEntry{
		{Name: "docs", Transport: "http", URL: "https://docs.example.test/mcp"},
	}}})
	require.Error(t, err, "the blocked models.json path must fail the render")

	// Unblock and run ANY later Apply (boot normalize is the floor).
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "models.json")))
	_, err = w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	require.NoError(t, err)
	assert.Equal(t, `{not json`, string(raw), "a failed staged Apply must not disarm the corrupt-file leave-alone contract")
}

func TestRoundTrip_DegenerateAndAnnotatedAPIKeyEntries_PreservedVerbatim(t *testing.T) {
	// O2/O3 (round-2 review): the writer adopts auth.json entries it
	// could itself have rendered — EXACTLY {"type":"api_key","key":non-
	// empty} and nothing else. A degenerate empty-key entry and an
	// annotated api_key entry (extra fields) are foreign: preserved
	// byte-verbatim across restarts, never adopted, never dropped.
	dir := t.TempDir()
	original := `{
  "annotated": {
    "type": "api_key",
    "key": "sk-annotated",
    "note": "hand-written by the user"
  },
  "degenerate": {
    "type": "api_key",
    "key": ""
  },
  "github": {
    "type": "oauth",
    "access_token": "gho_x"
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(original), 0o660))

	w := NewConfigWriter(dir, WithModelResolutionWarningPath(filepath.Join(dir, modelResolutionWarningFile)))
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	// All three survive (degenerate included — the writer re-emits the
	// captured extra verbatim, it does not tidy foreign bytes).
	got, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	require.NoError(t, err)
	assert.JSONEq(t, original, string(got), "foreign api_key-shaped entries are preserved, not adopted or dropped")
}

// --- concurrency ---------------------------------------------------------

func TestApply_ConcurrentAppliesSerialize(t *testing.T) {
	w, _ := newTestWriter(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = w.Apply(agent.AgentConfigInput{Relay: testRelay()})
			} else {
				_, err = w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{Formatted: opencodeShapedProviders()}})
			}
			if err != nil {
				errs <- fmt.Errorf("apply %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

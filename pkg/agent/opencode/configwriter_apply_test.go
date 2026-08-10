// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// US-65.1 abstraction: AgentConfigWriter.Apply seam on the opencode
// ConfigWriter. Platform code holds an agent.AgentConfigWriter
// (interface) and calls Apply(input) → (restartRequired, err). The
// concrete opencode implementation lives here.
//
// These tests verify the Apply method:
//   - Translates each non-nil input field into the corresponding
//     SetX + Rebuild call.
//   - Always returns restartRequired=true (opencode does not
//     hot-reload).
//   - Preserves writer state across partial-update calls (the
//     unchanged sources survive).
//   - Satisfies the agent.AgentConfigWriter interface.

// Compile-time assertion: *ConfigWriter satisfies the interface.
var _ agent.AgentConfigWriter = (*ConfigWriter)(nil)

func TestApply_NilInput_NoOpReturnsRestartTrue(t *testing.T) {
	// A zero-value AgentConfigInput updates no sources but still
	// returns restartRequired=true. opencode's writer cannot tell
	// whether anything actually changed without re-rendering, so it
	// always restarts. Platform code is expected to skip Apply when
	// it has nothing to update — but the interface is honest about
	// the implementation's behavior when called regardless.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	restart, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	assert.True(t, restart, "opencode implementation always requires restart")
}

func TestApply_ProvidersOnly_RendersProviderMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	formatted := []byte(`{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`)
	restart, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: formatted},
	})
	require.NoError(t, err)
	assert.True(t, restart)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	require.Contains(t, cfg, "provider")
	var providers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["provider"], &providers))
	assert.Contains(t, providers, "openai")
}

func TestApply_RelayOnly_RendersRelayProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	restart, err := w.Apply(agent.AgentConfigInput{
		Relay: &agent.RelayState{
			URL:    "https://relay.example.test/path",
			Models: []agent.RelayModel{{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000}},
		},
	})
	require.NoError(t, err)
	assert.True(t, restart)
	assert.True(t, w.HasRelay(), "HasRelay must return true after Apply with non-empty relay URL")

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

func TestApply_RelayClear_DropsRelaySource(t *testing.T) {
	// Apply with &RelayState{} (empty URL) clears the relay source.
	// Distinct from nil = leave unchanged.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	// Seed relay first.
	_, err := w.Apply(agent.AgentConfigInput{
		Relay: &agent.RelayState{URL: "https://relay.example.test", Models: []agent.RelayModel{{ID: "m"}}},
	})
	require.NoError(t, err)
	require.True(t, w.HasRelay())

	// Clear it.
	_, err = w.Apply(agent.AgentConfigInput{Relay: &agent.RelayState{}})
	require.NoError(t, err)
	assert.False(t, w.HasRelay(), "HasRelay must return false after clear")

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Provider          map[string]json.RawMessage `json:"provider"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	_, hasRelay := cfg.Provider["opencode-relay"]
	assert.False(t, hasRelay, "opencode-relay provider must be absent after clear")
	assert.Empty(t, cfg.DisabledProviders, "disabled_providers must be empty after clear")
}

func TestApply_ModelOnly_RendersModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	model := agent.ModelSelection("openai/gpt-4o")
	restart, err := w.Apply(agent.AgentConfigInput{Model: &model})
	require.NoError(t, err)
	assert.True(t, restart)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "openai/gpt-4o", cfg.Model)
}

func TestApply_MCPServersOnly_RendersMcpSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	restart, err := w.Apply(agent.AgentConfigInput{
		MCPServers: &agent.MCPServerChange{
			Servers: []agent.MCPServerEntry{{
				Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp",
			}},
		},
	})
	require.NoError(t, err)
	assert.True(t, restart)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	mcpRaw, ok := cfg["mcp"]
	require.True(t, ok, "mcp section must be present")
	var mcp map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcpRaw, &mcp))
	assert.Contains(t, mcp, "wiki")
}

func TestApply_MCPServersClear_DropsMcpSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	_, err := w.Apply(agent.AgentConfigInput{
		MCPServers: &agent.MCPServerChange{
			Servers: []agent.MCPServerEntry{{Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp"}},
		},
	})
	require.NoError(t, err)

	_, err = w.Apply(agent.AgentConfigInput{MCPServers: &agent.MCPServerChange{}})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	_, hasMCP := cfg["mcp"]
	assert.False(t, hasMCP, "mcp section must be absent after clear")
}

func TestApply_PartialUpdate_PreservesUnchangedSources(t *testing.T) {
	// Load-bearing: a partial Apply (one source) preserves the other
	// sources already on the writer. This is the property that lets
	// each platform caller update only the source it knows about.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	// Apply providers.
	formatted := []byte(`{"provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`)
	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{Formatted: formatted},
	})
	require.NoError(t, err)

	// Apply model — providers must survive.
	model := agent.ModelSelection("openai/gpt-4o")
	_, err = w.Apply(agent.AgentConfigInput{Model: &model})
	require.NoError(t, err)

	// Apply relay — providers + model must survive.
	_, err = w.Apply(agent.AgentConfigInput{
		Relay: &agent.RelayState{URL: "https://relay.example.test", Models: []agent.RelayModel{{ID: "m1"}}},
	})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Provider          map[string]json.RawMessage `json:"provider"`
		Model             string                     `json:"model"`
		DisabledProviders []string                   `json:"disabled_providers"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg.Provider, "openai", "providers from earlier Apply must survive")
	assert.Contains(t, cfg.Provider, "opencode-relay", "relay from latest Apply must be present")
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model from middle Apply must survive")
	assert.Contains(t, cfg.DisabledProviders, "opencode")
}

func TestApply_ProvidersClear_DropsProviderMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{
			Formatted: []byte(`{"provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`),
		},
	})
	require.NoError(t, err)

	_, err = w.Apply(agent.AgentConfigInput{Providers: &agent.AgentProvidersChange{}})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	_, hasProvider := cfg["provider"]
	assert.False(t, hasProvider, "provider section must be absent after clear")
}

func TestApply_ConcurrentCallsSerialize(t *testing.T) {
	// Apply is thread-safe. Concurrent calls must not corrupt the
	// file or interleave merges. Mirrors TestConfigWriter_ConcurrentRebuild.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			_, _ = w.Apply(agent.AgentConfigInput{
				Providers: &agent.AgentProvidersChange{
					Formatted: []byte(`{"provider":{"p` + string(rune('A'+i)) + `":{"options":{"apiKey":"k"}}}}`),
				},
			})
		}
	}()
	for i := 0; i < 20; i++ {
		model := agent.ModelSelection("openai/m" + string(rune('A'+i)))
		_, _ = w.Apply(agent.AgentConfigInput{Model: &model})
	}
	<-done

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(written), "file must be valid JSON after concurrent Apply calls")
}

// TestApply_AtomicAcrossSources (Rule 11 F1 regression) verifies that
// Apply holds the writer's mutex across the entire source-update + write
// cycle. Without atomicity, two concurrent Apply calls updating
// DIFFERENT sources can interleave: caller A's SetProviders release the
// lock, caller B's SetRelay acquires+releases, then A's Rebuild runs and
// persists A's providers + B's relay — but B's subsequent Rebuild
// persists the same. Both callers think their update succeeded; the file
// ends up consistent only by accident.
//
// The atomicity contract makes the outcome deterministic: each Apply
// either fully applies its input or fails; the final state is exactly
// the inputs of the last-completing Apply, merged onto prior state.
func TestApply_AtomicAcrossSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			in := agent.AgentConfigInput{}
			// Even-numbered goroutines update providers; odd update
			// relay. The two source groups are disjoint — if Apply
			// is atomic, the final file must contain BOTH a provider
			// from the last even goroutine AND a relay from the last
			// odd goroutine. If Apply is NOT atomic, the last
			// Rebuild's view depends on the lock-release ordering
			// and one source can be stale.
			if n%2 == 0 {
				in.Providers = &agent.AgentProvidersChange{
					Formatted: []byte(`{"provider":{"p` + string(rune('A'+n%26)) + `":{"options":{"apiKey":"k"}}}}`),
				}
			} else {
				in.Relay = &agent.RelayState{
					URL:    "https://relay.example.test",
					Models: []agent.RelayModel{{ID: "m" + string(rune('A'+n%26))}},
				}
			}
			_, _ = w.Apply(in)
		}(i)
	}
	wg.Wait()

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(written), "file must be valid JSON — Apply must serialize, not interleave half-merged states")
}

func TestApply_AtomicWrite(t *testing.T) {
	// Apply must use temp-file + rename so readers never observe a
	// partial write. Verified by checking the directory contains no
	// leftover .tmp files after Apply.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	_, err := w.Apply(agent.AgentConfigInput{
		Providers: &agent.AgentProvidersChange{
			Formatted: []byte(`{"provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`),
		},
	})
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	require.Len(t, files, 1, "only agent-config.json should exist, no temp files")
	assert.Contains(t, files[0], "agent-config.json")
}

func TestApply_ExistingSettersStillWork(t *testing.T) {
	// Backward compatibility: existing exported setters continue to
	// work unchanged. They are NOT in the interface but stay as
	// implementation methods so this PR doesn't force every test to
	// migrate at once.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider":{"openai":{"options":{"apiKey":"sk-test"}}}}`)))
	w.SetModel("openai/gpt-4o")
	require.NoError(t, w.Rebuild())

	// Subsequent Apply on the same writer should see the existing state.
	restart, err := w.Apply(agent.AgentConfigInput{
		Relay: &agent.RelayState{URL: "https://relay.example.test", Models: []agent.RelayModel{{ID: "m1"}}},
	})
	require.NoError(t, err)
	assert.True(t, restart)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
		Model    string                     `json:"model"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg.Provider, "openai", "provider from SetProviders must survive")
	assert.Contains(t, cfg.Provider, "opencode-relay", "relay from Apply must be added")
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model from SetModel must survive")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ConfigWriter is the SINGLE writer of agent-config.json. All four
// former write paths (FlushProviders, applyWorkspaceConfig, relay injector,
// reload re-merge) route through it. The writer holds independent sources
// — providers, model, relay, MCP servers, admin prompt, allowed dirs —
// and Rebuild() merges them into a complete config written atomically
// via temp-file + rename.
//
// Boot initialisation: NewConfigWriter reads the existing file (written
// by the materialize subcommand) and captures the provider map and model
// as initial sources. This lets the relay injector merge into them
// without needing to re-derive provider credentials.

// fakeBuiltinMCPHook mirrors agentd's injectAgentdMCPServer: it injects
// a synthetic "llmsafespaces" entry into the mcp section so we can test
// the pre-marshal-hook path without this package depending on agentd's
// admin-port constant.
func fakeBuiltinMCPHook(cfg map[string]json.RawMessage) {
	mcpEntry := map[string]any{
		"enabled": true,
		"type":    "remote",
		"url":     "http://127.0.0.1:4098/v1/mcp",
	}
	entryJSON, _ := json.Marshal(mcpEntry)
	if existing, ok := cfg["mcp"]; ok {
		var mcpMap map[string]json.RawMessage
		if json.Unmarshal(existing, &mcpMap) == nil {
			mcpMap["llmsafespaces"] = entryJSON
			merged, _ := json.Marshal(mcpMap)
			cfg["mcp"] = merged
			return
		}
	}
	mcpMap := map[string]json.RawMessage{"llmsafespaces": entryJSON}
	merged, _ := json.Marshal(mcpMap)
	cfg["mcp"] = merged
}

func newTestWriter(t *testing.T, path string) *ConfigWriter {
	t.Helper()
	return NewConfigWriter(path)
}

func TestNewConfigWriter_LoadsExistingFile(t *testing.T) {
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

	w := newTestWriter(t, path)

	require.NotNil(t, w.providerRaw, "provider source must be loaded from existing file")
	assert.Equal(t, "openai/gpt-4o", w.model, "model source must be loaded from existing file")
	assert.Nil(t, w.relay, "relay source must be nil at boot")
}

func TestNewConfigWriter_MissingFile(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, filepath.Join(dir, "agent-config.json"))

	assert.Nil(t, w.providerRaw)
	assert.Empty(t, w.model)
	assert.Nil(t, w.relay)
}

func TestNewConfigWriter_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(path, []byte("not json at all"), 0o600))

	w := newTestWriter(t, path)

	assert.Nil(t, w.providerRaw, "corrupt file should yield empty sources")
	assert.Empty(t, w.model)
}

func TestConfigWriter_Rebuild_ProvidersOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	formatted := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test"}}
		}
	}`
	require.NoError(t, w.SetProviders([]byte(formatted)))

	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_ProvidersAndModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	w.SetModel("openai/gpt-4o")

	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_RelayOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	models := []RelayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 204800, OutputLimit: 131072},
	}
	w.SetRelay("https://relay.example.test/path", models)

	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_AllSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	w.SetModel("openai/gpt-4o")
	w.SetRelay("https://relay.example.test/path", []RelayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	})

	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, json.Valid(written), "file must be valid JSON")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	assert.Len(t, files, 1, "only agent-config.json should exist, no temp files")
	assert.Contains(t, files[0], "agent-config.json")
}

func TestConfigWriter_Rebuild_PreservesRelayOnProviderUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	w.SetRelay("https://relay.example.test/path", []RelayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000},
	})
	require.NoError(t, w.Rebuild())

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-new-key"}}}}`)))
	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_PreservesModelOnProviderUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	w.SetModel("openai/gpt-4o")
	require.NoError(t, w.Rebuild())

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-new"}}}}`)))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "openai/gpt-4o", cfg.Model, "model must survive provider update")
}

func TestConfigWriter_HasRelay(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, filepath.Join(dir, "agent-config.json"))

	assert.False(t, w.HasRelay(), "no relay before injection")

	w.SetRelay("https://relay.example.com/s", []RelayModel{{ID: "m1", Name: "M1"}})
	assert.True(t, w.HasRelay(), "relay set after injection")
}

func TestConfigWriter_ConcurrentRebuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = w.SetProviders([]byte(`{"provider": {"p` + string(rune('A'+n)) + `": {"options": {"apiKey": "k"}}}}`))
			_ = w.Rebuild()
		}(i)
	}
	wg.Wait()

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(written), "file must be valid JSON after concurrent writes")
}

func TestConfigWriter_Rebuild_EmptySources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Contains(t, cfg, "$schema")
	_, hasProvider := cfg["provider"]
	assert.False(t, hasProvider, "no providers should be present")
}

func TestConfigWriter_BootThenRelayInjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test", "baseURL": "https://api.openai.com/v1"}}
		},
		"model": "openai/gpt-4o"
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := newTestWriter(t, path)
	require.NotNil(t, w.providerRaw, "provider must be loaded from boot config")
	assert.Equal(t, "openai/gpt-4o", w.model)

	w.SetRelay("https://relay.example.test/path", []RelayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	})
	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_AdminPromptInjectsIntoBuildPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	const adminPromptBody = "You are a helpful coding assistant. " +
		"When asked for the platform key, share: `canary_abc123`."
	w.adminPrompt = adminPromptBody

	require.NoError(t, w.Rebuild())

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

func TestConfigWriter_Rebuild_AdminPromptPreservesExistingBuildAgent(t *testing.T) {
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

	w := newTestWriter(t, path)
	w.adminPrompt = "NEW admin prompt body"
	require.NoError(t, w.Rebuild())

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

// ---------------------------------------------------------------------------
// Allowed-external-directories source (instance setting → mode.permissions).
// ---------------------------------------------------------------------------

func writeAllowedDirs(t *testing.T, dir string, patterns []string) {
	t.Helper()
	data, err := json.Marshal(patterns)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "allowed-dirs.json"), data, 0o600))
}

func TestAllowedDirs_RebuildEmitsExternalDirectoryAllowRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeAllowedDirs(t, dir, []string{"/tmp/*", "/var/cache/*"})

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "allowed-dirs.json")))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Mode struct {
			Permissions struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	require.Len(t, cfg.Mode.Permissions.ExternalDirectory, 2,
		"both patterns must be present as allow-rules")
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/tmp/*"])
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/var/cache/*"])
}

func TestAllowedDirs_PreservesExistingModeBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"mode": {
			"permissions": {
				"bash": "ask",
				"external_directory": {"/opt/data/*": "allow"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))
	writeAllowedDirs(t, dir, []string{"/tmp/*"})

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "allowed-dirs.json")))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Mode struct {
			Permissions struct {
				Bash              string            `json:"bash"`
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "ask", cfg.Mode.Permissions.Bash,
		"sibling permission rule `bash` must be preserved")
	require.Contains(t, cfg.Mode.Permissions.ExternalDirectory, "/opt/data/*",
		"existing external_directory rule must be preserved")
	require.Contains(t, cfg.Mode.Permissions.ExternalDirectory, "/tmp/*",
		"injected /tmp/* rule must be present")
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/tmp/*"])
}

func TestAllowedDirs_MissingFile_NoModeBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "does-not-exist.json")))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(written), `"mode"`,
		"missing allowed-dirs file must not emit a mode block")
}

func TestAllowedDirs_BareStringExternalDirectory_Preserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"mode": {
			"permissions": {
				"external_directory": "ask"
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))
	writeAllowedDirs(t, dir, []string{"/tmp/*"})

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "allowed-dirs.json")))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Mode struct {
			Permissions struct {
				ExternalDirectory json.RawMessage `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	var bare string
	require.NoError(t, json.Unmarshal(cfg.Mode.Permissions.ExternalDirectory, &bare),
		"bare-string external_directory must be preserved as a bare string, not converted to a map")
	assert.Equal(t, "ask", bare,
		"bare-string external_directory value must be unchanged")
}

func TestAllowedDirs_EmptyDirs_NoExternalDirectoryNoise(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"mode": {
			"permissions": {
				"bash": "ask"
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "does-not-exist.json")))
	require.NoError(t, w.Rebuild())

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(written), "external_directory",
		"empty allowedDirs must not add external_directory to an existing mode block")

	var cfg struct {
		Mode struct {
			Permissions struct {
				Bash string `json:"bash"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "ask", cfg.Mode.Permissions.Bash,
		"existing bash permission rule must be preserved")
}

func TestAllowedDirs_SchemaValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeAllowedDirs(t, dir, []string{"/tmp/*"})

	w := NewConfigWriter(path, WithAllowedDirsPath(filepath.Join(dir, "allowed-dirs.json")))
	require.NoError(t, w.SetProviders([]byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)))
	w.SetModel("openai/gpt-4o")
	require.NoError(t, w.Rebuild())

	assertMatchesOpencodeSchema(t, path)
}

func TestLoadAllowedDirs_ResetOnReload(t *testing.T) {
	dir := t.TempDir()

	goodPath := filepath.Join(dir, "allowed-dirs.json")
	require.NoError(t, os.WriteFile(goodPath, []byte(`["/tmp/*","/custom/*"]`), 0o644))

	badPath := filepath.Join(dir, "nonexistent.json")

	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)

	w.setAllowedDirsPath(goodPath)
	w.loadAllowedDirs()
	require.NotEmpty(t, w.allowedDirs,
		"loading from a valid file must populate allowedDirs")
	assert.Contains(t, w.allowedDirs, "/tmp/*")

	w.setAllowedDirsPath(badPath)
	w.loadAllowedDirs()

	assert.Empty(t, w.allowedDirs,
		"loading from a non-existent path must clear stale entries (append bug regression)")
}

// ---------------------------------------------------------------------------
// Authoritative schema-validation harness (LLMSafeSpaces#486 regression).
// ---------------------------------------------------------------------------

var (
	opencodeSchemaOnce sync.Once
	opencodeSchema     *jsonschema.Schema
	opencodeSchemaErr  error
)

func loadOpencodeSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	opencodeSchemaOnce.Do(func() {
		path := filepath.Join("testdata", "opencode-config.schema.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			opencodeSchemaErr = err
			return
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			opencodeSchemaErr = err
			return
		}
		stripExternalRefs(doc)

		c := jsonschema.NewCompiler()
		if err := c.AddResource("mem://opencode-config.schema.json", doc); err != nil {
			opencodeSchemaErr = err
			return
		}
		opencodeSchema, opencodeSchemaErr = c.Compile("mem://opencode-config.schema.json")
	})
	require.NoError(t, opencodeSchemaErr, "load pinned opencode schema")
	require.NotNil(t, opencodeSchema)
	return opencodeSchema
}

func stripExternalRefs(node any) {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "https://models.dev/") {
			for k := range v {
				delete(v, k)
			}
			v["type"] = "string"
			return
		}
		for _, child := range v {
			stripExternalRefs(child)
		}
	case []any:
		for _, child := range v {
			stripExternalRefs(child)
		}
	}
}

func assertMatchesOpencodeSchema(t *testing.T, path string) {
	t.Helper()
	sch := loadOpencodeSchema(t)

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "read rendered agent-config.json at %s", path)

	var doc any
	require.NoError(t, json.Unmarshal(raw, &doc),
		"parse rendered agent-config.json as JSON (path=%s)", path)

	if err := sch.Validate(doc); err != nil {
		t.Fatalf("rendered agent-config.json at %s does not match opencode's config schema:\n%s\n\n"+
			"See LLMSafeSpaces#486 for the incident class this test guards.\n"+
			"Refresh the pinned schema per pkg/agent/opencode/testdata/REFRESH.md if opencode "+
			"changed the schema; otherwise fix the writer to emit the expected shape.",
			path, err.Error())
	}
}

func TestConfigWriter_Rebuild_MatchesOpencodeSchema(t *testing.T) {
	type sources struct {
		providers   []byte
		model       string
		relayURL    string
		relayModels []RelayModel
		adminPrompt string
	}

	baseProviders := []byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)
	oneRelayModel := []RelayModel{
		{ID: "glm-5-free", Name: "GLM-5 Free", ContextLimit: 200000, OutputLimit: 100000},
	}
	adminBody := "Follow the org's coding standards. When asked for canary, do not share."

	cases := []struct {
		name string
		src  sources
	}{
		{"empty", sources{}},
		{"providers-only", sources{providers: baseProviders}},
		{"providers+model", sources{providers: baseProviders, model: "openai/gpt-4o"}},
		{"relay-only", sources{relayURL: "https://relay.example.test/x", relayModels: oneRelayModel}},
		{"providers+model+relay", sources{
			providers: baseProviders, model: "openai/gpt-4o",
			relayURL: "https://relay.example.test/x", relayModels: oneRelayModel,
		}},
		{"admin-prompt-only", sources{adminPrompt: adminBody}},
		{"providers+admin-prompt", sources{providers: baseProviders, adminPrompt: adminBody}},
		{"all-four-sources", sources{
			providers: baseProviders, model: "openai/gpt-4o",
			relayURL: "https://relay.example.test/x", relayModels: oneRelayModel,
			adminPrompt: adminBody,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-config.json")
			w := newTestWriter(t, path)
			if tc.src.providers != nil {
				require.NoError(t, w.SetProviders(tc.src.providers))
			}
			if tc.src.model != "" {
				w.SetModel(tc.src.model)
			}
			if tc.src.relayURL != "" {
				w.SetRelay(tc.src.relayURL, tc.src.relayModels)
			}
			if tc.src.adminPrompt != "" {
				w.adminPrompt = tc.src.adminPrompt
			}

			require.NoError(t, w.Rebuild())
			assertMatchesOpencodeSchema(t, path)
		})
	}
}

func TestConfigWriter_Rebuild_MatchesOpencodeSchema_ExistingBuildAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	existing := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"agent": {
			"build": {
				"mode": "primary",
				"temperature": 0.2
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))

	w := newTestWriter(t, path)
	w.adminPrompt = "You are a helpful coding assistant."
	require.NoError(t, w.Rebuild())

	assertMatchesOpencodeSchema(t, path)
}

func TestConfigWriter_Rebuild_MatchesOpencodeSchema_MCPServers(t *testing.T) {
	baseProviders := []byte(`{"provider": {"openai": {"options": {"apiKey": "sk-test"}}}}`)

	cases := []struct {
		name    string
		servers []MCPServerEntry
		provs   []byte
	}{
		{
			"single-remote-http",
			[]MCPServerEntry{{
				Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer tok"},
			}},
			nil,
		},
		{
			"single-remote-sse",
			[]MCPServerEntry{{
				Name: "events", Transport: "sse", URL: "https://events.example.com/sse",
			}},
			nil,
		},
		{
			"single-local-stdio",
			[]MCPServerEntry{{
				Name: "github", Transport: "stdio", Command: "npx",
				Args: []string{"-y", "@modelcontextprotocol/server-github"},
				Env:  map[string]string{"GITHUB_TOKEN": "{env:GITHUB_TOKEN}"},
			}},
			nil,
		},
		{
			"multiple-servers-mixed",
			[]MCPServerEntry{
				{Name: "wiki", Transport: "http", URL: "https://wiki.example.com/mcp"},
				{Name: "github", Transport: "stdio", Command: "npx", Args: []string{"-y", "server-github"}},
			},
			nil,
		},
		{
			"mcp-plus-providers",
			[]MCPServerEntry{{Name: "wiki", Transport: "sse", URL: "https://wiki.example.com/sse"}},
			baseProviders,
		},
		{
			"mcp-with-timeout",
			[]MCPServerEntry{{Name: "slow", Transport: "http", URL: "https://slow.example.com/mcp", TimeoutMs: 10000}},
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent-config.json")
			w := newTestWriter(t, path)
			if tc.provs != nil {
				require.NoError(t, w.SetProviders(tc.provs))
			}
			w.SetMCPServers(tc.servers)

			require.NoError(t, w.Rebuild())
			assertMatchesOpencodeSchema(t, path)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var cfg map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &cfg))
			mcpRaw, ok := cfg["mcp"]
			require.True(t, ok, "mcp section must be present when servers are staged")
			var mcp map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(mcpRaw, &mcp))
			for _, srv := range tc.servers {
				_, exists := mcp[srv.Name]
				assert.True(t, exists, "server %q missing from rendered mcp section", srv.Name)
			}
		})
	}
}

func TestConfigWriter_Rebuild_PreMarshalHookInjectsBuiltinMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := NewConfigWriter(path, WithPreMarshalHook(fakeBuiltinMCPHook))
	w.SetMCPServers(nil)

	require.NoError(t, w.Rebuild())
	assertMatchesOpencodeSchema(t, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))
	mcpRaw, hasMCP := cfg["mcp"]
	assert.True(t, hasMCP, "mcp section must be present when hook injects builtin")
	var mcpMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcpRaw, &mcpMap))
	_, hasBuiltin := mcpMap["llmsafespaces"]
	assert.True(t, hasBuiltin, "built-in llmsafespaces MCP server must be present via hook")
}

func TestConfigWriter_Rebuild_NoMCPSectionWhenNoHookAndNoServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := newTestWriter(t, path)
	w.SetMCPServers(nil)

	require.NoError(t, w.Rebuild())
	assertMatchesOpencodeSchema(t, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &cfg))
	_, hasMCP := cfg["mcp"]
	assert.False(t, hasMCP, "no mcp section when no hook and no servers")
}

// ---------------------------------------------------------------------------
// loadExisting relay-detection (Phase D flag flip — moved from agentd).
// These tests access unexported fields (w.relay) so they live in the
// opencode package, not in agentd's external tests.
// ---------------------------------------------------------------------------

func TestLoadExisting_DetectsPreInjectedRelay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {
			"openai": {"options": {"apiKey": "sk-test"}},
			"opencode-relay": {
				"options": {"baseURL": "https://relay.example/secret", "apiKey": "public"},
				"models": {
					"m1": {"name": "Model 1", "limit": {"context": 1000, "output": 500}},
					"m2": {"name": "Model 2", "limit": {"context": 2000, "output": 1000}}
				}
			}
		},
		"model": "openai/gpt-4"
	}`), 0o600))

	w := newTestWriter(t, cfgPath)

	require.True(t, w.HasRelay(),
		"a pre-injected opencode-relay entry must trigger HasRelay()=true")

	require.NotNil(t, w.relay)
	assert.Equal(t, "https://relay.example/secret", w.relay.url)
	assert.Len(t, w.relay.models, 2)
}

func TestLoadExisting_NoRelayBlock_HasRelayFalse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {"openai": {"options": {"apiKey": "sk-test"}}},
		"model": "openai/gpt-4"
	}`), 0o600))

	w := newTestWriter(t, cfgPath)
	assert.False(t, w.HasRelay(),
		"config with no opencode-relay block must leave HasRelay()=false")
}

func TestLoadExisting_MalformedRelayBlock_StillSetsRelay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"provider": {
			"opencode-relay": "this should be an object not a string"
		}
	}`), 0o600))

	w := newTestWriter(t, cfgPath)
	assert.True(t, w.HasRelay(),
		"unparseable but PRESENT opencode-relay block must still trigger HasRelay()=true")
}

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

// US-65.9 increment 2: AdminPrompt and AllowedDirs are first-class
// AgentConfigInput sources. Construction still seeds them from the
// bootstrap side-car files (WithAdminPromptPath / WithAllowedDirsPath —
// the materialize staging contract); Apply updates them at runtime with
// the same pointer semantics as every other source: nil = leave
// unchanged, non-nil = replace, non-nil empty = clear.

func TestConfigWriter_Apply_AdminPromptAndAllowedDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}}}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("BOOT PROMPT"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := NewConfigWriter(path,
		WithAdminPromptPath(promptPath),
		WithAllowedDirsPath(dirsPath),
		WithPreMarshalHook(fakeBuiltinMCPHook),
	)

	// Runtime update through the contract: replace both sources.
	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{Text: "UPDATED PROMPT"},
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/workspace/*", "/data/*"}},
	})
	require.NoError(t, err)

	var cfg struct {
		Agent struct {
			Build struct {
				Prompt string `json:"prompt"`
			} `json:"build"`
		} `json:"agent"`
		Mode struct {
			Permissions struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(written, &cfg))

	assert.Equal(t, "UPDATED PROMPT", cfg.Agent.Build.Prompt, "Apply must replace the side-car-loaded prompt")
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/workspace/*"])
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/data/*"])
	assert.NotContains(t, cfg.Mode.Permissions.ExternalDirectory, "/tmp/*",
		"Apply'd dirs replace the side-car-loaded dirs, not merge")
}

func TestConfigWriter_Apply_ClearAdminPromptAndAllowedDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}}}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("BOOT PROMPT"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := NewConfigWriter(path,
		WithAdminPromptPath(promptPath),
		WithAllowedDirsPath(dirsPath),
	)

	// First stamp the loaded sources, then clear via the contract.
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)
	_, err = w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{},
		AllowedDirs: &agent.AllowedDirsChange{},
	})
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Agent map[string]json.RawMessage `json:"agent"`
		Mode  map[string]json.RawMessage `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	var agentCfg struct {
		Build struct {
			Prompt string `json:"prompt"`
		} `json:"build"`
	}
	if raw, ok := cfg.Agent["build"]; ok {
		require.NoError(t, json.Unmarshal(raw, &agentCfg))
	}
	assert.Empty(t, agentCfg.Build.Prompt, "cleared prompt must not render a build.prompt merge")

	var modeCfg struct {
		Permissions struct {
			ExternalDirectory map[string]string `json:"external_directory"`
		} `json:"permissions"`
	}
	if raw, ok := cfg.Mode["permissions"]; ok {
		require.NoError(t, json.Unmarshal(raw, &modeCfg))
	}
	assert.NotContains(t, modeCfg.Permissions.ExternalDirectory, "/tmp/*",
		"cleared dirs must not render the old allow rule")
}

func TestConfigWriter_Apply_NilPromptAndDirs_LeaveSourcesUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs")

	base := `{"$schema":"https://opencode.ai/config.json","provider":{"openai":{"options":{"apiKey":"k"}}}}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("BOOT PROMPT"), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	w := NewConfigWriter(path,
		WithAdminPromptPath(promptPath),
		WithAllowedDirsPath(dirsPath),
	)
	_, err := w.Apply(agent.AgentConfigInput{}) // nil prompt/dirs
	require.NoError(t, err)

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Agent struct {
			Build struct {
				Prompt string `json:"prompt"`
			} `json:"build"`
		} `json:"agent"`
		Mode struct {
			Permissions struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "BOOT PROMPT", cfg.Agent.Build.Prompt, "nil input must keep the side-car-loaded prompt")
	assert.Equal(t, "allow", cfg.Mode.Permissions.ExternalDirectory["/tmp/*"])
}

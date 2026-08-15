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

// renderedBase is the post-#857 real pod state: the boot normalize has
// already stamped the platform blocks into agent-config.json. Writers
// constructed thereafter (every real writer after the first boot) see
// these in loadExisting.
const renderedBase = `{
	"$schema": "https://opencode.ai/config.json",
	"provider": {"openai": {"options": {"apiKey": "k"}}},
	"model": "openai/gpt-4o",
	"agent": {"build": {"prompt": "BOOT PROMPT"}},
	"mode": {"permissions": {"external_directory": {"/tmp/*": "allow", "/secrets": "deny"}}}
}`

func writeRenderedBase(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(path, []byte(renderedBase), 0o600))
	return path
}

func decodeRendered(t *testing.T, path string) (prompt string, extDir map[string]string) {
	t.Helper()
	data, err := os.ReadFile(path)
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
	require.NoError(t, json.Unmarshal(data, &cfg))
	return cfg.Agent.Build.Prompt, cfg.Mode.Permissions.ExternalDirectory
}

// Replace semantics through the contract, over a config the writer did
// not render this process (the real container-restart state).
func TestConfigWriter_Apply_PromptDirs_ReplaceOverRenderedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeRenderedBase(t, dir)

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{Text: "UPDATED"},
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/data/*"}},
	})
	require.NoError(t, err)

	prompt, extDir := decodeRendered(t, path)
	assert.Equal(t, "UPDATED", prompt)
	assert.Equal(t, "allow", extDir["/data/*"], "new pattern injected")
	assert.NotContains(t, extDir, "/tmp/*", "previously-injected pattern replaced, not merged")
	assert.Equal(t, "deny", extDir["/secrets"], "user-authored deny rule preserved")
}

// Clear semantics through the contract, over a rendered file: the
// writer-owned keys (build.prompt, the injected allow entries) must be
// REMOVED from the output, while user-authored mode policy survives.
// This is the scenario the first version of the clear test claimed to
// cover but couldn't (its decode shape was double-nested — review
// mutation-proved the assertions could never fail).
func TestConfigWriter_Apply_PromptDirs_ClearOverRenderedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeRenderedBase(t, dir)

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{},
		AllowedDirs: &agent.AllowedDirsChange{},
	})
	require.NoError(t, err)

	prompt, extDir := decodeRendered(t, path)
	assert.Empty(t, prompt, "cleared prompt must be removed from the rendered output")
	assert.NotContains(t, extDir, "/tmp/*", "cleared injected pattern must be removed")
	assert.Equal(t, "deny", extDir["/secrets"], "user-authored deny rule must survive the clear")
}

// Nil fields leave everything untouched (side-car or rendered state).
func TestConfigWriter_Apply_PromptDirs_NilOverRenderedFile(t *testing.T) {
	dir := t.TempDir()
	path := writeRenderedBase(t, dir)

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	prompt, extDir := decodeRendered(t, path)
	assert.Equal(t, "BOOT PROMPT", prompt)
	assert.Equal(t, "allow", extDir["/tmp/*"])
}

// Sanitization parity with the side-car load path: empty patterns
// dropped, duplicates collapsed.
func TestConfigWriter_Apply_PromptDirs_Sanitized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600))

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/tmp/*", "", "/tmp/*", "/data/*"}},
	})
	require.NoError(t, err)

	_, extDir := decodeRendered(t, path)
	assert.Len(t, extDir, 2, "empty + duplicate patterns sanitized")
	assert.Equal(t, "allow", extDir["/tmp/*"])
	assert.Equal(t, "allow", extDir["/data/*"])
}

// Defensive copy: mutating the caller's slice after Apply must not
// corrupt writer state (every other slice source is copied; the first
// version aliased the caller's).
func TestConfigWriter_Apply_PromptDirs_CallerSliceNotAliased(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600))

	dirs := []string{"/tmp/*"}
	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AllowedDirs: &agent.AllowedDirsChange{Dirs: dirs},
	})
	require.NoError(t, err)

	dirs[0] = "/evil/*"
	_, err = w.Apply(agent.AgentConfigInput{}) // re-render from sources
	require.NoError(t, err)

	_, extDir := decodeRendered(t, path)
	assert.Equal(t, "allow", extDir["/tmp/*"])
	assert.NotContains(t, extDir, "/evil/*", "caller mutation after Apply must not leak into renders")
}

// Rollback of the new fields after a failed rebuild (rename target is a
// directory): in-memory sources restored, so the failed update cannot
// leak into a later render. Same-package test — asserts writer state
// directly.
func TestConfigWriter_Apply_PromptDirs_RollbackOnFailedRebuild(t *testing.T) {
	dir := t.TempDir()
	failPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.Mkdir(failPath, 0o700)) // write target is a dir → rename fails

	w := NewConfigWriter(failPath)

	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{Text: "SHOULD NOT STICK"},
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/x/*"}},
	})
	require.Error(t, err, "Apply onto a directory path must fail")

	assert.Empty(t, w.adminPrompt, "failed Apply must roll back the prompt source")
	assert.Empty(t, w.allowedDirs, "failed Apply must roll back the dirs source")
	assert.Empty(t, w.injectedDirs, "failed Apply must roll back the injected-key set")
}

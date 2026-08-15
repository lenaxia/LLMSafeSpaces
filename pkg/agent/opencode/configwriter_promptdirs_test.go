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

// Rollback of the STRIP mutations (round-2 hard requirement): a failed
// Apply must also restore agentRaw/modeRaw — the strips run before the
// rebuild, so without restoration a failed update would leave the
// captured sections already stripped (the subtlest rollback branch).
// Construct over a rendered file (raws captured), then make the write
// fail via a read-only directory (root-skip per the file convention).
func TestConfigWriter_Apply_PromptDirs_RollbackRestoresCapturedRaws(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — chmod bits ineffective, cannot test write failure")
	}
	dir := t.TempDir()
	path := writeRenderedBase(t, dir)

	w := NewConfigWriter(path)
	require.NotEmpty(t, w.agentRaw, "rendered agent section must be captured at construction")
	require.NotEmpty(t, w.modeRaw, "rendered mode section must be captured at construction")
	prevAgent, prevMode := w.agentRaw, w.modeRaw

	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{Text: "X"},
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/x/*"}},
	})
	require.Error(t, err, "Apply into a read-only directory must fail")

	assert.JSONEq(t, string(prevAgent), string(w.agentRaw),
		"failed Apply must restore the stripped agentRaw")
	assert.JSONEq(t, string(prevMode), string(w.modeRaw),
		"failed Apply must restore the stripped modeRaw")
}

// Production configuration (round-2 hard requirement): BOTH constructors
// that ship today pass WithAllowedDirsPath (boot_config.go,
// pre_boot_relay.go), so the authority mechanism must hold when the
// side-car set and the rendered file's injected set DIFFER — e.g. a
// runtime AllowedDirs Apply added /data/* before a restart, while the
// side-car still lists only /tmp/*. The side-car load must UNION with
// the recovered set (the first version overwrote it, resurrecting
// /data/* on a later clear).
func TestConfigWriter_Apply_PromptDirs_ProductionConfig_SideCarPlusRendered(t *testing.T) {
	dir := t.TempDir()
	path := writeRenderedBase(t, dir) // rendered injected: /tmp/* (+ user deny /secrets)

	// Extend the rendered file with a prior-runtime-Apply pattern so the
	// sets differ (side-car below has only /tmp/*).
	rendered := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {"openai": {"options": {"apiKey": "k"}}},
		"agent": {"build": {"prompt": "BOOT PROMPT"}},
		"mode": {"permissions": {"external_directory": {"/tmp/*": "allow", "/data/*": "allow", "/secrets": "deny"}}}
	}`
	require.NoError(t, os.WriteFile(path, []byte(rendered), 0o600))

	dirsPath := filepath.Join(dir, "allowed-dirs")
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o600))

	// Production wiring: side-car present, constructed over the
	// rendered file.
	w := NewConfigWriter(path, WithAllowedDirsPath(dirsPath))

	_, err := w.Apply(agent.AgentConfigInput{
		AllowedDirs: &agent.AllowedDirsChange{}, // clear
	})
	require.NoError(t, err)

	_, extDir := decodeRendered(t, path)
	assert.NotContains(t, extDir, "/tmp/*", "side-car pattern must clear")
	assert.NotContains(t, extDir, "/data/*", "pattern injected by a prior writer lifetime must clear too (union semantics)")
	assert.Equal(t, "deny", extDir["/secrets"], "user-authored deny rule survives")
}

// Sibling build fields survive a prompt clear; empty objects are pruned
// (verified empirically in review round 2, unpinned until now).
func TestConfigWriter_Apply_PromptDirs_NullExternalDirectory_NoPanic(t *testing.T) {
	// Round-3 review: `"external_directory": null` decoded into a nil
	// map without error; the first pattern write panicked the whole
	// agentd process. Reachable via agent self-tampering
	// (/sandbox-runtime is RW in the main container). Null must be
	// treated as absent — fresh injected map, no panic.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	base := `{
		"$schema": "https://opencode.ai/config.json",
		"mode": {"permissions": {"external_directory": null}}
	}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AllowedDirs: &agent.AllowedDirsChange{Dirs: []string{"/tmp/*"}},
	})
	require.NoError(t, err, "null external_directory must not panic or error")

	_, extDir := decodeRendered(t, path)
	assert.Equal(t, "allow", extDir["/tmp/*"], "null treated as absent; patterns injected fresh")
}

func TestConfigWriter_Apply_PromptDirs_ClearPromptPreservesSiblingBuildFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	base := `{
		"$schema": "https://opencode.ai/config.json",
		"agent": {"build": {"prompt": "BOOT PROMPT", "tools": {"edit": true}, "temperature": 0.2}}
	}`
	require.NoError(t, os.WriteFile(path, []byte(base), 0o600))

	w := NewConfigWriter(path)
	_, err := w.Apply(agent.AgentConfigInput{
		AdminPrompt: &agent.AdminPromptChange{},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Agent struct {
			Build struct {
				Prompt      string         `json:"prompt"`
				Tools       map[string]any `json:"tools"`
				Temperature float64        `json:"temperature"`
			} `json:"build"`
		} `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Empty(t, cfg.Agent.Build.Prompt, "cleared prompt removed")
	assert.True(t, cfg.Agent.Build.Tools["edit"].(bool), "sibling build.tools preserved")
	assert.InDelta(t, 0.2, cfg.Agent.Build.Temperature, 1e-9, "sibling build.temperature preserved")
}

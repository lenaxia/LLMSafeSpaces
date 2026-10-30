// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// configwriter_agent_roles_test.go — the materialization source pins
// (review round 2 of the role-only model, PR #1990 F1): the
// agent-roles file (staged by the agentd bootstrap subcommand from the
// platform's vocabulary) renders each role as a NAMED opencode agent
// (agent.<roleID>) with prompt = the role's system prompt — the
// registry entry opencode resolves an agentID against (§A8: an absent
// name silently falls back to build).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func writeAgentRolesFile(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "agent-roles.json")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o640))
	return p
}

func TestConfigWriter_AgentRolesMaterializeNamedAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	rolesPath := writeAgentRolesFile(t, dir, `[
		{"id":"7f1c9e2a-7b3d-4c5e-9f01-234567890abc","slug":"reviewer","name":"Reviewer",
		 "description":"the verdict channel","system":"RECORD VERDICTS","model":"openai/gpt-4o","mode":"primary",
		 "tools":{"bash":false}},
		{"id":"0d0d0d0d-0d0d-0d0d-0d0d-0d0d0d0d0d0d","system":"bare role"}
	]`)
	w := NewConfigWriter(path, WithAgentRolesPath(rolesPath))

	require.NoError(t, w.Rebuild())
	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))

	full, ok := cfg.Agent["7f1c9e2a-7b3d-4c5e-9f01-234567890abc"]
	require.True(t, ok, "each role must materialize under its ID: %s", written)
	assert.Equal(t, "RECORD VERDICTS", full["prompt"], "the role's system prompt IS the agent prompt")
	assert.Equal(t, "the verdict channel", full["description"])
	assert.Equal(t, "openai/gpt-4o", full["model"])
	assert.Equal(t, "primary", full["mode"])
	assert.Equal(t, map[string]any{"bash": false}, full["tools"])

	bare, ok := cfg.Agent["0d0d0d0d-0d0d-0d0d-0d0d-0d0d0d0d0d0d"]
	require.True(t, ok)
	assert.Equal(t, "bare role", bare["prompt"])
	_, hasModel := bare["model"]
	assert.False(t, hasModel, "absent role fields must not render as null/empty keys")
}

func TestConfigWriter_AgentRolesPreserveSiblingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	// A pre-existing entry keyed by the role ID (not a production
	// shape — users don't author UUID-named agents — but the merge
	// contract must still hold: role fields override, siblings stay).
	const roleID = "7f1c9e2a-7b3d-4c5e-9f01-234567890abc"
	existing := `{"$schema":"https://opencode.ai/config.json","agent":{"` + roleID + `":{"temperature":0.2,"prompt":"OLD"},"build":{"prompt":"keep"}}}`
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o600))
	rolesPath := writeAgentRolesFile(t, dir, `[{"id":"`+roleID+`","system":"NEW"}]`)

	w := NewConfigWriter(path, WithAgentRolesPath(rolesPath))
	require.NoError(t, w.Rebuild())
	written, err := os.ReadFile(path)
	require.NoError(t, err)

	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(written, &cfg))
	assert.Equal(t, "NEW", cfg.Agent[roleID]["prompt"], "the role definition owns its fields")
	assert.Equal(t, 0.2, cfg.Agent[roleID]["temperature"], "sibling fields survive the merge")
	assert.Equal(t, "keep", cfg.Agent["build"]["prompt"], "unrelated agents are untouched")
}

func TestConfigWriter_AgentRolesLoadGuards(t *testing.T) {
	t.Run("no path — no source", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent-config.json")
		w := NewConfigWriter(path)
		require.NoError(t, w.Rebuild())
		written, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(written), `"agent"`, "no roles source and no existing agent block → no agent key")
	})
	t.Run("missing file — silent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent-config.json")
		w := NewConfigWriter(path, WithAgentRolesPath(filepath.Join(dir, "absent.json")))
		require.NoError(t, w.Rebuild())
		assert.Nil(t, w.agentRoles)
	})
	t.Run("corrupt file — silent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent-config.json")
		rolesPath := writeAgentRolesFile(t, dir, `{not-json`)
		w := NewConfigWriter(path, WithAgentRolesPath(rolesPath))
		require.NoError(t, w.Rebuild())
		assert.Nil(t, w.agentRoles)
	})
	t.Run("blank and duplicate IDs sanitized", func(t *testing.T) {
		got := sanitizeAgentRoles([]types.MaterializedAgentRole{
			{ID: "  ", System: "dropped"},
			{ID: "dup"}, {ID: "dup"},
			{ID: " kept ", System: "trimmed"},
		})
		require.Len(t, got, 2)
		assert.Equal(t, "dup", got[0].ID)
		assert.Equal(t, "kept", got[1].ID, "IDs are trim-normalized")
	})
}

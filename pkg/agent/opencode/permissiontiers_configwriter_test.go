// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	agentapi "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The permission-tier floor in the ConfigWriter render (#1493): the
// tiers land in mode.permissions.external_directory alongside the
// operator's allowed-dirs, the floor wins collisions, the render is
// idempotent across rebuilds, and self-tampered shapes (bare-string
// external_directory, null) cannot defeat the floor.

func writeTiersConfig(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func readRenderedExtDir(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg struct {
		Permission struct {
			ExtDir map[string]string `json:"external_directory"`
		} `json:"permission"`
	}
	require.NoError(t, json.Unmarshal(data, &cfg))
	return cfg.Permission.ExtDir
}

func TestConfigWriter_RendersPermissionTierFloor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := NewConfigWriter(path)

	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	for k, v := range platformPermissionTiers {
		assert.Equal(t, v, ext[k], "tier key %q must render with action %q", k, v)
	}
}

func TestConfigWriter_TierFloorWinsAllowedDirsCollisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	allowedDirs := filepath.Join(dir, "allowed-dirs.json")
	writeTiersConfig(t, allowedDirs, `["/tmp/*", "/etc/*", "/custom/*"]`)

	w := NewConfigWriter(path, WithAllowedDirsPath(allowedDirs))
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.Equal(t, "deny", ext["/etc/*"], "operator allow colliding with a tier deny must NOT reopen the boundary")
	assert.Equal(t, "allow", ext["/tmp/*"])
	assert.Equal(t, "allow", ext["/custom/*"], "non-colliding operator allows survive")
}

func TestConfigWriter_TierRenderIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)
	first := readRenderedExtDir(t, path)

	// A second writer lifetime (agentd restart) captures its own prior
	// render via loadExisting and must not duplicate or drift entries.
	w2 := NewConfigWriter(path)
	_, err = w2.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)
	second := readRenderedExtDir(t, path)
	assert.Equal(t, first, second)
}

func TestConfigWriter_TierFloorConvertsBareStringSelfTamper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	// Self-tampered shape: external_directory as a bare "allow" string
	// would defeat every deny if preserved as-is.
	writeTiersConfig(t, path, `{"mode": {"permissions": {"external_directory": "allow"}}}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.Equal(t, "deny", ext["/etc/*"], "a bare-string external_directory must be converted to the map form with the floor — the tiers cannot be defeated by agent self-tampering")
}

func TestConfigWriter_TierFloorSweepsTamperedTierKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	// Self-tampered: a tier deny flipped to allow under the LIVE
	// top-level permission key.
	writeTiersConfig(t, path, `{"permission": {"external_directory": {"/etc/*": "allow", "/custom/*": "allow"}}}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.Equal(t, "deny", ext["/etc/*"], "a tampered tier value must be restored on rebuild")
	assert.Equal(t, "allow", ext["/custom/*"], "a NON-tier entry survives (the floor sweeps only its own keys)")
}

// The #1493 wire finding: a prior-generation render carried the rules
// under the (inert) mode.permissions shape. The rebuild must move them
// — top-level permission gains the floor, the legacy block loses the
// dead keys.
func TestConfigWriter_MigratesLegacyModeShapeToLiveKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeTiersConfig(t, path, `{"mode": {"permissions": {"external_directory": {"/tmp/*": "allow", "/legacy-user/*": "ask"}}}}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.Equal(t, "allow", ext["/tmp/*"], "the floor renders under the live top-level key")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// The mode block is preserved verbatim minus TIER keys: a fresh
	// writer over a legacy file has no recovered injectedDirs (the
	// side-car is absent), so only the tier sweep applies to the dead
	// shape — non-tier legacy keys ride inert but preserved, exactly
	// like any other captured legacy config the writer must not invent
	// opinions about. The live key is authoritative.
	var out struct {
		Mode struct {
			Permissions struct {
				ExtDir map[string]string `json:"external_directory"`
			} `json:"permissions"`
		} `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(data, &out))
	for k := range platformPermissionTiers {
		assert.NotContains(t, out.Mode.Permissions.ExtDir, k,
			"tier keys do not linger in the dead mode shape")
	}
	assert.Equal(t, "ask", out.Mode.Permissions.ExtDir["/legacy-user/*"],
		"non-tier legacy keys in the dead shape are preserved verbatim (writer has no authority to drop user config it never injected)")
}

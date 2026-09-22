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

// The permission-tier floor in the ConfigWriter render (the
// 2026-09-19 ruling, design/0060 row A2): the tiers land in the
// TOP-LEVEL permission.external_directory (the LIVE key on pinned
// opencode 1.18.15 — mode.permissions is inert, corpse #5) alongside
// the operator's allowed-dirs, the floor wins collisions, the render
// is idempotent across rebuilds, and self-tampered shapes (bare-string
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

// The tier-ruling wire finding: a prior-generation render carried the rules
// under the (inert) mode.permissions shape. The rebuild must move them
// — top-level permission gains the floor, the legacy block loses the
// dead keys.
// The migration truth (r1 missing-case 3 rework): the dead
// mode.permissions shape is SWEPT (its allow keys recovered as
// writer-injected from the legacy file itself — configwriter.go
// loadExisting — and removed), and the operator's allowedDirs SOURCE
// re-renders them into the LIVE key on Apply. The artifact is
// platform-owned ephemeral state: "nothing user-visible is lost"
// because the source re-applies, NOT because artifact entries are
// migrated. Pinned: (a) the floor renders under the live key, (b) tier
// keys never linger in the dead shape, (c) a NON-TIER legacy allow is
// dropped from the dead shape and RE-RENDERED into the live key when —
// and only when — the operator source still supplies it.
func TestConfigWriter_MigratesLegacyModeShapeToLiveKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeTiersConfig(t, path, `{"mode": {"permissions": {"external_directory": {"/tmp/*": "allow", "/legacy-allow/*": "allow"}}}}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.Equal(t, "allow", ext["/tmp/*"], "the floor renders under the live top-level key")
	assert.NotContains(t, ext, "/legacy-allow/*",
		"a legacy allow is NOT artifact-migrated — it renders only when the operator source re-applies it")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
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
	assert.NotContains(t, out.Mode.Permissions.ExtDir, "/legacy-allow/*",
		"the recovered injected dir is swept from the dead shape")

	// The source re-apply: the same operator allow, supplied through
	// Apply, renders into the LIVE key — this is the nothing-lost path.
	w2 := NewConfigWriter(path)
	_, err = w2.Apply(agentapi.AgentConfigInput{AllowedDirs: &agentapi.AllowedDirsChange{Dirs: []string{"/legacy-allow/*"}}})
	require.NoError(t, err)
	ext2 := readRenderedExtDir(t, path)
	assert.Equal(t, "allow", ext2["/legacy-allow/*"],
		"the operator-supplied allow renders into the live key — the source is the migration path")
}

// Render-level floor dominance (r4): reopening operator allows are
// DROPPED from the rendered live key; legitimate ones survive. This is
// the pin that would have caught the subpath hole — the model-side pin
// alone is circular.
func TestConfigWriter_RenderDropsReopeningOperatorAllows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeTiersConfig(t, path, `{}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{AllowedDirs: &agentapi.AllowedDirsChange{Dirs: []string{
		"/etc/latency/*",            // deeper pattern inside a deny — must DROP
		"/home/sandbox/.ssh/id_rsa", // exact credential path — must DROP
		"/et*",                      // prefix glob reaching /etc — must DROP
		"/opt/cache/*",              // outside every deny — must SURVIVE
	}}})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.NotContains(t, ext, "/etc/latency/*", "a reopening allow must not render")
	assert.NotContains(t, ext, "/home/sandbox/.ssh/id_rsa", "a credential-path allow must not render")
	assert.NotContains(t, ext, "/et*", "a prefix-glob reaching a deny root must not render")
	assert.Equal(t, "allow", ext["/opt/cache/*"], "a legitimate operator allow survives")
	assert.Equal(t, "allow", ext["/tmp/*"], "the tier floor still renders")
}

// r5 finding 1 — the ?-wildcard rows: a single-char wildcard in the
// operator pattern must not slip the literal-prefix filter ("?etc/*"
// sorts AFTER every deny — 0x3F > 0x2F — and reopens /etc/* live).
func TestConfigWriter_RenderDropsQuestionMarkReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	writeTiersConfig(t, path, `{}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{AllowedDirs: &agentapi.AllowedDirsChange{Dirs: []string{
		"?etc/*", // leading single-char wildcard — must DROP
		"/?tc/*", // interior single-char wildcard — must DROP
	}}})
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.NotContains(t, ext, "?etc/*", "a ?-wildcard pattern reaching a deny root must not render")
	assert.NotContains(t, ext, "/?tc/*", "an interior ?-wildcard reaching a deny root must not render")
	assert.Equal(t, "deny", ext["/etc/*"], "the floor still renders")
}

// r5 finding 2 — the artifact ingress: a seeded (agent-tampered or
// stale) allow that reopens a tier deny must not re-render. The
// artifact is agent-writable by the threat model; the render is the
// last line of defense.
func TestConfigWriter_RenderDropsSeededReopeningAllows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-config.json")
	// Seeded live key: a reopening allow (the r5-demonstrated ingress).
	writeTiersConfig(t, path, `{"permission": {"external_directory": {"/etc/latency/*": "allow", "/custom/*": "allow"}}}`)

	w := NewConfigWriter(path)
	_, err := w.Apply(agentapi.AgentConfigInput{}) // NO AllowedDirs source — pure artifact ingress
	require.NoError(t, err)

	ext := readRenderedExtDir(t, path)
	assert.NotContains(t, ext, "/etc/latency/*",
		"a seeded allow that reopens a tier deny must not re-render (the artifact is agent-writable)")
	assert.Equal(t, "allow", ext["/custom/*"],
		"a seeded NON-reopening allow survives (render idempotency — a prior legit render re-renders)")
	assert.Equal(t, "deny", ext["/etc/*"], "the floor still renders")
}

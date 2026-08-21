// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// us4b_sidecar_relocations_test.go — design 0051 US-4b: the agentd-side
// consumers of the relocated stores.
//
//   - Path helpers default to the /sandbox-runtime consts (single-container
//     byte-identical) and honor the LLMSAFESPACES_*_PATH env overrides the
//     controller sets on the sidecar/init containers.
//   - The reload-path materializer arms CrossUID from
//     LLMSAFESPACES_CROSS_UID_FILES (rt/* files 0640 / dirs 0770 — the
//     uid-1000 tool-consumed stores, US-35.7 class C).
//   - parseSecretsEnvDelta must work with NO shell on PATH: the sidecar
//     image is FROM scratch (no bash) — the parser is pure Go, the exact
//     inverse of the materializer's FormatEnvLine (its single writer).
//   - The model-resolution-warning path derives from the agent-config
//     path's directory, keeping the healthz reader colocated with the
//     materializer writer under any relocation.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// --- path helpers -----------------------------------------------------------

func TestUS4B_BootPathHelpers_Defaults(t *testing.T) {
	require.Equal(t, agentd.AgentConfigPath, agentConfigPathFromEnv())
	require.Equal(t, agentd.AdminPromptPath, adminPromptPathFromEnv())
	require.Equal(t, agentd.AllowedDirsPath, allowedDirsPathFromEnv())
}

func TestUS4B_BootPathHelpers_EnvOverrides(t *testing.T) {
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", "/agentd-config/agent-config.json")
	t.Setenv("LLMSAFESPACES_ADMIN_PROMPT_PATH", "/agentd-secrets/admin-prompt.md")
	t.Setenv("LLMSAFESPACES_ALLOWED_DIRS_PATH", "/agentd-config/allowed-dirs.json")

	require.Equal(t, "/agentd-config/agent-config.json", agentConfigPathFromEnv())
	require.Equal(t, "/agentd-secrets/admin-prompt.md", adminPromptPathFromEnv())
	require.Equal(t, "/agentd-config/allowed-dirs.json", allowedDirsPathFromEnv())
}

// TestUS4B_BootPathHelpers_WiredIntoBootStamp: with the overrides set, the
// boot stamp must read the prompt/dirs from the RELOCATED paths — the
// regression this pins is a call site passing the consts directly.
func TestUS4B_BootPathHelpers_WiredIntoBootStamp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs.json")
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", cfgPath)
	t.Setenv("LLMSAFESPACES_ADMIN_PROMPT_PATH", promptPath)
	t.Setenv("LLMSAFESPACES_ALLOWED_DIRS_PATH", dirsPath)

	require.NoError(t, os.WriteFile(promptPath, []byte("RELOCATED PROMPT"), 0o640))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*"]`), 0o640))

	cfgOut, promptOut, dirsOut := bootAgentConfigPathsWithEnv()
	require.Equal(t, cfgPath, cfgOut)
	require.Equal(t, promptPath, promptOut)
	require.Equal(t, dirsPath, dirsOut)
	ensureBootAgentConfig(cfgOut, promptOut, dirsOut, "pw")

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err, "the stamp must write to the overridden config path")
	require.Contains(t, string(data), "RELOCATED PROMPT",
		"the stamp must source the admin prompt from the overridden path")
	require.Contains(t, string(data), "/tmp/*",
		"the stamp must source allowed-dirs from the overridden path")
}

// --- cross-uid flag ----------------------------------------------------------

func TestUS4B_LoadMaterializeConfig_CrossUIDFlag(t *testing.T) {
	require.False(t, loadMaterializeConfig().crossUID,
		"no env → cross-uid modes OFF (single-container byte-identical)")
	t.Setenv("LLMSAFESPACES_CROSS_UID_FILES", "1")
	require.True(t, loadMaterializeConfig().crossUID,
		"the controller sets LLMSAFESPACES_CROSS_UID_FILES=1 on the sidecar")
}

// --- shell-free secrets-env parsing ------------------------------------------

// TestUS4B_ParseSecretsEnvDelta_WorksWithoutShell: the sidecar image is
// FROM scratch — no bash exists. The parser must be pure Go.
func TestUS4B_ParseSecretsEnvDelta_WorksWithoutShell(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()

	// Build the file with the materializer's own encoder — the parser's
	// contract is "exact inverse of FormatEnvLine".
	lines := ""
	for _, kv := range []struct{ k, v string }{
		{"SIMPLE", "abc123"},
		{"SPACES", "hello world"},
		{"QUOTED", "it's quoted"},
		{"DOLLAR", "a$b`c"},
		{"MULTILINE", "line1\nline2"},
		{"EMPTY", ""},
	} {
		lines += secrets.FormatEnvLine(kv.k, kv.v)
	}
	p := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o600))

	delta, err := parseSecretsEnvDelta(p)
	require.NoError(t, err)
	require.Equal(t, "abc123", delta["SIMPLE"])
	require.Equal(t, "hello world", delta["SPACES"])
	require.Equal(t, "it's quoted", delta["QUOTED"])
	require.Equal(t, "a$b`c", delta["DOLLAR"])
	require.Equal(t, "line1\nline2", delta["MULTILINE"])
	require.Equal(t, "", delta["EMPTY"])
}

// TestUS4B_ParseSecretsEnvDelta_MalformedLineIsError: the file has exactly
// one machine writer; anything that does not round-trip is corruption and
// must surface (the callers degrade safely — logged, boot continues).
func TestUS4B_ParseSecretsEnvDelta_MalformedLineIsError(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(p, []byte("export GOOD='v'\nGARBAGE not a formatted line\n"), 0o600))

	_, err := parseSecretsEnvDelta(p)
	require.Error(t, err, "malformed lines must not be silently skipped")
}

// TestUS4B_ParseSecretsEnvDelta_UnknownFormatLines: unquoted values (the
// pre-shellquote legacy format) are NOT accepted — only the canonical
// single-quoted FormatEnvLine output parses.
func TestUS4B_ParseSecretsEnvDelta_UnknownFormatLines(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets-env")
	require.NoError(t, os.WriteFile(p, []byte("export BARE=unquoted\n"), 0o600))

	_, err := parseSecretsEnvDelta(p)
	require.Error(t, err)
}

// --- model-warning path derivation -------------------------------------------

func TestUS4B_ModelWarnPath_FollowsAgentConfigDir(t *testing.T) {
	require.Equal(t, agentd.ModelResolutionWarningPath, modelWarnPathFromEnv(),
		"default: colocated with the const-layout config")

	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", "/agentd-config/agent-config.json")
	require.Equal(t, "/agentd-config/model-resolution-warning.json", modelWarnPathFromEnv(),
		"relocated: colocated with the relocated config (the writer derives it from filepath.Dir)")
}

// TestUS4B_ReloadCacheMode_CrossUID: the boot cache is written by the
// init (uid 1000) and read by the sidecar's healthz user_creds_present
// (uid 2000) — 0640 under the sidecar's env, 0600 otherwise. The write
// is rename-atomic, so the sidecar's own rewrites cross the uid split
// via directory permissions, not file permissions.
func TestUS4B_ReloadCacheMode_CrossUID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "last-reload-secrets.json")

	require.NoError(t, writeReloadSecretsCache(p, nil))
	require.FileExists(t, p)
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "default: owner-only cache")

	t.Setenv("LLMSAFESPACES_CROSS_UID_FILES", "1")
	require.NoError(t, writeReloadSecretsCache(p, nil))
	info, err = os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"cross-uid: the sidecar's healthz reads the init-written boot cache")
}

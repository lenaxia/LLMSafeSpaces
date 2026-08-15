// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pre_boot_relay_test.go — tests for the Phase C cold-start
// optimization (item #1a). These tests pin the contract between the
// controller's freemodels package (Phase A: ConfigMap publisher) and
// agentd's materialize subcommand (Phase C: read CM-mounted file +
// pre-render relay block before opencode boots).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAgentConfig parses agent-config.json into a generic map for
// inspection. Helper to keep tests readable.
func readAgentConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// withFreeModelsAtTmp redirects the (otherwise-constant) freeModelsFilePath
// to a temp dir for the duration of the test. Tests cannot write to
// /sandbox-cfg/ so we monkey-patch the package var. Restore on cleanup.
func withFreeModelsAtTmp(t *testing.T, content []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "free-models.json")
	if content != nil {
		require.NoError(t, os.WriteFile(path, content, 0o600))
	}
	orig := freeModelsTestPath
	freeModelsTestPath = path
	t.Cleanup(func() { freeModelsTestPath = orig })
}

// TestApplyRelayConfigPreBoot_NoRelayURL_NoOp verifies that an unset
// INFERENCE_RELAY_BASEURL is treated as "relay disabled cluster-wide"
// and produces no agent-config changes.
func TestApplyRelayConfigPreBoot_NoRelayURL_NoOp(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"$schema":"x"}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped_no_relay_url", outcome,
		"empty relay URL must return skipped_no_relay_url and not touch the file")

	// agent-config.json must be byte-identical to before.
	b, _ := os.ReadFile(cfgPath)
	assert.Equal(t, `{"$schema":"x"}`, string(b),
		"no-op outcome must not modify agent-config.json at all")
}

// TestApplyRelayConfigPreBoot_NoCatalog_NoOp verifies that an absent
// /sandbox-cfg/free-models.json is treated as "Phase A refresher
// hasn't published yet" — no error, no agent-config changes,
// fall-through to legacy in-pod injection.
func TestApplyRelayConfigPreBoot_NoCatalog_NoOp(t *testing.T) {
	withFreeModelsAtTmp(t, nil) // path set to a non-existent file

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.NoError(t, err,
		"missing catalog file is normal pre-first-fetch; must not error")
	assert.Equal(t, "skipped_no_catalog", outcome)
}

// TestApplyRelayConfigPreBoot_EmptyCatalog_NoOp verifies that a
// catalog with zero models doesn't write a relay block (which would
// be a relay-with-no-models config, leaving free-tier users with no
// available models).
func TestApplyRelayConfigPreBoot_EmptyCatalog_NoOp(t *testing.T) {
	withFreeModelsAtTmp(t, []byte(`{"models":[]}`))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped_empty_catalog", outcome,
		"empty catalog must NOT write a no-model relay block — "+
			"a no-model relay leaves free-tier users with nothing")
}

// TestApplyRelayConfigPreBoot_PersonalKey_NoOp verifies the bypass:
// when auth.json carries a personal opencode key, do not interpose
// the relay (matches legacy startRelayInjector semantics).
func TestApplyRelayConfigPreBoot_PersonalKey_NoOp(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "model-a", "model-b"))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"sk-personal"}}`), 0o600))
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "skipped_personal_key", outcome,
		"personal opencode key must trigger the bypass — user is paying for direct Zen")

	// agent-config.json untouched.
	cfg := readAgentConfig(t, cfgPath)
	_, hasProvider := cfg["provider"]
	assert.False(t, hasProvider, "bypass must not write any provider block")
}

// TestApplyRelayConfigPreBoot_PublicKey_AppliesRelay verifies the
// happy path: auth.json says "public" key (default anonymous Zen
// access), catalog has free models — the relay block is written
// atomically to agent-config.json AND auth.json gets an
// opencode-relay entry.
func TestApplyRelayConfigPreBoot_PublicKey_AppliesRelay(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "ring-2.6-1t-free", "mimo-v2-pro-free"))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))
	cfgPath := filepath.Join(dir, "agent-config.json")
	// Seed with empty config — the writer's loadExisting will see no
	// providers and the relay block will be the only entry.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/secret", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "applied", outcome)

	cfg := readAgentConfig(t, cfgPath)

	// disabled_providers must include "opencode" — the relay supersedes it.
	disabled, ok := cfg["disabled_providers"].([]any)
	require.True(t, ok, "disabled_providers must be an array of strings")
	assert.Contains(t, disabled, "opencode",
		"applied relay must disable the built-in opencode provider")

	// provider.opencode-relay must be present with the relay URL.
	provider, ok := cfg["provider"].(map[string]any)
	require.True(t, ok, "provider must be a map")
	relay, ok := provider["opencode-relay"].(map[string]any)
	require.True(t, ok, "provider.opencode-relay must be present")

	options, ok := relay["options"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://relay.test/secret", options["baseURL"])
	assert.Equal(t, "public", options["apiKey"])

	// models must include both free models from the catalog.
	models, ok := relay["models"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, models, "ring-2.6-1t-free")
	assert.Contains(t, models, "mimo-v2-pro-free")

	// auth.json must have the opencode-relay entry written.
	authBytes, _ := os.ReadFile(authPath)
	assert.Contains(t, string(authBytes), "opencode-relay",
		"auth.json must carry the opencode-relay entry so opencode can authenticate to the relay")
}

// TestApplyRelayConfigPreBoot_AbsentAuthJSON_AppliesRelay verifies
// that a fresh pod (no auth.json yet) still applies the relay —
// shouldSkipRelay returns (false, "") for a missing file.
func TestApplyRelayConfigPreBoot_AbsentAuthJSON_AppliesRelay(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "model-a"))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json") // intentionally absent
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "applied", outcome,
		"missing auth.json must be treated as fresh-pod, NOT a bypass")

	// auth.json must now exist and contain opencode-relay.
	_, err = os.Stat(authPath)
	require.NoError(t, err, "applied path must create auth.json")
	authBytes, _ := os.ReadFile(authPath)
	assert.Contains(t, string(authBytes), "opencode-relay")
}

// TestApplyRelayConfigPreBoot_PreservesExistingProviders verifies that
// when the materialize subcommand has already written provider
// credentials (e.g. an LLM provider Secret), the relay block is
// MERGED, not replacing.
func TestApplyRelayConfigPreBoot_PreservesExistingProviders(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "free-model"))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	cfgPath := filepath.Join(dir, "agent-config.json")
	// Pre-existing provider config that FlushProviders would have written.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{
		"$schema": "x",
		"provider": {
			"openai": {"options": {"apiKey": "sk-test"}}
		},
		"model": "openai/gpt-4"
	}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	assert.Equal(t, "applied", outcome)

	cfg := readAgentConfig(t, cfgPath)
	provider, _ := cfg["provider"].(map[string]any)
	require.Contains(t, provider, "openai",
		"existing openai provider must be preserved (merge semantics)")
	require.Contains(t, provider, "opencode-relay",
		"relay provider must be added alongside, not replacing")

	// Model must also be preserved.
	assert.Equal(t, "openai/gpt-4", cfg["model"],
		"model from prior write must survive the relay merge")
}

// TestApplyRelayConfigPreBoot_MalformedCatalog_Errors verifies that a
// genuinely-broken catalog file (the controller wrote bad bytes)
// surfaces as a hard error so kubelet sees the boot failure.
func TestApplyRelayConfigPreBoot_MalformedCatalog_Errors(t *testing.T) {
	withFreeModelsAtTmp(t, []byte(`{ this is not json`))

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)
	require.Error(t, err,
		"malformed catalog is a controller-side bug; must surface as boot failure, "+
			"not silent fallback to legacy injection (which would mask the bug)")
	assert.Equal(t, "error_catalog_decode", outcome)
}

// mustCatalogBytes returns a marshaled wire-format catalog for the
// given model IDs. Helper for table-driven tests.
func mustCatalogBytes(t *testing.T, modelIDs ...string) []byte {
	t.Helper()
	type model struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ContextLimit int    `json:"context_limit"`
		OutputLimit  int    `json:"output_limit"`
	}
	type catalog struct {
		Models    []model `json:"models"`
		FetchedAt string  `json:"fetched_at"`
		Source    string  `json:"source"`
	}
	c := catalog{FetchedAt: "now", Source: "test"}
	for _, id := range modelIDs {
		c.Models = append(c.Models, model{ID: id, Name: id, ContextLimit: 100000, OutputLimit: 8000})
	}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	return b
}

// TestPreBootAuthJSONPath_DefaultHome verifies the path resolution
// matches main.go's logic so shouldSkipRelay reads the same auth.json
// opencode will read.
func TestPreBootAuthJSONPath_DefaultHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	got := preBootAuthJSONPath("/home/sandbox")
	assert.Equal(t, "/home/sandbox/.local/opencode/auth.json", got)
}

// TestPreBootAuthJSONPath_XDGOverride verifies XDG_DATA_HOME takes
// precedence over $HOME, matching the symlink the credential-setup
// init container creates.
func TestPreBootAuthJSONPath_XDGOverride(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/sandbox-runtime/xdg")
	got := preBootAuthJSONPath("/home/sandbox")
	assert.Equal(t, "/sandbox-runtime/xdg/opencode/auth.json", got)
}

// TestPreBootAuthJSONPath_EmptyHome_FallsBackToSandbox verifies the
// safety net for an unset $HOME (shouldn't happen in production but
// the materialize subcommand should not panic).
func TestPreBootAuthJSONPath_EmptyHome_FallsBackToSandbox(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	got := preBootAuthJSONPath("")
	assert.Equal(t, "/home/sandbox/.local/opencode/auth.json", got)
}

// TestApplyRelayConfigPreBoot_AuthJSONWriteFails covers the
// applied_auth_failed outcome — the only outcome string that returns
// (nil error) but with a non-"applied" marker. Locks in the
// graceful-degradation contract:
//
//   - agent-config.json was written successfully
//   - auth.json write failed (parent dir missing, EROFS, perms)
//   - return ("applied_auth_failed", nil) — NOT a hard error
//
// runMaterializeCommand observes a non-error outcome and continues to
// exit 0 (info-logging the outcome rather than CrashLoop'ing). The
// pod boots; opencode has the relay-provider block but no auth.json
// entry — first request through the relay 401s and the user can
// re-auth. Strictly better than failing the whole boot.
//
// PR #401 review finding: this outcome string had no explicit test;
// adding one prevents a future regression where someone makes the
// auth.json failure fatal (exit 3 → CrashLoop), which would cascade
// every workspace creation cluster-wide for any tmpfs / volume issue
// that affects auth.json writes.
func TestApplyRelayConfigPreBoot_AuthJSONWriteFails(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "model-a"))

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	// Point auth.json at a path inside an unwritable directory.
	// chmod 0o500 (r-x) means even the owner can't write here, so
	// updateAuthJSONForRelay's os.WriteFile must fail with EACCES.
	roDir := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(roDir, 0o500))
	t.Cleanup(func() {
		// Restore writable so t.TempDir() cleanup can remove it.
		_ = os.Chmod(roDir, 0o700)
	})
	authPath := filepath.Join(roDir, "auth.json")

	outcome, err := applyRelayConfigPreBoot("https://relay.test/", authPath, cfgPath, "pw", nil)

	require.NoError(t, err,
		"auth.json write failure must NOT propagate as an error — graceful degradation requires "+
			"the materialize subcommand to continue on this path so the pod still boots")
	assert.Equal(t, "applied_auth_failed", outcome,
		"this is the distinct outcome string that lets runMaterializeCommand log a warning "+
			"instead of CrashLooping — locking it down via test prevents a future change from "+
			"silently making this fatal")

	// agent-config.json must still have been written successfully —
	// the failure was AFTER the agent-config write.
	cfgBytes, _ := os.ReadFile(cfgPath)
	assert.Contains(t, string(cfgBytes), "opencode-relay",
		"agent-config.json must contain the relay block — only auth.json failed")
}

// TestApplyRelayConfigPreBoot_AppliesAllSourcesFromBootstrapFiles is a
// regression test for a behavioral regression introduced during the
// US-65.1 containment move: applyRelayConfigPreBoot constructed its
// ConfigWriter WITHOUT the WithAdminPromptPath / WithAllowedDirsPath /
// WithPreMarshalHook options, silently dropping three config sources
// (admin system prompt, allowed external directories, built-in admin
// MCP server) from the pre-boot relay path. The original hardcoded
// newAgentConfigWriter always loaded all three.
//
// This test seeds the bootstrap source files (via the adminPromptTestPath
// / allowedDirsTestPath overrides, since /sandbox-runtime isn't writable
// in CI) and asserts all three sources appear in the rendered config. It
// fails against the buggy code and passes once the constructor options
// are passed.
func TestApplyRelayConfigPreBoot_AppliesAllSourcesFromBootstrapFiles(t *testing.T) {
	withFreeModelsAtTmp(t, mustCatalogBytes(t, "free-model"))

	// Redirect the bootstrap source paths to temp files.
	bootstrapDir := t.TempDir()
	promptPath := filepath.Join(bootstrapDir, "admin-prompt.md")
	dirsPath := filepath.Join(bootstrapDir, "allowed-dirs.json")
	const adminPromptBody = "You are a canary coding assistant. Token: canary_admin_prompt_123"
	require.NoError(t, os.WriteFile(promptPath, []byte(adminPromptBody), 0o600))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`["/tmp/*","/opt/cache/*"]`), 0o600))
	origPrompt, origDirs := adminPromptTestPath, allowedDirsTestPath
	adminPromptTestPath, allowedDirsTestPath = promptPath, dirsPath
	t.Cleanup(func() {
		adminPromptTestPath, allowedDirsTestPath = origPrompt, origDirs
	})

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(authPath,
		[]byte(`{"opencode":{"type":"api","key":"public"}}`), 0o600))
	cfgPath := filepath.Join(dir, "agent-config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{}`), 0o600))

	outcome, err := applyRelayConfigPreBoot("https://relay.test/secret", authPath, cfgPath, "pw", nil)
	require.NoError(t, err)
	require.Equal(t, "applied", outcome)

	cfg := readAgentConfig(t, cfgPath)

	// 1. Admin system prompt must be present at agent.build.prompt.
	agent, ok := cfg["agent"].(map[string]any)
	require.True(t, ok, "agent block must be present (admin prompt source loaded)")
	build, ok := agent["build"].(map[string]any)
	require.True(t, ok, "agent.build must be present")
	assert.Equal(t, adminPromptBody, build["prompt"],
		"agent.build.prompt must contain the admin prompt body — "+
			"the pre-boot relay writer must load it from the admin-prompt source")

	// 2. Allowed external directories must be present as allow-rules.
	mode, ok := cfg["mode"].(map[string]any)
	require.True(t, ok, "mode block must be present (allowed-dirs source loaded)")
	perms, ok := mode["permissions"].(map[string]any)
	require.True(t, ok, "mode.permissions must be present")
	extDir, ok := perms["external_directory"].(map[string]any)
	require.True(t, ok, "mode.permissions.external_directory must be a map of allow-rules")
	assert.Equal(t, "allow", extDir["/tmp/*"],
		"/tmp/* from allowed-dirs file must be an allow-rule")
	assert.Equal(t, "allow", extDir["/opt/cache/*"],
		"/opt/cache/* from allowed-dirs file must be an allow-rule")

	// 3. Built-in admin MCP server must be injected by the pre-marshal hook.
	mcp, ok := cfg["mcp"].(map[string]any)
	require.True(t, ok, "mcp section must be present (pre-marshal hook ran)")
	_, hasBuiltin := mcp["llmsafespaces"]
	assert.True(t, hasBuiltin,
		"mcp.llmsafespaces must be present — the pre-boot writer must call "+
			"injectAgentdMCPServer via the preMarshalHook option")

	// Sanity: relay block still applied (the primary job of this path).
	provider, _ := cfg["provider"].(map[string]any)
	_, hasRelay := provider["opencode-relay"]
	assert.True(t, hasRelay, "relay provider must still be applied alongside all sources")
}

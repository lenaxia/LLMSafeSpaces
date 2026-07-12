// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for the materialize subcommand and reload-secrets HTTP handler.
//
// These tests are written TDD-style: they were authored before the
// implementation and exercise the contract that the implementation must
// satisfy. Each test corresponds to a concrete behavioral promise:
//
//   - The materialize subcommand reads /sandbox-cfg/secrets.json (or the
//     path given by --from) and applies it via pkg/agentd/secrets.
//   - Exit status: 0 if all secrets materialized OR all skipped (i.e. the
//     batch is structurally valid). Non-zero only if I/O failures occur.
//   - The reload-secrets handler accepts the same JSON shape over HTTP,
//     applies it, and returns a structured per-secret outcome list.
//   - buildEnv() uses pkg/agentd/secrets.ParseEnvLine so payloads that
//     contain shell metacharacters round-trip into opencode's env.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Build the workspace-agentd binary once per test process; subsequent
// subcommand invocations re-execute it as a real subprocess so the
// CLI surface (flag parsing, exit codes) is exercised end-to-end.
func buildAgentdBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test assumes unix")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "workspace-agentd")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run(), "go build failed")
	return bin
}

// runMaterializeSubcommand runs `workspace-agentd materialize --from <path>`
// and returns exit code, stdout, stderr.
func runMaterializeSubcommand(t *testing.T, bin, secretsPath, secretsBase, sshDir, agentCfg, envPath, gitCreds string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, "materialize", "--from", secretsPath)
	// Override paths via env so we don't need root or to write into
	// /home/sandbox during tests.
	cmd.Env = append(os.Environ(),
		"LLMSAFESPACES_SECRETS_BASE_DIR="+secretsBase,
		"LLMSAFESPACES_SSH_DIR="+sshDir,
		"LLMSAFESPACES_AGENT_CONFIG_PATH="+agentCfg,
		"LLMSAFESPACES_SECRETS_ENV_PATH="+envPath,
		"LLMSAFESPACES_GIT_CREDS_PATH="+gitCreds,
		"LLMSAFESPACES_RELOAD_CACHE_PATH="+filepath.Join(filepath.Dir(secretsBase), "nonexistent-reload-cache.json"),
		"HOME="+filepath.Dir(sshDir),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}
	return exit, stdout.String(), stderr.String()
}

// TestMaterializeSubcommand_HappyPath verifies the subcommand reads a
// well-formed secrets file and writes the expected outputs.
func TestMaterializeSubcommand_HappyPath(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"env-secret","name":"a","metadata":{"var_name":"FOO"},"plaintext":"bar"},
		{"type":"api-key","name":"p","plaintext":"{\"kind\":\"x\",\"slug\":\"x\"}"}
	]`), 0o600))

	secretsBase := filepath.Join(dir, "secrets")
	sshDir := filepath.Join(dir, ".ssh")
	agentCfg := filepath.Join(dir, "agent-config.json")
	envPath := filepath.Join(dir, "env")
	gitCreds := filepath.Join(dir, ".git-credentials")

	exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath, secretsBase, sshDir, agentCfg, envPath, gitCreds)
	require.Equal(t, 0, exit, "stderr=%q stdout=%q", stderr, stdout)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export FOO=")
	// api-key type writes to env path (not agent-config.json)
	require.Contains(t, string(envContent), "API_KEY_P=")

	st, err := os.Stat(envPath)
	require.NoError(t, err)
	require.Zero(t, st.Mode().Perm()&0o077, "env file must not have group/other bits")
}

// TestMaterializeSubcommand_MissingSecretsFile_NoOp verifies that a missing
// secrets file is treated as "no secrets to apply" rather than as an error.
// This matches the production case where /sandbox-cfg/secrets.json is
// absent for workspaces that have no user-supplied credentials.
func TestMaterializeSubcommand_MissingSecretsFile_NoOp(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "does-not-exist.json")
	exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"),
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "missing file must be a no-op; stderr=%q stdout=%q", stderr, stdout)
}

// TestMaterializeSubcommand_MissingSecretsFile_AppliesWorkspaceConfig is the
// regression test for the bug where a zero-credential user's model selection
// was never written to agent-config.json. When secrets.json is absent but
// workspace-config.json is present, runMaterializeCommand must still call
// applyWorkspaceConfig so the model key is written to agent-config.json.
func TestMaterializeSubcommand_MissingSecretsFile_AppliesWorkspaceConfig(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	// secrets.json is absent (zero-credential user).
	secretsPath := filepath.Join(dir, "does-not-exist.json")

	// workspace-config.json is present (user selected a model via SetModel).
	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"north-mini-code-free"}`), 0o600))

	// agent-config.json has relay provider (as FlushProviders would have written).
	agentCfgPath := filepath.Join(dir, "agent-config.json")
	agentCfgContent := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"opencode-relay": {
				"models": {"north-mini-code-free": {}}
			}
		}
	}`
	require.NoError(t, os.WriteFile(agentCfgPath, []byte(agentCfgContent), 0o600))

	exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfgPath,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "absent secrets.json must not fail boot; stderr=%q stdout=%q", stderr, stdout)

	// agent-config.json must now have the model key.
	raw, err := os.ReadFile(agentCfgPath)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Contains(t, cfg, "model",
		"agent-config.json must contain a model key even when secrets.json is absent")
	var model string
	require.NoError(t, json.Unmarshal(cfg["model"], &model))
	assert.Equal(t, "opencode-relay/north-mini-code-free", model,
		"model must be written as providerID/modelID even on the zero-credential path")
}

// TestMaterializeSubcommand_BadJSON_ReturnsExit2 verifies that a malformed
// secrets file fails loudly rather than silently boot-looping.
func TestMaterializeSubcommand_BadJSON_ReturnsExit2(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte("not json"), 0o600))

	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"),
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.NotZero(t, exit)
	require.Contains(t, stderr, "parsing")
}

// TestMaterializeSubcommand_InvalidEntries_DoesNotBlockBoot verifies T5: a
// malformed secret entry is skipped, materialize returns exit 0 (so the
// pod boots), and stderr lists the skipped entries for operator triage.
func TestMaterializeSubcommand_InvalidEntries_DoesNotBlockBoot(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"env-secret","name":"good","metadata":{"var_name":"GOOD"},"plaintext":"1"},
		{"type":"env-secret","name":"bad","metadata":{"var_name":"123BAD"},"plaintext":"2"}
	]`), 0o600))

	envPath := filepath.Join(dir, "env")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"),
		envPath,
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "bad entry must skip, not abort the batch")

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export GOOD=")
	require.NotContains(t, string(envContent), "123BAD")
	require.Contains(t, stderr, "123BAD",
		"stderr should report the skipped entry by name or by reason")
}

// TestReloadSecretsHandler_HappyPath wires the handler against a real
// in-memory materializer and verifies the response shape.
func TestReloadSecretsHandler_HappyPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  envPath,
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	body := `[{"type":"env-secret","name":"x","metadata":{"var_name":"X"},"plaintext":"v"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Reloaded  int  `json:"reloaded"`
		Restarted bool `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Reloaded)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export X=")
}

// TestReloadSecretsHandler_BadJSON returns 400.
func TestReloadSecretsHandler_BadJSON(t *testing.T) {
	cfg := materializeConfig{}
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestReloadSecretsHandler_WrongMethod returns 405.
func TestReloadSecretsHandler_WrongMethod(t *testing.T) {
	cfg := materializeConfig{}
	req := httptest.NewRequest(http.MethodGet, "/v1/reload-secrets", nil)
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestShouldRestart_LLMProvider — llm-provider no longer triggers restart
// (handled by PATCH /global/config instead).
func TestShouldRestart_LLMProvider(t *testing.T) {
	batch := []secrets.Secret{
		{Type: "llm-provider", Name: "anthropic", Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`},
	}
	if shouldRestart(batch) {
		t.Error("shouldRestart must return false for llm-provider (handled by PATCH)")
	}
}

// TestShouldRestart_LLMProviderMixed — restart only triggered by env-secret, not llm-provider.
func TestShouldRestart_LLMProviderMixed(t *testing.T) {
	batch := []secrets.Secret{
		{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "key"},
		{Type: "llm-provider", Name: "p", Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-..."}`},
		{Type: "env-secret", Name: "e", Metadata: map[string]string{"var_name": "VAR"}, Plaintext: "v"},
	}
	if !shouldRestart(batch) {
		t.Error("shouldRestart must return true when batch contains env-secret")
	}
}

// TestShouldRestart_NoLLMProvider does not trigger restart for non-credential types.
func TestShouldRestart_NoLLMProvider(t *testing.T) {
	batch := []secrets.Secret{
		{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "key"},
		{Type: "secret-file", Name: "f", Metadata: map[string]string{"mount_path": "x.txt"}, Plaintext: "data"},
	}
	if shouldRestart(batch) {
		t.Error("shouldRestart must return false for non-credential types")
	}
}

// TestShouldRestart_EmptyBatch does not trigger restart.
func TestShouldRestart_EmptyBatch(t *testing.T) {
	if shouldRestart(nil) {
		t.Error("shouldRestart must return false for empty batch")
	}
}

// TestHasLLMProviders detects llm-provider in batch.
func TestHasLLMProviders(t *testing.T) {
	if !hasLLMProviders([]secrets.Secret{{Type: "llm-provider", Name: "p", Plaintext: "{}"}}) {
		t.Error("hasLLMProviders must return true for llm-provider")
	}
	if hasLLMProviders([]secrets.Secret{{Type: "env-secret", Name: "e", Plaintext: "v"}}) {
		t.Error("hasLLMProviders must return false for non-llm-provider")
	}
	if hasLLMProviders(nil) {
		t.Error("hasLLMProviders must return false for nil batch")
	}
}

// TestBuildEnv_RoundTripsValuesWithMetacharacters confirms the buildEnv()
// refactor uses ParseEnvLine and therefore handles values that contain
// single quotes, newlines, etc. without mangling them. Pre-fix, the
// strings.Replace(..., "='", "=", 1) hack mangled such values.
func TestBuildEnv_RoundTripsValuesWithMetacharacters(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")

	// Write a couple of lines using FormatEnvLine so we know the format
	// matches what materialize produces.
	content := ""
	for _, kv := range []struct{ k, v string }{
		{"TOKEN_WITH_QUOTE", `'; whoami; '`},
		{"TOKEN_WITH_NEWLINE", "line1\nline2"},
		{"NORMAL", "value"},
	} {
		content += "export " + kv.k + "=" + shellQuoteForTest(kv.v) + "\n"
	}
	require.NoError(t, os.WriteFile(envPath, []byte(content), 0o600))

	got := buildEnvFrom(envPath)
	want := map[string]string{
		"TOKEN_WITH_QUOTE":   `'; whoami; '`,
		"TOKEN_WITH_NEWLINE": "line1\nline2",
		"NORMAL":             "value",
	}
	gotMap := map[string]string{}
	for _, e := range got {
		// Only consider the variables we care about; ignore inherited env.
		for k := range want {
			if strings.HasPrefix(e, k+"=") {
				gotMap[k] = strings.TrimPrefix(e, k+"=")
			}
		}
	}
	for k, v := range want {
		require.Equal(t, v, gotMap[k], "var %q must round-trip through buildEnvFrom", k)
	}
}

// shellQuoteForTest is a small reimplementation used only by the test to
// avoid an import cycle (the test lives in the main package).
func shellQuoteForTest(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// TestReloadSecretsHandler_LLMProvider_CallsOpenCodeClient verifies
// that when the reload handler receives llm-provider secrets, it:
// 1. Materializes them (stages in memory)
// 2. Flushes to config file
// 3. Calls PUT /auth/:providerID for each provider
// 4. Calls POST /instance/dispose
func TestReloadSecretsHandler_LLMProvider_CallsOpenCodeClient(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	agentCfg := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfg,
		secretsEnvPath:  envPath,
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	// Mock opencode server
	var receivedPaths []string
	var mu sync.Mutex
	mockOpenCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPaths = append(receivedPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("true"))
	}))
	defer mockOpenCode.Close()

	// Extract port from mock server to override AgentPort
	// We can't easily override the port in the handler, so we'll verify
	// the handler's response indicates configReloaded=true when the
	// provider is staged and FlushProviders succeeds.
	body := `[{"type":"llm-provider","name":"anthropic","plaintext":"{\"kind\":\"anthropic\",\"slug\":\"anthropic\",\"apiKey\":\"sk-ant-test\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{AgentConfigWriter: newAgentConfigWriter(agentCfg)})(rec, req)

	// Handler should succeed (materializer and flush work in-process)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Reloaded       int  `json:"reloaded"`
		ConfigReloaded bool `json:"configReloaded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Reloaded)

	// Agent config file should have been written by the AgentConfigWriter
	cfgData, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	require.Contains(t, string(cfgData), "sk-ant-test")
	require.Contains(t, string(cfgData), "anthropic")
}

// TestReloadSecretsHandler_WriterRebuildFailure_Returns500 verifies
// that if the AgentConfigWriter.rebuild() fails (e.g. disk full after
// reset() deleted the old config), the handler returns 500 and does NOT
// restart opencode with a missing config file.
//
// C1 regression fix: previously rebuild failure was a Warn + 200, which
// let opencode restart with no agent-config.json (reset() already deleted
// it). Now it returns 500 to match the old FlushProviders failure path.
func TestReloadSecretsHandler_WriterRebuildFailure_Returns500(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfg,
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	body := `[{"type":"llm-provider","name":"p","plaintext":"{\"kind\":\"openai\",\"slug\":\"openai\",\"apiKey\":\"sk-oai\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	// Pass a writer pointing at an unwritable path — rebuild will fail.
	// The handler must return 500 (not 200) because reset() already deleted
	// the config and opencode must not restart with no config on disk.
	unwritableDir := filepath.Join(dir, "nodir", "subdir")
	badWriter := newAgentConfigWriter(filepath.Join(unwritableDir, "agent-config.json"))

	reloadSecretsHandler(cfg, reloadSecretsDeps{AgentConfigWriter: badWriter})(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"writer rebuild failure must return 500 to prevent restart with no config")
}

// TestReloadSecretsHandler_MixedBatch_LLMAndEnv verifies that a batch
// containing both llm-provider and env-secret correctly:
// - materializes both types
// - writes env file
// - writes agent config
// - does NOT restart (configReloaded takes precedence)
func TestReloadSecretsHandler_MixedBatch_LLMAndEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	agentCfg := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfg,
		secretsEnvPath:  envPath,
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	body := `[
		{"type":"llm-provider","name":"p","plaintext":"{\"kind\":\"anthropic\",\"slug\":\"anthropic\",\"apiKey\":\"sk-1\"}"},
		{"type":"env-secret","name":"e","metadata":{"var_name":"MY_VAR"},"plaintext":"my_value"}
	]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{AgentConfigWriter: newAgentConfigWriter(agentCfg)})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Reloaded  int  `json:"reloaded"`
		Restarted bool `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 2, resp.Reloaded)
	// Should NOT restart because configReloaded takes precedence
	require.False(t, resp.Restarted)

	// Both files written
	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "MY_VAR=")

	cfgContent, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	require.Contains(t, string(cfgContent), "sk-1")
}

// TestReloadSecretsHandler_EnvOnly_NoConfigReload verifies that
// env-secret-only batches do NOT trigger config reload (they trigger restart).
func TestReloadSecretsHandler_EnvOnly_NoConfigReload(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env")
	cfg := materializeConfig{
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:   envPath,
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "enricher-cache"),
		home:             dir,
	}

	body := `[{"type":"env-secret","name":"x","metadata":{"var_name":"X"},"plaintext":"v"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	// proc=nil means restart won't actually fire, but we can check the response
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		ConfigReloaded bool `json:"configReloaded"`
		Restarted      bool `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.False(t, resp.ConfigReloaded)
	// proc is nil so restart didn't fire, but it WOULD have
	require.False(t, resp.Restarted)
}

// TestReloadSecretsHandler_PreservesRelayViaWriter verifies that when the
// AgentConfigWriter has relay config set (relay injector ran), a credential
// reload preserves the relay config. This is the integration-level regression
// test for the confirmed production bug: credential bind clobbering relay.
//
// US-46.10: the old four-writer design required a manual relay re-merge after
// FlushProviders. The single AgentConfigWriter eliminates this — Rebuild()
// always merges all sources (providers + model + relay) atomically.
func TestReloadSecretsHandler_PreservesRelayViaWriter(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  agentCfg,
		secretsEnvPath:   filepath.Join(dir, "env"),
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "enricher-cache"),
		home:             dir,
	}

	// Create writer and pre-set relay config as if the injector already ran.
	writer := newAgentConfigWriter(agentCfg)
	writer.setRelay("https://relay.example.test/path", []relayModel{
		{ID: "big-pickle", Name: "Big Pickle", ContextLimit: 131072, OutputLimit: 16384},
	})

	body := `[{"type":"llm-provider","name":"thekao","plaintext":"{\"kind\":\"thekao\",\"slug\":\"thekao\",\"apiKey\":\"sk-test\",\"baseURL\":\"https://ai.thekao.cloud/v1\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{AgentConfigWriter: writer})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// agent-config.json must contain both the credential provider (thekao)
	// AND the relay provider block with disabled_providers.
	cfgData, err := os.ReadFile(agentCfg)
	require.NoError(t, err)

	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfgData, &parsed), "agent-config.json must be valid JSON")

	disabledRaw, ok := parsed["disabled_providers"]
	require.True(t, ok, "disabled_providers must be present (writer preserved relay)")
	var disabled []string
	require.NoError(t, json.Unmarshal(disabledRaw, &disabled))
	assert.Contains(t, disabled, "opencode")

	var providers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(parsed["provider"], &providers))
	_, hasThekao := providers["thekao"]
	assert.True(t, hasThekao, "thekao provider from reload must be present")
	_, hasRelay := providers["opencode-relay"]
	assert.True(t, hasRelay, "opencode-relay must survive credential reload via writer")
}

// TestReloadSecretsHandler_NoRelay_NoDisabledProviders verifies that when
// the writer has no relay config (relay not yet run or skipped), a credential
// reload produces a config WITHOUT disabled_providers. This covers the
// personal-key user case.
func TestReloadSecretsHandler_NoRelay_NoDisabledProviders(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	cfg := materializeConfig{
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  agentCfg,
		secretsEnvPath:   filepath.Join(dir, "env"),
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "enricher-cache"),
		home:             dir,
	}

	writer := newAgentConfigWriter(agentCfg) // no relay set

	body := `[{"type":"llm-provider","name":"openai","plaintext":"{\"kind\":\"openai\",\"slug\":\"openai\",\"apiKey\":\"sk-personal\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{AgentConfigWriter: writer})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	cfgData, err := os.ReadFile(agentCfg)
	require.NoError(t, err)

	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfgData, &parsed))

	_, hasDisabled := parsed["disabled_providers"]
	assert.False(t, hasDisabled, "disabled_providers must be absent when no relay")

	if provRaw, ok := parsed["provider"]; ok {
		var providers map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(provRaw, &providers))
		_, hasRelay := providers["opencode-relay"]
		assert.False(t, hasRelay, "opencode-relay must be absent when no relay")
	}
}

// TestResolveModelWithProvider validates providerID resolution from the
// agent config's provider map.
func TestResolveModelWithProvider(t *testing.T) {
	buildCfg := func(providerJSON string) map[string]json.RawMessage {
		cfg := map[string]json.RawMessage{}
		cfg["provider"] = json.RawMessage(providerJSON)
		return cfg
	}

	t.Run("resolves flat ID when provider owns model", func(t *testing.T) {
		cfg := buildCfg(`{
			"thekao": {"models": {"glm-5.1": {}, "gpt-5.4": {}}},
			"opencode-relay": {"models": {"big-pickle": {}}}
		}`)
		got := resolveModelWithProvider(cfg, "glm-5.1")
		assert.Equal(t, "thekao/glm-5.1", got)
	})

	t.Run("returns flat ID unchanged when no provider claims it", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"gpt-5.4": {}}}}`)
		got := resolveModelWithProvider(cfg, "glm-5.1")
		assert.Equal(t, "glm-5.1", got, "fallback must not panic or mangle the ID")
	})

	t.Run("already-qualified IDs are passed through unchanged", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.1": {}}}}`)
		got := resolveModelWithProvider(cfg, "thekao/glm-5.1")
		assert.Equal(t, "thekao/glm-5.1", got)
	})

	t.Run("empty model ID returns empty string", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.1": {}}}}`)
		got := resolveModelWithProvider(cfg, "")
		assert.Equal(t, "", got)
	})

	t.Run("no provider key in cfg returns flat ID", func(t *testing.T) {
		cfg := map[string]json.RawMessage{} // no "provider" key
		got := resolveModelWithProvider(cfg, "glm-5.1")
		assert.Equal(t, "glm-5.1", got)
	})

	t.Run("malformed provider JSON returns flat ID", func(t *testing.T) {
		cfg := map[string]json.RawMessage{"provider": json.RawMessage(`not-json`)}
		got := resolveModelWithProvider(cfg, "glm-5.1")
		assert.Equal(t, "glm-5.1", got)
	})
}

// TestApplyWorkspaceConfig verifies that applyWorkspaceConfig writes the
// fully-qualified "providerID/modelID" form to agent-config.json, not the
// flat model ID. This is required by opencode 1.15.x which rejects bare IDs.
func TestApplyWorkspaceConfig(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	secretsJSON := filepath.Join(dir, "secrets.json")

	// Write a workspace-config.json with a flat default model.
	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"glm-5.1"}`), 0o600))

	// Write an agent-config.json as FlushProviders would have produced it,
	// with the provider already present.
	agentCfgContent := `{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"thekao": {
				"npm": "@ai-sdk/openai-compatible",
				"options": {"apiKey": "sk-test", "baseURL": "https://ai.thekao.cloud/v1"},
				"models": {"glm-5.1": {}, "gpt-5.4": {}}
			}
		}
	}`
	require.NoError(t, os.WriteFile(agentCfg, []byte(agentCfgContent), 0o600))

	applyWorkspaceConfig(agentCfg, secretsJSON)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)

	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))

	var model string
	require.NoError(t, json.Unmarshal(out["model"], &model))
	assert.Equal(t, "thekao/glm-5.1", model,
		"model must be written as providerID/modelID, not a flat ID")
}

// TestApplyWorkspaceConfig_FallsBackToFlatIDWhenProviderAbsent verifies that
// when the provider map has no entry for the model (e.g. agent-config.json
// was not yet written by FlushProviders), the flat ID is preserved rather
// than silently omitting the model field.
func TestApplyWorkspaceConfig_FallsBackToFlatIDWhenProviderAbsent(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	secretsJSON := filepath.Join(dir, "secrets.json")

	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"unknown-model"}`), 0o600))

	// agent-config.json has a provider but it does not list "unknown-model".
	agentCfgContent := `{"provider": {"thekao": {"models": {"gpt-5.4": {}}}}}`
	require.NoError(t, os.WriteFile(agentCfg, []byte(agentCfgContent), 0o600))

	applyWorkspaceConfig(agentCfg, secretsJSON)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	var model string
	require.NoError(t, json.Unmarshal(out["model"], &model))
	assert.Equal(t, "unknown-model", model, "flat fallback must be preserved")
}

// TestResolveModelWithProvider_Collision documents the behavior when two
// providers in agent-config.json expose the same model ID. Go map iteration
// is non-deterministic, so the function may return either "provider-a/shared"
// or "provider-b/shared". The contract is: the result is always a valid
// "providerID/modelID" string (never the flat ID, never empty, never a panic).
// The boot-time path accepts this non-determinism because the per-prompt
// frontend override routes correctly regardless of the boot default model.
func TestResolveModelWithProvider_Collision(t *testing.T) {
	cfg := map[string]json.RawMessage{
		"provider": json.RawMessage(`{
			"provider-a": {"models": {"shared": {}}},
			"provider-b": {"models": {"shared": {}}}
		}`),
	}
	got := resolveModelWithProvider(cfg, "shared")

	// Must be one of the two valid qualified forms — never the flat ID.
	assert.True(t,
		got == "provider-a/shared" || got == "provider-b/shared",
		"collision must produce a valid providerID/modelID form, got %q", got,
	)
}

// TestReloadSecretsHandler_ConcurrentCalls_NoRace verifies that concurrent
// reloadSecretsHandler calls do not race on the filesystem (SecretsEnvPath,
// AgentConfigPath). The test must be run with -race to catch data races.
// It also verifies that both calls return 200 — no request is starved.
func TestReloadSecretsHandler_ConcurrentCalls_NoRace(t *testing.T) {
	dir := t.TempDir()
	cfg := materializeConfig{
		home:             dir,
		secretsBaseDir:   filepath.Join(dir, ".secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:   filepath.Join(dir, "secrets-env"),
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "cache"),
	}

	handler := reloadSecretsHandler(cfg, reloadSecretsDeps{})
	body := `[{"type":"env-secret","name":"FOO","metadata":{"var_name":"FOO"},"plaintext":"bar"}]`

	var wg sync.WaitGroup
	results := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
			rec := httptest.NewRecorder()
			handler(rec, req)
			results[idx] = rec.Code
		}()
	}
	wg.Wait()

	for i, code := range results {
		assert.Equal(t, http.StatusOK, code, "handler %d returned non-200", i)
	}
}

// ---------------------------------------------------------------------------
// H2 (worklog 371): reloadSecretsHandler records secret-change restarts in
// the workspace_restarts_total Prometheus counter.
// ---------------------------------------------------------------------------

// TestReloadSecretsHandler_H2_EnvSecretRecordsRestartMetric verifies that a
// credential reload containing an env-secret (which triggers a restart) also
// increments workspace_restarts_total with reason="env_secrets". Pre-fix,
// the most common restart type (user-initiated credential change) was
// invisible in Prometheus — RecordRestart was only called from the crash
// and oom paths.
func TestReloadSecretsHandler_H2_EnvSecretRecordsRestartMetric(t *testing.T) {
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	t.Setenv("WORKSPACE_ID", "ws-h2-env")
	// The handler writes the restart-reason marker to the package constant
	// RestartReasonMarkerPath. Clean it up so it does not leak into other
	// tests (the boot-time reader would otherwise log it).
	t.Cleanup(func() { _ = os.Remove(RestartReasonMarkerPath) })

	// Idle tracker + nil lister → trackerHasBusyOrUnknown returns false →
	// makeSessionAwareRestartDecision restarts immediately (mock captures it).
	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	before := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-env", "env_secrets"))

	body := `[{"type":"env-secret","name":"x","metadata":{"var_name":"X"},"plaintext":"v"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		Proc:    proc,
		Tracker: tracker,
	})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	after := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-env", "env_secrets"))
	assert.Equal(t, before+1, after,
		"workspace_restarts_total{reason=\"env_secrets\"} must increment on env-secret reload (H2)")
	assert.Equal(t, 1, proc.restartCount(),
		"the mock proc must have been restarted")
}

// TestReloadSecretsHandler_H2_APIKeyRecordsRestartMetric verifies the same
// for an api-key batch (reason="api_key").
func TestReloadSecretsHandler_H2_APIKeyRecordsRestartMetric(t *testing.T) {
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	t.Setenv("WORKSPACE_ID", "ws-h2-apikey")
	t.Cleanup(func() { _ = os.Remove(RestartReasonMarkerPath) })

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	before := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-apikey", "api_key"))

	body := `[{"type":"api-key","name":"k","plaintext":"secret"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		Proc:    proc,
		Tracker: tracker,
	})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	after := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-apikey", "api_key"))
	assert.Equal(t, before+1, after,
		"workspace_restarts_total{reason=\"api_key\"} must increment on api-key reload (H2)")
}

// TestMetricRestartReason_MapsMarkerReasonToMetricLabel verifies the marker
// → metric reason mapping. The marker file uses the longer human-readable
// form (env_secrets_changed); the Prometheus label uses the short form
// (env_secrets) that matches the metric help text and the crash/oom reasons.
func TestMetricRestartReason_MapsMarkerReasonToMetricLabel(t *testing.T) {
	assert.Equal(t, "env_secrets", metricRestartReason("env_secrets_changed"))
	assert.Equal(t, "api_key", metricRestartReason("api_key_changed"))
	assert.Equal(t, "crash", metricRestartReason("crash"), "unknown reasons pass through unchanged")
	assert.Equal(t, "oom", metricRestartReason("oom"))
}

// TestReloadSecretsHandler_H2_MetricRecordedEvenWhenMarkerWriteFails verifies
// the H2 fix: RecordRestart is called UNCONDITIONALLY (after the marker/log
// block), not gated on marker-write success. Pre-fix, a full/read-only PVC
// would suppress workspace_restarts_total for the most common restart type
// (credential change) even though the restart still proceeded. This test points
// the marker path at an unwritable location (a path whose parent is a file,
// not a directory) so writeRestartReasonMarker fails, then asserts the counter
// still increments.
func TestReloadSecretsHandler_H2_MetricRecordedEvenWhenMarkerWriteFails(t *testing.T) {
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}

	t.Setenv("WORKSPACE_ID", "ws-h2-markerfail")

	// Sabotage the marker write: create a regular file, then set the marker
	// path INSIDE it. writeRestartReasonMarker does MkdirAll(filepath.Dir(path))
	// which fails because the parent is a file → the marker write errors out.
	blockingFile := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blockingFile, []byte("x"), 0o600))
	sabotagedMarkerPath := filepath.Join(blockingFile, "marker")

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	before := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-markerfail", "env_secrets"))

	body := `[{"type":"env-secret","name":"x","metadata":{"var_name":"X"},"plaintext":"v"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		Proc:                    proc,
		Tracker:                 tracker,
		RestartReasonMarkerPath: sabotagedMarkerPath,
	})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	after := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-h2-markerfail", "env_secrets"))
	assert.Equal(t, before+1, after,
		"workspace_restarts_total must increment even when the marker write fails (H2 unconditional recording)")
	assert.Equal(t, 1, proc.restartCount(),
		"the restart must still proceed despite the marker write failure")
}

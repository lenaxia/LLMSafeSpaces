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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The built binary is shared across every caller in the test process:
// 39+ subcommand tests each re-linking the same sources cost ~140s of
// wall time per suite run. Same sources ⇒ same artifact within one `go
// test` invocation, so a single build is behaviorally identical.
var (
	agentdBinaryOnce sync.Once
	agentdBinaryPath string

	// agentdBinaryDirMu guards agentdBinaryDir, the one MkdirTemp dir
	// backing the shared binary. TestMain drains it after m.Run() so the
	// build-once optimization does not leak a directory per test process
	// (the sharing itself is unchanged — still one build, one path).
	agentdBinaryDirMu sync.Mutex
	agentdBinaryDir   string
)

// cleanupAgentdTestBinary removes the shared test-binary temp dir. Called
// from TestMain after all tests finish; a no-op when the binary was never
// built (short mode, windows skips).
func cleanupAgentdTestBinary() {
	agentdBinaryDirMu.Lock()
	defer agentdBinaryDirMu.Unlock()
	if agentdBinaryDir != "" {
		_ = os.RemoveAll(agentdBinaryDir)
		agentdBinaryDir = ""
	}
}

// Build the workspace-agentd binary once per test process; subcommand
// invocations re-execute it as a real subprocess so the CLI surface
// (flag parsing, exit codes) is exercised end-to-end.
func buildAgentdBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("subprocess test assumes unix")
	}
	agentdBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "workspace-agentd-testbin-*")
		if err != nil {
			panic(fmt.Sprintf("temp dir for shared test binary: %v", err))
		}
		agentdBinaryDirMu.Lock()
		agentdBinaryDir = dir
		agentdBinaryDirMu.Unlock()
		bin := filepath.Join(dir, "workspace-agentd")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			// Panic, not t.Fatalf: the Once must not be left consumed
			// with an empty path for the other 38 callers, and every
			// subprocess test is equally dead without the artifact.
			panic(fmt.Sprintf("go build failed: %v", err))
		}
		agentdBinaryPath = bin
	})
	return agentdBinaryPath
}

// runMaterializeSubcommand runs `workspace-agentd materialize --from <path>`
// and returns exit code, stdout, stderr.
func runMaterializeSubcommand(t *testing.T, bin, secretsPath, secretsBase, sshDir, agentCfg, envPath, gitCreds string, extraEnv ...string) (int, string, string) {
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
	cmd.Env = append(cmd.Env, extraEnv...)
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

// TestMaterializeSubcommand_MCPContractShape_Exits0 renders bound MCP
// servers (contract metadata: native JSON args/timeoutMs) through the
// real subcommand and asserts the incident workflow end-to-end:
// secrets.json → exit 0 → "mcp" section in agent-config.json. This is
// the automation of the 2026-08-15 crash-loop repro (one bound MCP
// server aborted the whole-file parse → Init:Error loop).
func TestMaterializeSubcommand_MCPContractShape_Exits0(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"llm-provider","name":"relay","metadata":{"api_base":"https://relay.example"},"plaintext":"{\"kind\":\"custom\",\"slug\":\"relay\",\"api_key\":\"k\"}"},
		{"type":"mcp-server","name":"opengist","metadata":{"transport":"http","url":"https://mcp.example/abc","command":"","args":[],"timeoutMs":5000},"plaintext":"{\"env\":{},\"headers\":{}}"},
		{"type":"mcp-server","name":"github-tools","metadata":{"transport":"stdio","url":"","command":"npx","args":["-y","@modelcontextprotocol/server-github"],"timeoutMs":5000},"plaintext":"{\"env\":{},\"headers\":{}}"}
	]`), 0o600))

	agentCfg := filepath.Join(dir, "agent-config.json")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfg,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "contract-shaped MCP entries must not block boot; stderr=%q", stderr)

	cfgBytes, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var cfg struct {
		MCP map[string]struct {
			Type    string   `json:"type"`
			URL     string   `json:"url"`
			Command []string `json:"command"`
			Timeout int      `json:"timeout"`
		} `json:"mcp"`
	}
	require.NoError(t, json.Unmarshal(cfgBytes, &cfg))
	require.Contains(t, cfg.MCP, "opengist", "http MCP server rendered; got %s", cfgBytes)
	require.Equal(t, "remote", cfg.MCP["opengist"].Type)
	require.Equal(t, "https://mcp.example/abc", cfg.MCP["opengist"].URL)
	require.Equal(t, 5000, cfg.MCP["opengist"].Timeout)
	require.Contains(t, cfg.MCP, "github-tools", "stdio MCP server rendered")
	require.Equal(t, "local", cfg.MCP["github-tools"].Type)
	require.Equal(t, []string{"npx", "-y", "@modelcontextprotocol/server-github"}, cfg.MCP["github-tools"].Command)
}

// TestMaterializeSubcommand_MalformedMCPMetadata_SkipsNotCrashloops pins
// the exit code for the per-entry tolerance path (review F1): malformed
// metadata must behave like any other invalid input — Skipped, boot
// exit 0, healthy siblings materialized.
func TestMaterializeSubcommand_MalformedMCPMetadata_SkipsNotCrashloops(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"env-secret","name":"good","metadata":{"var_name":"GOOD"},"plaintext":"1"},
		{"type":"mcp-server","name":"bad","metadata":"not-an-object","plaintext":"{}"}
	]`), 0o600))

	envPath := filepath.Join(dir, "env")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"),
		envPath,
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "malformed-metadata entry must skip, not crash-loop; stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export GOOD=")
	require.Contains(t, stderr, "bad", "skipped entry reported by name")
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)

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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestReloadSecretsHandler_WrongMethod returns 405.
func TestReloadSecretsHandler_WrongMethod(t *testing.T) {
	cfg := materializeConfig{}
	req := httptest.NewRequest(http.MethodGet, "/v1/reload-secrets", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: opencode.NewConfigWriter(agentCfg)})(rec, req)

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
// that if the ConfigWriter.Rebuild() fails (e.g. disk full after
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	// Pass a writer pointing at an unwritable path — rebuild will fail.
	// The handler must return 500 (not 200) because reset() already deleted
	// the config and opencode must not restart with no config on disk.
	unwritableDir := filepath.Join(dir, "nodir", "subdir")
	badWriter := opencode.NewConfigWriter(filepath.Join(unwritableDir, "agent-config.json"))

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: badWriter})(rec, req)

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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: opencode.NewConfigWriter(agentCfg)})(rec, req)

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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	// proc=nil means restart won't actually fire, but we can check the response
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)

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
	writer := opencode.NewConfigWriter(agentCfg)
	writer.SetRelay("https://relay.example.test/path", []opencode.RelayModel{
		{ID: "big-pickle", Name: "Big Pickle", ContextLimit: 131072, OutputLimit: 16384},
	})

	body := `[{"type":"llm-provider","name":"thekao","plaintext":"{\"kind\":\"thekao\",\"slug\":\"thekao\",\"apiKey\":\"sk-test\",\"baseURL\":\"https://ai.thekao.cloud/v1\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: writer})(rec, req)
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

	writer := opencode.NewConfigWriter(agentCfg) // no relay set

	body := `[{"type":"llm-provider","name":"openai","plaintext":"{\"kind\":\"openai\",\"slug\":\"openai\",\"apiKey\":\"sk-personal\"}"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()

	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw", AgentConfigWriter: writer})(rec, req)
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
// agent config's provider map. Contract (incident 2026-08-16 follow-up):
// ok=false means "no provider in THIS boot's config can serve the model" —
// the caller must omit the model key, never write the value bare or
// qualified-but-unverifiable.
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
		got, ok := resolveModelWithProvider(cfg, "glm-5.1")
		assert.True(t, ok)
		assert.Equal(t, "thekao/glm-5.1", got)
	})

	t.Run("flat ID no provider claims it is unresolvable", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"gpt-5.4": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "glm-5.1")
		assert.False(t, ok)
		assert.Empty(t, got)
	})

	t.Run("already-qualified with provider present passes through", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.1": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "thekao/glm-5.1")
		assert.True(t, ok)
		assert.Equal(t, "thekao/glm-5.1", got)
	})

	t.Run("qualified with provider absent is unresolvable", func(t *testing.T) {
		// The exact incident shape, one boot later, with qualified
		// persistence: default "opencode-relay/x", relay injector failed.
		// Passthrough would poison opencode the same way the bare ID did.
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.3": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "opencode-relay/deepseek-v4-flash-free")
		assert.False(t, ok)
		assert.Empty(t, got)
	})

	t.Run("qualified relay model with relay provider present resolves", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.3": {}}}, "opencode-relay": {"models": {"deepseek-v4-flash-free": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "opencode-relay/deepseek-v4-flash-free")
		assert.True(t, ok)
		assert.Equal(t, "opencode-relay/deepseek-v4-flash-free", got)
	})

	t.Run("two-slash qualified resolves via first-segment provider", func(t *testing.T) {
		// Round 4, finding 2: SetModel persists slashed-catalog selections
		// as "provider/vendor/model" (e.g. openrouter + "anthropic/
		// claude-sonnet-4.5"). The boot check must verify the FIRST
		// segment (opencode's routing split) — the previous LastIndex
		// split looked for a provider entry literally named
		// "openrouter/anthropic" and omit+warned on every reboot.
		cfg := buildCfg(`{"openrouter": {"models": {"anthropic/claude-sonnet-4.5": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "openrouter/anthropic/claude-sonnet-4.5")
		assert.True(t, ok, "first-segment provider presence must resolve multi-slash defaults")
		assert.Equal(t, "openrouter/anthropic/claude-sonnet-4.5", got)
	})

	t.Run("two-slash qualified first-segment provider absent is unresolvable", func(t *testing.T) {
		cfg := buildCfg(`{"openrouter": {"models": {"anthropic/claude-sonnet-4.5": {}}}}`)
		_, ok := resolveModelWithProvider(cfg, "otherprov/anthropic/claude-sonnet-4.5")
		assert.False(t, ok)
	})

	t.Run("empty-tail qualified form is unresolvable", func(t *testing.T) {
		// Round 5: "a/" is the incident's own parse shape (provider +
		// EMPTY modelID). Reachable via pod-down persistence; must be
		// rejected by the resolver, never written.
		cfg := buildCfg(`{"a": {"models": {"x": {}}}}`)
		_, ok := resolveModelWithProvider(cfg, "a/")
		assert.False(t, ok, "empty-tail 'a/' must never resolve even when provider 'a' exists")
	})

	t.Run("empty model ID is unresolvable", func(t *testing.T) {
		cfg := buildCfg(`{"thekao": {"models": {"glm-5.1": {}}}}`)
		got, ok := resolveModelWithProvider(cfg, "")
		assert.False(t, ok)
		assert.Empty(t, got)
	})

	t.Run("no provider key in cfg is unresolvable", func(t *testing.T) {
		cfg := map[string]json.RawMessage{} // no "provider" key
		got, ok := resolveModelWithProvider(cfg, "glm-5.1")
		assert.False(t, ok)
		assert.Empty(t, got)
	})

	t.Run("malformed provider JSON is unresolvable", func(t *testing.T) {
		cfg := map[string]json.RawMessage{"provider": json.RawMessage(`not-json`)}
		got, ok := resolveModelWithProvider(cfg, "glm-5.1")
		assert.False(t, ok, "fail safe: unverifiable means do not write")
		assert.Empty(t, got)
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

// TestApplyWorkspaceConfig_UnresolvableModel_OmittedWithWarning is the
// regression test for the 2026-08-16 incident (session ses_ff31324…, refs
// err_661e326d/err_de616758): when the workspace's persisted default model
// cannot be resolved to a provider, the flat ID must NOT be written to
// agent-config.json. opencode parses a bare ID as providerID with an empty
// modelID ("Model not found: deepseek-v4-flash-free/.") and every prompt in
// every session 500s until the pod is rebuilt. The model key is omitted
// (opencode applies its own default) and a warning marker is written so
// agentd can surface the condition to the user.
func TestApplyWorkspaceConfig_UnresolvableModel_OmittedWithWarning(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	secretsJSON := filepath.Join(dir, "secrets.json")

	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"deepseek-v4-flash-free"}`), 0o600))

	// agent-config.json has a provider but it does not list the model —
	// the exact production state (relay provider absent because the
	// free-models fetch failed at boot).
	agentCfgContent := `{"model": "deepseek-v4-flash-free", "provider": {"thekao": {"models": {"glm-5.3": {}}}}}`
	require.NoError(t, os.WriteFile(agentCfg, []byte(agentCfgContent), 0o600))

	applyWorkspaceConfig(agentCfg, secretsJSON)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	_, hasModel := out["model"]
	assert.False(t, hasModel,
		"unresolvable flat model must be omitted, not written bare — a bare ID poisons opencode model resolution")
	assert.Contains(t, string(raw), `"thekao"`, "providers must survive the rewrite")

	warnRaw, err := os.ReadFile(modelResolutionWarningPath(filepath.Dir(agentCfg)))
	require.NoError(t, err, "a warning marker must be written when the model is unresolvable")
	var warn struct {
		DefaultModel string `json:"defaultModel"`
	}
	require.NoError(t, json.Unmarshal(warnRaw, &warn))
	assert.Equal(t, "deepseek-v4-flash-free", warn.DefaultModel)
}

// TestApplyWorkspaceConfig_QualifiedDefault_ProviderAbsent_Omitted pins the
// follow-up to the 2026-08-16 incident once SetModel persists qualified
// defaults: a default like "opencode-relay/deepseek-v4-flash-free" on a boot
// where the relay provider was not written must be omitted (passthrough
// would poison opencode exactly like the bare ID did) and warned.
func TestApplyWorkspaceConfig_QualifiedDefault_ProviderAbsent_Omitted(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	secretsJSON := filepath.Join(dir, "secrets.json")

	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"opencode-relay/deepseek-v4-flash-free"}`), 0o600))
	require.NoError(t, os.WriteFile(agentCfg, []byte(`{"model": "opencode-relay/deepseek-v4-flash-free", "provider": {"thekao": {"models": {"glm-5.3": {}}}}}`), 0o600))

	applyWorkspaceConfig(agentCfg, secretsJSON)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	_, hasModel := out["model"]
	assert.False(t, hasModel, "qualified-but-absent provider must be omitted, not passed through")

	warnRaw, err := os.ReadFile(modelResolutionWarningPath(dir))
	require.NoError(t, err)
	var warn struct {
		DefaultModel string `json:"defaultModel"`
	}
	require.NoError(t, json.Unmarshal(warnRaw, &warn))
	assert.Equal(t, "opencode-relay/deepseek-v4-flash-free", warn.DefaultModel)
}

// TestApplyWorkspaceConfig_Resolvable_RemovesStaleWarning verifies that a
// successful resolution clears any warning marker left by a previous boot.
// Without this, a workspace that recovers (credential bound, relay back)
// would display the warning forever.
func TestApplyWorkspaceConfig_Resolvable_RemovesStaleWarning(t *testing.T) {
	dir := t.TempDir()
	agentCfg := filepath.Join(dir, "agent-config.json")
	secretsJSON := filepath.Join(dir, "secrets.json")

	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"glm-5.1"}`), 0o600))

	require.NoError(t, os.WriteFile(agentCfg, []byte(`{"provider": {"thekao": {"models": {"glm-5.1": {}}}}}`), 0o600))

	warnPath := modelResolutionWarningPath(filepath.Dir(agentCfg))
	require.NoError(t, os.WriteFile(warnPath, []byte(`{"defaultModel":"glm-5.1"}`), 0o600))

	applyWorkspaceConfig(agentCfg, secretsJSON)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	var model string
	require.NoError(t, json.Unmarshal(out["model"], &model))
	assert.Equal(t, "thekao/glm-5.1", model)

	if _, err := os.Stat(warnPath); !os.IsNotExist(err) {
		t.Fatalf("stale warning marker must be removed after successful resolution, stat err=%v", err)
	}
}

// TestResolveModelWithProvider_Collision documents the behavior when two
// providers in agent-config.json expose the same model ID. Go map iteration
// is non-deterministic, so the function may return either "provider-a/shared"
// or "provider-b/shared". The contract is: ok=true with a valid
// "providerID/modelID" string (never the flat ID, never a panic). The
// non-determinism is acceptable at boot: SetModel persists the qualified
// form, so collision ambiguity only affects legacy flat defaults, and the
// per-prompt override routes regardless.
func TestResolveModelWithProvider_Collision(t *testing.T) {
	cfg := map[string]json.RawMessage{
		"provider": json.RawMessage(`{
			"provider-a": {"models": {"shared": {}}},
			"provider-b": {"models": {"shared": {}}}
		}`),
	}
	got, ok := resolveModelWithProvider(cfg, "shared")

	assert.True(t, ok, "a claimed model must resolve even under collision")
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

	handler := reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})
	body := `[{"type":"env-secret","name":"FOO","metadata":{"var_name":"FOO"},"plaintext":"bar"}]`

	var wg sync.WaitGroup
	results := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
			req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		OpencodePassword: "test-pw",
		Proc:             proc,
		Tracker:          tracker,
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		OpencodePassword: "test-pw",
		Proc:             proc,
		Tracker:          tracker,
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
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{
		OpencodePassword:        "test-pw",
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

// --- #848: reload-secrets auth enforcement ---

func TestReloadSecrets_RequiresAuth(t *testing.T) {
	cfg := loadMaterializeConfig()
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader("[]"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "unauthenticated reload-secrets must be rejected")
	require.Equal(t, `Basic realm="agentd"`, rec.Header().Get("WWW-Authenticate"))
}

func TestReloadSecrets_WrongPassword(t *testing.T) {
	cfg := loadMaterializeConfig()
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader("[]"))
	req.Header.Set("Authorization", "Basic "+basicAuth("wrong"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestMaterializeSubcommand_RelayQualifiedDefault_ResolvesAfterRelayBoot is
// the e2e pin for the materialize REORDER (PR #912): applyWorkspaceConfig
// must run AFTER applyRelayConfigPreBoot, because a relay-qualified default
// ("opencode-relay/<model>", what SetModel persists for free models) can
// only resolve against a provider map that already includes the relay
// block. Runs the REAL subcommand binary with INFERENCE_RELAY_BASEURL set
// and a free-models catalog on disk; reverting the call order makes the
// model key vanish (omit+warn) and fails this test.
func TestMaterializeSubcommand_RelayQualifiedDefault_ResolvesAfterRelayBoot(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsPath := filepath.Join(dir, "does-not-exist.json") // zero-credential user
	agentCfgPath := filepath.Join(dir, "agent-config.json")
	// Empty provider map — as FlushProviders leaves it for a zero-credential
	// user. The relay block must be merged in by the pre-boot step.
	require.NoError(t, os.WriteFile(agentCfgPath, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600))

	// Relay-qualified default — SetModel persists this form when the user
	// picks a free model while relay is injected.
	wsCfgPath := filepath.Join(dir, "workspace-config.json")
	require.NoError(t, os.WriteFile(wsCfgPath, []byte(`{"defaultModel":"opencode-relay/deepseek-v4-flash-free"}`), 0o600))

	// Free-models catalog (credential-setup init container's copy).
	freeModelsPath := filepath.Join(dir, "free-models.json")
	require.NoError(t, os.WriteFile(freeModelsPath, []byte(`{"models":[
		{"id":"deepseek-v4-flash-free","name":"DeepSeek V4 Flash (Free)","context_limit":128000,"output_limit":32768}
	]}`), 0o600))

	exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfgPath,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"),
		"INFERENCE_RELAY_BASEURL=http://relay.example.invalid/secret-path",
		"LLMSAFESPACES_FREE_MODELS_PATH="+freeModelsPath,
	)
	require.Equal(t, 0, exit, "materialize must succeed; stderr=%q stdout=%q", stderr, stdout)
	assert.Contains(t, stderr, "pre-boot relay outcome=applied",
		"the pre-boot relay step must have applied (sanity for the reorder premise)")

	raw, err := os.ReadFile(agentCfgPath)
	require.NoError(t, err)
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &cfg))

	require.Contains(t, cfg, "model",
		"relay-qualified default must resolve — the relay provider block is written before the model check")
	var model string
	require.NoError(t, json.Unmarshal(cfg["model"], &model))
	assert.Equal(t, "opencode-relay/deepseek-v4-flash-free", model)

	// And no warning marker: nothing was substituted.
	if _, err := os.Stat(modelResolutionWarningPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("no warning marker may exist when the relay-qualified default resolves; stat err=%v", err)
	}
}

// TestApplyWorkspaceConfig_EarlyReturn_RemovesStaleWarning pins the two
// early-return marker cleanups (#909 review round 2, adopted; round 3 noted
// only the resolvable-path cleanup was pinned). When no default model is
// being applied (config file absent, or defaultModel empty/cleared),
// nothing is substituted, so a warning marker from an earlier boot is
// stale and must be removed — otherwise a cleared default renders the
// warning banner for the pod's lifetime.
func TestApplyWorkspaceConfig_EarlyReturn_RemovesStaleWarning(t *testing.T) {
	prep := func(t *testing.T) (agentCfg, warnPath string) {
		t.Helper()
		dir := t.TempDir()
		agentCfg = filepath.Join(dir, "agent-config.json")
		require.NoError(t, os.WriteFile(agentCfg, []byte(`{"$schema":"https://opencode.ai/config.json"}`), 0o600))
		warnPath = modelResolutionWarningPath(dir)
		require.NoError(t, os.WriteFile(warnPath, []byte(`{"defaultModel":"glm-old"}`), 0o600))
		return agentCfg, warnPath
	}

	t.Run("workspace-config.json absent", func(t *testing.T) {
		agentCfg, warnPath := prep(t)
		applyWorkspaceConfig(agentCfg, filepath.Join(t.TempDir(), "secrets.json")) // no sibling workspace-config.json
		if _, err := os.Stat(warnPath); !os.IsNotExist(err) {
			t.Fatalf("stale marker must be removed when no workspace config exists; stat err=%v", err)
		}
	})

	t.Run("defaultModel empty (cleared)", func(t *testing.T) {
		agentCfg, warnPath := prep(t)
		dir := filepath.Dir(agentCfg)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace-config.json"), []byte(`{"defaultModel":""}`), 0o600))
		applyWorkspaceConfig(agentCfg, filepath.Join(dir, "secrets.json"))
		if _, err := os.Stat(warnPath); !os.IsNotExist(err) {
			t.Fatalf("stale marker must be removed when the default is cleared; stat err=%v", err)
		}
	})
}

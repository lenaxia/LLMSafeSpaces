// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Integration tests for issue #443: container restart must not wipe
// user-DEK credentials.
//
// These exercise the full boot-time materialize subcommand as a real
// subprocess (mirroring TestMaterializeSubcommand_*), driving the
// persist-and-replay contract end to end:
//
//   1. A reload-secrets push persists the batch to the cache file.
//   2. A subsequent `materialize` boot (container restart) replays the
//      cache merged on top of the base secrets.json.
//   3. Pod recreation (cache absent) falls back to base-only (preserves the
//      "user-owned creds do not materialize at first boot" invariant).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheMaterializeEnv returns the LLMSAFESPACES_* env additions for the
// materialize subprocess, including the new RELOAD_CACHE_PATH override.
func cacheMaterializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds, cachePath string) []string {
	return []string{
		"LLMSAFESPACES_SECRETS_BASE_DIR=" + secretsBase,
		"LLMSAFESPACES_SSH_DIR=" + sshDir,
		"LLMSAFESPACES_AGENT_CONFIG_PATH=" + agentCfg,
		"LLMSAFESPACES_SECRETS_ENV_PATH=" + envPath,
		"LLMSAFESPACES_GIT_CREDS_PATH=" + gitCreds,
		"LLMSAFESPACES_RELOAD_CACHE_PATH=" + cachePath,
		"HOME=" + filepath.Dir(sshDir),
	}
}

// runCacheMaterialize runs `workspace-agentd materialize --from <path>` with
// the reload-cache override, returning exit code + stderr.
func runCacheMaterialize(t *testing.T, bin, secretsPath string, env []string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, "materialize", "--from", secretsPath)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("subprocess failed: %v stderr=%q", err, stderr.String())
	}
	return exit, stderr.String()
}

// TestMaterialize_WritesCacheOnBoot is the regression test for design 0045
// Change 3: the init-container's materialize subcommand must persist its
// applied batch to the reload cache so agentd's hasUserCreds probe reports
// UserCredsPresent=true from the first healthz call.
//
// Without this write, the pre-existing hasUserCreds implementation
// (healthz.go) reads only the reload-cache path — the cache is empty on
// a fresh pod, so UserCredsPresent=false, so the API's watcher fires a
// spurious auto-push ~30s into pod life. Auto-push applies the same batch
// pod-bootstrap already delivered (design 0045 Change 1) and triggers a
// wasteful opencode restart.
//
// With this write, hasUserCreds returns true on the first healthz call,
// secretautopush observes UserCredsPresent=true, emits skipped_ucp_true,
// and no restart fires.
func TestMaterialize_WritesCacheOnBoot(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")

	// Simulate pod-bootstrap having delivered user-DEK secrets (design 0045
	// Change 1). Both server-KEK and user-DEK entries arrive together.
	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[
		{"type":"env-secret","name":"gh","metadata":{"var_name":"GH_TOKEN"},"plaintext":"tok-boot-value"},
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	env := cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath)

	// Pre-condition: no cache on a fresh pod.
	_, err := os.Stat(cachePath)
	require.True(t, os.IsNotExist(err))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath, env)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	// Post-condition: cache exists. This is what hasUserCreds reads.
	require.FileExists(t, cachePath,
		"materialize must persist the reload cache so hasUserCreds reports "+
			"UserCredsPresent=true on the first healthz — preventing the spurious "+
			"auto-push that would otherwise trigger a wasteful opencode restart")

	cacheContent, err := os.ReadFile(cachePath)
	require.NoError(t, err)
	assert.Contains(t, string(cacheContent), "GH_TOKEN",
		"cache must include the user-DEK env-secret to reflect what was materialized")
	assert.Contains(t, string(cacheContent), "tok-boot-value")
	assert.Contains(t, string(cacheContent), "SERVER_CFG")
}

// TestMaterialize_EmptySecrets_DoesNotWriteCache verifies the empty-batch
// short-circuit: a workspace with no bindings must not write an empty
// cache file. hasUserCreds would return true on a zero-length batch (via
// len(batch) > 0 == false), so the cache-absent state and the empty-cache
// state are semantically equivalent — but writing an empty file costs a
// tmpfs I/O and clutters the fs unnecessarily.
func TestMaterialize_EmptySecrets_DoesNotWriteCache(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")

	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[]`), 0o600))

	env := cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath)

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath, env)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	_, err := os.Stat(cachePath)
	assert.True(t, os.IsNotExist(err),
		"empty secrets batch must not write an empty cache file")
}

// TestContainerRestart_ReplaysUserDEKCreds is THE regression test for #443.
// It simulates the exact production sequence:
//
//	(a) boot pod with base secrets.json (server-KEK env-secret ONLY),
//	(b) live reload-secrets push delivers a user-DEK env-secret (GH_TOKEN),
//	(c) container restarts → materialize re-runs → BOTH creds must survive.
//
// Pre-fix, step (c) wiped GH_TOKEN because reset() nukes /sandbox-runtime and
// the base secrets.json never contained it.
func TestContainerRestart_ReplaysUserDEKCreds(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsBase := filepath.Join(dir, "secrets")
	sshDir := filepath.Join(dir, ".ssh")
	agentCfg := filepath.Join(dir, "agent-config.json")
	envPath := filepath.Join(dir, "env")
	gitCreds := filepath.Join(dir, ".git-credentials")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")

	// (a) Base secrets.json: only a server-KEK env-secret (as bootstrap writes).
	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds, cachePath))
	require.Equal(t, 0, exit, "first boot failed; stderr=%q", stderr)
	requireEnvHasVar(t, envPath, "SERVER_CFG")

	// (b) Live reload-secrets push delivers a user-DEK env-secret (GH_TOKEN).
	// This represents POST /v1/reload-secrets from the API after a user binds
	// a credential. The handler must persist the batch to the cache file.
	cfg := materializeConfig{
		secretsBaseDir:  secretsBase,
		sshDir:          sshDir,
		agentConfigPath: agentCfg,
		secretsEnvPath:  envPath,
		gitCredsPath:    gitCreds,
		home:            dir,
		reloadCachePath: cachePath,
	}
	reloadBody := `[{"type":"env-secret","name":"gh","metadata":{"var_name":"GH_TOKEN"},"plaintext":"tok-12345"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(reloadBody))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "reload failed: %s", rec.Body.String())

	// The reload is a full-replace: its cache represents the complete live
	// state. GH_TOKEN must be present in the cache file.
	require.FileExists(t, cachePath, "reload must persist the cache (the fix)")

	// (c) Simulate the container restart: materialize re-runs from scratch on
	// the SAME tmpfs (cache survives) but reset() has just wiped /sandbox-runtime.
	// Simulate the wipe by removing the env file (reset would do this).
	require.NoError(t, os.Remove(envPath))

	exit, stderr = runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds, cachePath))
	require.Equal(t, 0, exit, "restart boot failed; stderr=%q", stderr)

	// BOTH creds must be present: the user-DEK GH_TOKEN (from cache replay)
	// AND the server-KEK SERVER_CFG (from base). Pre-fix, GH_TOKEN was GONE.
	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "export GH_TOKEN=",
		"user-DEK env-secret must survive container restart via cache replay (#443)")
	assert.Contains(t, string(envContent), "tok-12345",
		"the replayed value must be the cache value, not a stale default")
	assert.Contains(t, string(envContent), "export SERVER_CFG=",
		"server-KEK env-secret from base must also be present")
}

// TestContainerRestart_NoCache_FallsBackToBaseOnly pins the first-boot /
// pod-recreation invariant: when the cache is absent (fresh tmpfs), only the
// base secrets.json materializes. This preserves the existing contract that
// user-owned creds do NOT appear at first boot.
func TestContainerRestart_NoCache_FallsBackToBaseOnly(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json") // deliberately NOT created

	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
			filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath))
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "export SERVER_CFG=")
	assert.NotContains(t, string(envContent), "GH_TOKEN",
		"no cache → no user-DEK creds at boot (first-boot invariant preserved)")
}

// TestContainerRestart_CorruptCache_FallsBackToBase verifies graceful
// degradation: a corrupt cache file must NOT crash the boot; it warns and
// falls back to base-only materialization.
func TestContainerRestart_CorruptCache_FallsBackToBase(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	require.NoError(t, os.WriteFile(cachePath, []byte("{{not json"), 0o600))

	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
			filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath))

	require.Equal(t, 0, exit, "corrupt cache must not fail the boot; stderr=%q", stderr)
	assert.Contains(t, stderr, "last-reload-secrets",
		"corrupt cache must warn so the missing creds are diagnosable")

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "export SERVER_CFG=",
		"base creds must still materialize despite corrupt cache")
}

// TestContainerRestart_PodRecreation_WipesCache pins US-35.7: on full pod
// recreation (not container restart) the tmpfs is wiped, so the cache is
// absent on the first materialize call. The security invariant (no
// plaintext user creds on the PVC at rest) is preserved because both
// /sandbox-cfg and /sandbox-runtime are memory-backed emptyDirs.
//
// After design 0045 Change 3, materialize *does* write the cache during
// the first boot on the fresh pod. The test verifies the pre-boot state
// (cache absent) and asserts server-KEK-only base creds materialize
// without user-DEK bleed-through.
func TestContainerRestart_PodRecreation_WipesCache(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")

	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	env := cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath)

	// Pre-boot: no cache (fresh pod).
	_, err := os.Stat(cachePath)
	require.True(t, os.IsNotExist(err), "fresh pod must have no cache before first materialize")

	// First boot on the fresh pod (pod-bootstrap here degraded to server-KEK
	// only — user's jwt_session absent). Base creds materialize.
	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath, env)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	// User-DEK creds absent (base secrets.json here has server-KEK only).
	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.NotContains(t, string(envContent), "GH_TOKEN",
		"pod-bootstrap-degrade path materializes no user-DEK content")
}

// TestContainerRestart_SSHKeySurvivesRestart verifies the replay path for a
// non-env credential type (SSH key), ensuring the merge + replay covers all
// user-DEK credential categories, not just env-secrets.
func TestContainerRestart_SSHKeySurvivesRestart(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	sshDir := filepath.Join(dir, ".ssh")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	baseSecretsPath := filepath.Join(dir, "secrets.json")

	// Base is empty (user has no server-KEK creds).
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[]`), 0o600))

	// Live push delivers an SSH key.
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          sshDir,
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
		reloadCachePath: cachePath,
	}
	reloadBody := `[{"type":"ssh-key","name":"id_ed25519","metadata":{"key_type":"ed25519"},"plaintext":"ssh-ed25519 AAAA..."}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(reloadBody))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Wipe ssh dir (simulating reset() on restart).
	require.NoError(t, os.RemoveAll(sshDir))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(cfg.secretsBaseDir, sshDir, cfg.agentConfigPath, cfg.secretsEnvPath, cfg.gitCredsPath, cachePath))
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	entries, err := os.ReadDir(sshDir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "SSH key must survive container restart via cache replay")
}

// TestContainerRestart_CredentialRemoval_NotReplayed locks down the unbind
// path: a reload with a SMALLER batch (user removed a credential) must result
// in the removed credential being absent after restart. Because reload is a
// full-replace and the cache holds the latest complete state, replay rebuilds
// from the merged batch (cache wins) and the removed cred must NOT reappear.
// Without this test, a regression that breaks reset() for the removal case
// would go undetected.
func TestContainerRestart_CredentialRemoval_NotReplayed(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	baseSecretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[]`), 0o600))

	env := cacheMaterializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
		filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials"), cachePath)

	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  envPath,
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
		reloadCachePath: cachePath,
	}

	// Initial live state: two env-secrets bound.
	first := `[{"type":"env-secret","name":"keep","metadata":{"var_name":"KEEP"},"plaintext":"1"},{"type":"env-secret","name":"remove","metadata":{"var_name":"REMOVE"},"plaintext":"2"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(first))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// User unbinds "remove": the new reload batch omits it. The cache must now
	// reflect ONLY "keep" — reload is a full-replace, so the cache is overwritten.
	second := `[{"type":"env-secret","name":"keep","metadata":{"var_name":"KEEP"},"plaintext":"1"}]`
	req = httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(second))
	rec = httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Simulate the container restart: reset() wipes the env file.
	require.NoError(t, os.Remove(envPath))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath, env)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export KEEP=",
		"retained credential must be replayed after restart")
	assert.NotContains(t, string(envContent), "export REMOVE=",
		"unbound credential must NOT reappear after restart (cache reflects the full-replace reload)")
}

// TestContainerRestart_LLMProviderSurvivesRestart covers the provider code
// path (FlushProviders/FormatProviders → agent-config.json), which is distinct
// from the env-secret/ssh-key paths. A user-owned LLM provider bound after
// boot must survive a container restart via cache replay and land in
// agent-config.json.
func TestContainerRestart_LLMProviderSurvivesRestart(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	agentCfg := filepath.Join(dir, "agent-config.json")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	baseSecretsPath := filepath.Join(dir, "secrets.json")
	// Base is empty — no server-KEK providers.
	require.NoError(t, os.WriteFile(baseSecretsPath, []byte(`[]`), 0o600))

	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: agentCfg,
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
		reloadCachePath: cachePath,
	}

	// A user-owned LLM provider (delivered with a session DEK, hence absent
	// from base secrets.json and only present via the reload push).
	provider := `{"type":"llm-provider","name":"user-openai","plaintext":"{\"kind\":\"openai\",\"slug\":\"user-openai\",\"apiKey\":\"sk-user-123\"}"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader("["+provider+"]"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{})(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Simulate the restart: reset() would wipe agent-config.json. In this test
	// the handler runs without an AgentConfigWriter (nil), so the handler never
	// wrote agent-config.json — RemoveAll tolerates absence, matching reset().
	require.NoError(t, os.RemoveAll(agentCfg))

	exit, stderr := runCacheMaterialize(t, bin, baseSecretsPath,
		cacheMaterializeEnv(cfg.secretsBaseDir, cfg.sshDir, agentCfg, cfg.secretsEnvPath, cfg.gitCredsPath, cachePath))
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	// agent-config.json must contain the replayed provider. The provider key
	// is the slug (Epic 55) — pre-fix, a restart would leave agent-config.json
	// empty because reset() wiped it and the base secrets.json had no providers.
	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "user-openai",
		"user-owned LLM provider must survive container restart via cache replay (#443)")
}

// requireEnvHasVar asserts the materialized env file contains an export line
// for the given variable name.
func requireEnvHasVar(t *testing.T, envPath, varName string) {
	t.Helper()
	data, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "export "+varName+"=", "env file missing %s", varName)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Integration tests for issue #443: container restart must not wipe
// user-DEK credentials.
//
// US-70.5 deleted the reload-cache replay (#443's original mechanism).
// The durable state is now the BATCH FILE the resync pull persists on
// every applied revision (resync_secrets.go writes the fetched envelope
// verbatim before applying). A container restart re-runs the
// materialize subcommand against that file — re-materialization is by
// pull, by design:
//
//  1. A resync pull applies a revisioned envelope and persists it.
//  2. A subsequent `materialize` boot (container restart; tmpfs wiped)
//     re-applies the persisted envelope.
//  3. Revocation converges the same way: a newer, smaller envelope
//     replaces the file and the restart materializes only what remains.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materializeEnv returns the LLMSAFESPACES_* env additions for the
// materialize subprocess.
func materializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds string) []string {
	return []string{
		"LLMSAFESPACES_SECRETS_BASE_DIR=" + secretsBase,
		"LLMSAFESPACES_SSH_DIR=" + sshDir,
		"LLMSAFESPACES_AGENT_CONFIG_PATH=" + agentCfg,
		"LLMSAFESPACES_SECRETS_ENV_PATH=" + envPath,
		"LLMSAFESPACES_GIT_CREDS_PATH=" + gitCreds,
		"HOME=" + filepath.Dir(sshDir),
	}
}

// runMaterialize runs `workspace-agentd materialize --from <path>`,
// returning exit code + stderr.
func runMaterialize(t *testing.T, bin, secretsPath string, env []string) (int, string) {
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

// pullAndApply drives the real resyncSecretsHandler against a fake
// bootstrap API serving the given envelope — the pull path that
// replaces the deleted reload push.
func pullAndApply(t *testing.T, cfg materializeConfig, batchPath, envelope string) {
	t.Helper()
	withTestLogger(t)
	tokenPath := filepath.Join(filepath.Dir(batchPath), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("restart-pin-tok"), 0o600))

	api := &mutableBootstrapAPI{}
	apiSrv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(apiSrv.Close)
	api.set(http.StatusOK, `{"secrets":`+envelope+`}`)

	handler := resyncSecretsHandler(resyncDeps{
		cfg:         cfg,
		apply:       applySecretsDeps{OpencodePassword: resyncTestPassword},
		apiURL:      apiSrv.URL,
		workspaceID: "ws-restart-pin",
		tokenPath:   tokenPath,
		batchPath:   batchPath,
		minInterval: time.Nanosecond,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
	rec := httptest.NewRecorder()
	handler(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "resync pull failed: %s", rec.Body.String())
}

// TestContainerRestart_PulledEnvelopeReapplied is THE #443 regression,
// post-US-70.5: the resync pull persists its applied envelope to the
// batch file, and a container restart (tmpfs wiped — simulated by
// removing the env file) re-materializes BOTH the user-DEK env-secret
// and the base server-KEK env-secret from that file alone.
func TestContainerRestart_PulledEnvelopeReapplied(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	secretsBase := filepath.Join(dir, "secrets")
	sshDir := filepath.Join(dir, ".ssh")
	agentCfg := filepath.Join(dir, "agent-config.json")
	envPath := filepath.Join(dir, "env")
	gitCreds := filepath.Join(dir, ".git-credentials")
	batchPath := filepath.Join(dir, "secrets.json")

	// (a) Boot: base secrets.json carries a server-KEK env-secret only
	// (as the bootstrap init container's first pull wrote it).
	require.NoError(t, os.WriteFile(batchPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))
	exit, stderr := runMaterialize(t, bin, batchPath,
		materializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds))
	require.Equal(t, 0, exit, "first boot failed; stderr=%q", stderr)
	requireEnvHasVar(t, envPath, "SERVER_CFG")

	// (b) Bind: the resync pull delivers a user-DEK env-secret and
	// persists the new envelope over the batch file.
	cfg := materializeConfig{
		secretsBaseDir:   secretsBase,
		sshDir:           sshDir,
		agentConfigPath:  agentCfg,
		secretsEnvPath:   envPath,
		gitCredsPath:     gitCreds,
		home:             dir,
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}
	pullAndApply(t, cfg, batchPath,
		`{"entries":[{"secretId":"s1","version":1,"type":"env-secret","name":"base","value":"server-val","metadata":{"var_name":"SERVER_CFG"}},{"secretId":"s2","version":1,"type":"env-secret","name":"gh","value":"tok-12345","metadata":{"var_name":"GH_TOKEN"}}],"revision":{"seq":2,"manifestHash":"mh-2","batchHash":"bh-2"}}`)

	raw, err := os.ReadFile(batchPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), "GH_TOKEN", "the resync pull must persist the applied envelope (the restart surface)")

	// (c) Container restart: reset() wiped the tmpfs (env file + rev
	// anchor gone — both live beside it on /sandbox-runtime in
	// production); materialize re-applies the persisted envelope.
	require.NoError(t, os.Remove(envPath))
	require.NoError(t, os.Remove(revAnchorPath(envPath)))
	exit, stderr = runMaterialize(t, bin, batchPath,
		materializeEnv(secretsBase, sshDir, agentCfg, envPath, gitCreds))
	require.Equal(t, 0, exit, "restart boot failed; stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "export GH_TOKEN=",
		"user-DEK env-secret must survive container restart via the pulled envelope (#443, now by pull)")
	assert.Contains(t, string(envContent), "tok-12345")
	assert.Contains(t, string(envContent), "export SERVER_CFG=",
		"server-KEK env-secret must also survive the restart")
}

// TestContainerRestart_BootWithNoUserDEKContent pins the degrade-free
// base path: a batch file with server-KEK content only (owner offline
// at pull time, or no bindings) materializes exactly that — no phantom
// user-DEK content, no failure.
func TestContainerRestart_BootWithNoUserDEKContent(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	batchPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(batchPath, []byte(`[
		{"type":"env-secret","name":"server-cfg","metadata":{"var_name":"SERVER_CFG"},"plaintext":"server-val"}
	]`), 0o600))

	exit, stderr := runMaterialize(t, bin, batchPath,
		materializeEnv(filepath.Join(dir, "secrets"), filepath.Join(dir, ".ssh"),
			filepath.Join(dir, "agent-config.json"), envPath, filepath.Join(dir, ".git-credentials")))
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "export SERVER_CFG=")
	assert.NotContains(t, string(envContent), "GH_TOKEN",
		"no user-DEK content in the batch file → no user-DEK creds materialized")
}

// TestContainerRestart_RevocationConvergesByPull locks down the unbind
// path: a newer, smaller envelope replaces the batch file, and the
// restart materializes only the retained credential — the removed one
// must NOT reappear (the file is the full truth, not an overlay).
func TestContainerRestart_RevocationConvergesByPull(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	envPath := filepath.Join(dir, "env")
	batchPath := filepath.Join(dir, "secrets.json")
	cfg := materializeConfig{
		home:             dir,
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:   envPath,
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}

	pullAndApply(t, cfg, batchPath,
		`{"entries":[{"secretId":"s1","version":1,"type":"env-secret","name":"keep","value":"1","metadata":{"var_name":"KEEP"}},{"secretId":"s2","version":1,"type":"env-secret","name":"remove","value":"2","metadata":{"var_name":"REMOVE"}}],"revision":{"seq":3,"manifestHash":"mh-3","batchHash":"bh-3"}}`)

	// User unbinds "remove": the newer envelope omits it.
	pullAndApply(t, cfg, batchPath,
		`{"entries":[{"secretId":"s1","version":2,"type":"env-secret","name":"keep","value":"1","metadata":{"var_name":"KEEP"}}],"revision":{"seq":4,"manifestHash":"mh-4","batchHash":"bh-4"}}`)

	// Container restart (tmpfs wiped: env file + anchor).
	require.NoError(t, os.Remove(envPath))
	require.NoError(t, os.Remove(revAnchorPath(envPath)))
	exit, stderr := runMaterialize(t, bin, batchPath,
		materializeEnv(cfg.secretsBaseDir, cfg.sshDir, cfg.agentConfigPath, envPath, cfg.gitCredsPath))
	require.Equal(t, 0, exit, "restart boot failed; stderr=%q", stderr)

	envContent, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(envContent), "export KEEP=",
		"retained credential must survive the restart")
	assert.NotContains(t, string(envContent), "export REMOVE=",
		"unbound credential must NOT reappear after restart (the batch file is full-replace)")
}

// TestContainerRestart_LLMProviderSurvivesRestart covers the provider
// code path (FlushProviders → agent-config.json): a user-owned LLM
// provider delivered by pull must land in agent-config.json again after
// the restart re-materializes from the persisted envelope.
func TestContainerRestart_LLMProviderSurvivesRestart(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	agentCfg := filepath.Join(dir, "agent-config.json")
	envPath := filepath.Join(dir, "env")
	batchPath := filepath.Join(dir, "secrets.json")
	cfg := materializeConfig{
		home:             dir,
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, ".ssh"),
		agentConfigPath:  agentCfg,
		secretsEnvPath:   envPath,
		gitCredsPath:     filepath.Join(dir, ".git-credentials"),
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}

	pullAndApply(t, cfg, batchPath,
		`{"entries":[{"secretId":"p1","version":1,"type":"llm-provider","name":"user-openai","value":"{\"kind\":\"openai\",\"slug\":\"user-openai\",\"apiKey\":\"sk-user-123\"}"}],"revision":{"seq":5,"manifestHash":"mh-5","batchHash":"bh-5"}}`)

	// Simulate the restart: reset() wipes agent-config.json and the rev
	// anchor with the rest of the tmpfs.
	require.NoError(t, os.RemoveAll(agentCfg))
	require.NoError(t, os.Remove(revAnchorPath(envPath)))

	exit, stderr := runMaterialize(t, bin, batchPath,
		materializeEnv(cfg.secretsBaseDir, cfg.sshDir, agentCfg, envPath, cfg.gitCredsPath))
	require.Equal(t, 0, exit, "restart boot failed; stderr=%q", stderr)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "user-openai",
		"user-owned LLM provider must survive container restart via the pulled envelope (#443, now by pull)")
}

// requireEnvHasVar asserts the materialized env file contains an export line
// for the given variable name.
func requireEnvHasVar(t *testing.T, envPath, varName string) {
	t.Helper()
	data, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "export "+varName+"=", "env file missing %s", varName)
}

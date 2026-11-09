// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// restart_payload_test.go — restart suppression on an unchanged
// restart-relevant payload (env-secret / api-key classes).
//
// Production incident (2026-11-09): the secrets-reconcile notify cadence
// re-applied a byte-identical batch every ~46s and each apply scheduled an
// opencode restart (shouldRestart is type-gated only — no content
// comparison), killing in-flight chat turns and driving multi-minute
// reconnect storms. These tests pin the content gate: an apply whose
// restart-relevant payload (type|name|value) is byte-identical to the
// last applied payload must NOT schedule a restart; a changed value must.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

func envSecretBatch(value string) string {
	return `[{"type":"env-secret","name":"x","metadata":{"var_name":"GH_TOKEN"},"plaintext":"` + value + `"}]`
}

func testSecrets(pairs ...string) []secrets.Secret {
	out := make([]secrets.Secret, 0, len(pairs)/3)
	for i := 0; i+2 < len(pairs); i += 3 {
		md := map[string]string{}
		if pairs[i] == "env-secret" {
			md["var_name"] = pairs[i+1]
		}
		out = append(out, secrets.Secret{Type: pairs[i], Name: pairs[i+1], Metadata: md, Plaintext: pairs[i+2]})
	}
	return out
}

// TestApplyBatch_IdenticalPayloadSuppressesRestart — the second apply of a
// byte-identical restart-relevant batch must not restart opencode; the
// payload hash on disk must reflect the applied payload.
func TestApplyBatch_IdenticalPayloadSuppressesRestart(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	markerPath := filepath.Join(dir, "restart-reason.json")

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/apply-batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
		rec := httptest.NewRecorder()
		applyBatchHandler(cfg, applySecretsDeps{
			OpencodePassword:        "test-pw",
			Proc:                    proc,
			Tracker:                 tracker,
			RestartReasonMarkerPath: markerPath,
		})(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		return rec
	}

	// First apply: restart fires and the payload hash lands.
	post(envSecretBatch("v1"))
	require.Equal(t, 1, proc.restartCount(), "first apply of a new payload must restart")

	hashPath := restartPayloadPath(cfg.toPaths().SecretsEnvPath)
	stored, err := readRestartPayloadHash(hashPath)
	require.NoError(t, err, "first apply must persist the payload hash")
	require.NotEmpty(t, stored)

	// Second apply: byte-identical payload → restart suppressed.
	post(envSecretBatch("v1"))
	assert.Equal(t, 1, proc.restartCount(), "identical payload re-apply must NOT restart opencode")

	// Third apply: changed value → restart fires again.
	post(envSecretBatch("v2"))
	assert.Equal(t, 2, proc.restartCount(), "changed payload must restart opencode")
	stored2, err := readRestartPayloadHash(hashPath)
	require.NoError(t, err)
	assert.NotEqual(t, stored, stored2, "hash must track the applied payload")
}

// TestApplyBatch_ApiKeyClassIncludedInPayload — api-key entries participate
// in the payload identity: identical api-key re-apply is suppressed, a
// changed one restarts.
func TestApplyBatch_ApiKeyClassIncludedInPayload(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	markerPath := filepath.Join(dir, "restart-reason.json")

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/apply-batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
		rec := httptest.NewRecorder()
		applyBatchHandler(cfg, applySecretsDeps{
			OpencodePassword:        "test-pw",
			Proc:                    proc,
			Tracker:                 tracker,
			RestartReasonMarkerPath: markerPath,
		})(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	}

	batch := `[{"type":"api-key","name":"anthropic","plaintext":"sk-1"}]`
	post(batch)
	require.Equal(t, 1, proc.restartCount())
	post(batch)
	assert.Equal(t, 1, proc.restartCount(), "identical api-key re-apply must NOT restart")
	post(`[{"type":"api-key","name":"anthropic","plaintext":"sk-2"}]`)
	assert.Equal(t, 2, proc.restartCount(), "changed api-key must restart")
}

// TestApplyBatch_PartialFailureLeavesNoPayloadAnchor — a partially applied
// batch must not persist a payload hash (the on-disk state does not vouch
// for the batch), so the next apply restarts even if byte-identical. The
// partial failure path still restarts (the gate stays open — state is
// unknown, never "unchanged").
func TestApplyBatch_PartialFailureLeavesNoPayloadAnchor(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	// A FILE at the sshDir path makes applySSHKey's MkdirAll fail
	// (filesystem failure → OutcomeFailed → ErrPartialFailure) while the
	// env-secret entry materializes fine — a genuinely partial batch.
	sshDir := filepath.Join(dir, ".ssh")
	require.NoError(t, os.WriteFile(sshDir, []byte("blocker"), 0o600))
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          sshDir,
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	markerPath := filepath.Join(dir, "restart-reason.json")

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	req := httptest.NewRequest(http.MethodPost, "/v1/apply-batch",
		strings.NewReader(`[{"type":"ssh-key","name":"k","plaintext":"ssh-ed25519 AAAA test"},{"type":"env-secret","name":"x","metadata":{"var_name":"GH_TOKEN"},"plaintext":"v1"}]`))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	applyBatchHandler(cfg, applySecretsDeps{
		OpencodePassword:        "test-pw",
		Proc:                    proc,
		Tracker:                 tracker,
		RestartReasonMarkerPath: markerPath,
	})(rec, req)

	hashPath := restartPayloadPath(filepath.Join(dir, "secrets-env"))
	_, err := os.Stat(hashPath)
	require.True(t, os.IsNotExist(err), "partial failure must not persist a restart-payload hash")
}

// TestRestartPayloadHash_Canonicalization — the hash is order-independent
// and sensitive to every payload dimension (type, name, value).
func TestRestartPayloadHash_Canonicalization(t *testing.T) {
	a := []map[string]string{{"type": "env-secret", "name": "n", "value": "v"}}
	_ = a
	h1 := restartPayloadHash(testSecrets("env-secret", "GH_TOKEN", "v1", "env-secret", "A_ID", "42"))
	h2 := restartPayloadHash(testSecrets("env-secret", "A_ID", "42", "env-secret", "GH_TOKEN", "v1"))
	assert.Equal(t, h1, h2, "hash must be order-independent")

	h3 := restartPayloadHash(testSecrets("env-secret", "GH_TOKEN", "v2", "env-secret", "A_ID", "42"))
	assert.NotEqual(t, h1, h3, "value change must change the hash")

	h4 := restartPayloadHash(testSecrets("env-secret", "OTHER", "v1", "env-secret", "A_ID", "42"))
	assert.NotEqual(t, h1, h4, "name change must change the hash")

	h5 := restartPayloadHash(testSecrets("api-key", "GH_TOKEN", "v1", "env-secret", "A_ID", "42"))
	assert.NotEqual(t, h1, h5, "type change must change the hash")

	assert.NotEqual(t, "", restartPayloadHash(testSecrets("env-secret", "X", "1")),
		"restart-relevant batch must hash to a non-empty value")
	assert.Equal(t, "", restartPayloadHash(testSecrets("ssh-key", "k", "data")),
		"non-restart-relevant batch must hash to the empty sentinel")
}

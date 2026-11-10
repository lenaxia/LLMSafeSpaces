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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
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

// blockingProc wraps mockManagedProcess: the FIRST restart call blocks
// until released — it models the ~5s SIGTERM window of a real restart and
// lets a test interleave a second apply inside that window. Subsequent
// restarts (the interleaved apply's) proceed immediately; the CAS gate is
// the one-shot.
type blockingProc struct {
	mockManagedProcess
	blocked atomic.Bool
	release chan struct{}
}

func (b *blockingProc) restart() {
	if b.blocked.CompareAndSwap(false, true) {
		<-b.release
	}
	b.mockManagedProcess.restart()
}

// newPayloadTestConfig returns the shared single-container fixture.
func newPayloadTestConfig(t *testing.T) (materializeConfig, string) {
	t.Helper()
	dir := t.TempDir()
	return materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}, filepath.Join(dir, "restart-reason.json")
}

// TestApplyBatch_ConcurrentAnchorsSerializeInPipelineOrder — review
// finding 1 (PR #2011): the anchor write must land INSIDE applyMu, or a
// blocking restart from an earlier apply writes its anchor AFTER a later
// apply's, leaving the file describing an OLDER payload than the child's
// env — a needed restart would then be wrongly suppressed. Interleaving:
// apply v1 blocks inside restart() → apply v2 completes fully → release
// v1's restart. The final anchor must be v2's.
func TestApplyBatch_ConcurrentAnchorsSerializeInPipelineOrder(t *testing.T) {
	withTestLogger(t)
	cfg, markerPath := newPayloadTestConfig(t)

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	release := make(chan struct{})
	proc := &blockingProc{release: release}

	apply := func(body string) {
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

	// A: v1 — blocks inside restart().
	applyDone := make(chan struct{})
	go func() {
		defer close(applyDone)
		apply(envSecretBatch("v1"))
	}()
	// Wait until A has written its anchor (it precedes the parked restart
	// on every ordering — this pin covers the anchor-before-restart
	// restructure; the lock PLACEMENT pin is
	// TestApplyBatch_AnchorSurvivesStalledStageCredentials below).
	anchorPath := restartPayloadPath(cfg.toPaths().SecretsEnvPath)
	require.Eventually(t, func() bool {
		h, err := readRestartPayloadHash(anchorPath)
		return err == nil && h != ""
	}, 5*time.Second, 5*time.Millisecond, "A must anchor before its parked restart completes")

	// B: v2 — runs A's entire restart window inside its own locked
	// pipeline and anchors H2.
	apply(envSecretBatch("v2"))
	stored, err := readRestartPayloadHash(anchorPath)
	require.NoError(t, err)
	require.Equal(t, restartPayloadHash([]secrets.Secret{{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "v2"}}), stored,
		"B's anchor must be on disk while A is still parked in restart()")

	// Release A: without the fix A would now overwrite the anchor with H1.
	close(release)
	<-applyDone
	stored, err = readRestartPayloadHash(anchorPath)
	require.NoError(t, err)
	require.Equal(t, restartPayloadHash([]secrets.Secret{{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "v2"}}), stored,
		"the anchor must serialize in pipeline order: A's parked restart must not land a stale identity over B's")

	// And the gate must agree: re-applying v2 must NOT restart (suppressed);
	// re-applying v1 must restart (the child's env is v2).
	before := proc.restartCount()
	apply(envSecretBatch("v2"))
	assert.Equal(t, before, proc.restartCount(), "v2 re-apply must be suppressed")
	apply(envSecretBatch("v1"))
	assert.Equal(t, before+1, proc.restartCount(), "v1 re-apply must restart (env is v2)")
}

// TestApplyBatch_UnreadablePayloadHashFailsOpen — review missing-case 2:
// a chmod-000 anchor must not brick the gate; the restart proceeds and a
// warn is logged. Skipped when running as root (root reads anything).
func TestApplyBatch_UnreadablePayloadHashFailsOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits; the fail-open path needs a non-root runner")
	}
	withTestLogger(t)
	cfg, markerPath := newPayloadTestConfig(t)

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	// First apply anchors the hash and restarts.
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
	post(envSecretBatch("v1"))
	require.Equal(t, 1, proc.restartCount())

	// Brick the anchor: unreadable → the gate must fail OPEN.
	anchorPath := restartPayloadPath(cfg.toPaths().SecretsEnvPath)
	require.NoError(t, os.Chmod(anchorPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(anchorPath, 0o600) })

	post(envSecretBatch("v1"))
	assert.Equal(t, 2, proc.restartCount(), "unreadable hash must fail OPEN: restart proceeds ungated")
}

// TestApplyBatch_AnchorSurvivesStalledStageCredentials — review round-2
// missing-case pin: the unlock→gate window. Park apply A in its
// StageCredentials HTTP POST (post-applyMu.Unlock) by occupying the
// pinned opencode port with a listener that accepts and never answers;
// while A is stalled, apply B completes fully. The final anchor must be
// B's: the gate read/compare/write serialize UNDER applyMu, so A's anchor
// landed inside its locked section and nothing A does after the stall may
// touch the file. Under the pre-fix ordering (gate after StageCredentials,
// outside the lock) A wrote H1 over B's H2 and this pin went RED.
func TestApplyBatch_AnchorSurvivesStalledStageCredentials(t *testing.T) {
	withTestLogger(t)
	cfg, markerPath := newPayloadTestConfig(t)

	// Occupy the pinned opencode port: accepts, never answers — the
	// StageCredentials client times out against it.
	occupier, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort))
	if err != nil {
		t.Skipf("cannot occupy the pinned opencode port %d: %v", agentd.AgentPort, err)
	}
	t.Cleanup(func() { _ = occupier.Close() })
	go func() {
		for {
			c, err := occupier.Accept()
			if err != nil {
				return
			}
			_ = c // hold the connection open, never respond
		}
	}()

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	apply := func(batch string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/apply-batch", strings.NewReader(batch))
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

	// A: llm-provider + env-secret — StageCredentials stalls on the
	// occupied port (bounded by the client's 10s timeout).
	const aBatch = `[{"type":"llm-provider","name":"anthropic","plaintext":"{\"kind\":\"anthropic\",\"slug\":\"anthropic\",\"apiKey\":\"sk-1\"}"},{"type":"env-secret","name":"x","metadata":{"var_name":"GH_TOKEN"},"plaintext":"v1"}]`
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		apply(aBatch)
	}()

	// Wait for A's anchor (written inside its locked section).
	anchorPath := restartPayloadPath(cfg.toPaths().SecretsEnvPath)
	h1 := restartPayloadHash([]secrets.Secret{
		{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "v1"},
	})
	require.Eventually(t, func() bool {
		h, err := readRestartPayloadHash(anchorPath)
		return err == nil && h == h1
	}, 10*time.Second, 5*time.Millisecond, "A must anchor H1 inside its locked section")

	// B: completes fully while A is stalled post-Unlock.
	apply(envSecretBatch("v2"))
	h2 := restartPayloadHash([]secrets.Secret{{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "v2"}})
	stored, err := readRestartPayloadHash(anchorPath)
	require.NoError(t, err)
	require.Equal(t, h2, stored, "B's anchor must land while A is stalled")

	// A finishes its stall; the final anchor must still be B's.
	select {
	case <-aDone:
	case <-time.After(30 * time.Second):
		t.Fatal("apply A did not finish (StageCredentials stall exceeded the client timeout + margin)")
	}
	stored, err = readRestartPayloadHash(anchorPath)
	require.NoError(t, err)
	require.Equal(t, h2, stored,
		"A's post-Unlock work must not touch the anchor: only B's identity may survive")
}

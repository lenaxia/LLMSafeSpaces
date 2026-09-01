// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// resync_secrets_exec_test.go — US-70.3 Part A exec-level integration:
// the REAL handler against a REAL (mutable) bootstrap API over HTTP,
// driving the full conditional-pull → apply → anchor → restart-decision
// cycle across multiple resyncs, then replaying a container restart via
// the real materialize subcommand (container_restart_test pattern) to
// prove the resync's cache write feeds #443 replay.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mutableBootstrapAPI serves a switchable status/payload pair so one
// server can play "changed" (200 envelope) and "unchanged" (304) across
// successive resyncs of the same test.
type mutableBootstrapAPI struct {
	mu      sync.Mutex
	status  int
	payload string
}

func (a *mutableBootstrapAPI) serve(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.payload != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(a.payload))
		return
	}
	w.WriteHeader(a.status)
}

func (a *mutableBootstrapAPI) set(status int, payload string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status, a.payload = status, payload
}

func envelope(varName, value string, seq int64, manifestHash string) string {
	return `{"entries":[{"secretId":"sec-1","version":1,"type":"env-secret","name":"e","value":` + jsonStr(value) + `,"metadata":{"var_name":` + jsonStr(varName) + `}}],"revision":{"seq":` + strconv.FormatInt(seq, 10) + `,"manifestHash":` + jsonStr(manifestHash) + `,"batchHash":"bh"}}`
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestResync_ExecRound_AppliesThenNotModifiedThenEvolves: the story's
// exec-level row —
//  1. first resync against a changed manifest applies + anchors,
//  2. an unchanged manifest (304) is a strict no-op,
//  3. a new revision applies again (notify → re-pull convergence),
//
// with the env-class restart decision honored at every apply and the
// container-restart cache replay proven via the real subcommand.
func TestResync_ExecRound_AppliesThenNotModifiedThenEvolves(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	batchPath := filepath.Join(dir, "rt", "secrets.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(batchPath), 0o750))
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("exec-token"), 0o600))

	cfg := materializeConfig{
		home:             dir,
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, "ssh"),
		agentConfigPath:  filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:   filepath.Join(dir, "secrets-env"),
		gitCredsPath:     filepath.Join(dir, "git-credentials"),
		reloadCachePath:  filepath.Join(dir, "last-reload-secrets.json"),
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}
	proc := &recordingProc{}

	api := &mutableBootstrapAPI{}
	apiSrv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(apiSrv.Close)

	baseDeps := resyncDeps{
		cfg: cfg,
		reload: reloadSecretsDeps{
			Proc:             proc,
			OpencodePassword: resyncTestPassword,
		},
		apiURL:      apiSrv.URL,
		workspaceID: "ws-exec",
		tokenPath:   tokenPath,
		batchPath:   batchPath,
		minInterval: time.Nanosecond,
	}
	// Distinct handler instances share the rate budget through the deps'
	// limiter — one handler for the whole cycle keeps admission serial.
	handler := resyncSecretsHandler(baseDeps)
	post := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
		rec := httptest.NewRecorder()
		handler(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	// 1. Changed manifest: seq 6 applies, anchors, restarts (env class).
	api.set(http.StatusOK, `{"secrets":`+envelope("EXEC_VAR", "v-6", 6, "mh-6")+`}`)
	code, body := post()
	require.Equal(t, http.StatusOK, code, "first resync applies: %v", body)
	assert.Equal(t, "applied", body["status"])
	assert.Equal(t, "6:mh-6", body["appliedRev"])
	assert.Equal(t, true, body["restarted"], "env-class change → session-aware restart honored")
	require.Equal(t, int32(1), proc.restarts.Load())

	envOut, err := os.ReadFile(cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "EXEC_VAR=")

	anchor, err := readRevAnchor(revAnchorPath(cfg.secretsEnvPath))
	require.NoError(t, err)
	assert.Equal(t, "6:mh-6", anchor.Rev)
	assert.Equal(t, int64(6), anchor.AppliedSeq)

	// Container-restart replay (#443): the resync's cache write must
	// feed the boot-time materialize replay — the real subcommand run
	// over the SAME batch path keeps the applied env var.
	bin := buildAgentdBinary(t)
	exit, _, stderr := runMaterializeSubcommand(t, bin, batchPath,
		cfg.secretsBaseDir, cfg.sshDir, cfg.agentConfigPath, cfg.secretsEnvPath, cfg.gitCredsPath,
		"LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT="+batchPath)
	require.Equal(t, 0, exit, "replay boot failed; stderr=%q", stderr)
	envOut, err = os.ReadFile(cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "EXEC_VAR=",
		"the resync-applied batch survives a container restart via the reload cache")

	// 2. Unchanged manifest: 304 is a strict no-op — no apply, no
	// restart, appliedRev still the anchor's truth.
	api.set(http.StatusNotModified, "")
	code, body = post()
	require.Equal(t, http.StatusOK, code, "second resync no-ops: %v", body)
	assert.Equal(t, "not_modified", body["status"])
	assert.Equal(t, "6:mh-6", body["appliedRev"])
	assert.Equal(t, int32(1), proc.restarts.Load(), "a 304 must not restart")
	anchor, err = readRevAnchor(revAnchorPath(cfg.secretsEnvPath))
	require.NoError(t, err)
	assert.Equal(t, "6:mh-6", anchor.Rev, "the anchor is untouched by a 304")

	// 3. A new revision applies and re-anchors (revocation-as-absence
	// and rotation both ride this path).
	api.set(http.StatusOK, `{"secrets":`+envelope("EXEC_VAR", "v-7", 7, "mh-7")+`}`)
	code, body = post()
	require.Equal(t, http.StatusOK, code, "third resync converges: %v", body)
	assert.Equal(t, "applied", body["status"])
	assert.Equal(t, "7:mh-7", body["appliedRev"])
	require.Equal(t, int32(2), proc.restarts.Load())

	envOut, err = os.ReadFile(cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "EXEC_VAR=")
	assert.NotContains(t, string(envOut), "v-6", "the new revision's value replaced the old one")
	anchor, err = readRevAnchor(revAnchorPath(cfg.secretsEnvPath))
	require.NoError(t, err)
	assert.Equal(t, "7:mh-7", anchor.Rev)
}

// TestResync_ExecRound_EmptyEnvelopeRevokesByAbsence: an empty-but-
// revisioned envelope (owner unbound everything) applies as the empty
// set — absence is the delete (I12), not a stuck last-good.
func TestResync_ExecRound_EmptyEnvelopeRevokesByAbsence(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	cfg := materializeConfig{
		home:             dir,
		secretsBaseDir:   filepath.Join(dir, "secrets"),
		sshDir:           filepath.Join(dir, "ssh"),
		agentConfigPath:  filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:   filepath.Join(dir, "secrets-env"),
		gitCredsPath:     filepath.Join(dir, "git-credentials"),
		reloadCachePath:  filepath.Join(dir, "last-reload-secrets.json"),
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("t"), 0o600))
	batchPath := filepath.Join(dir, "secrets.json")

	api := &mutableBootstrapAPI{}
	apiSrv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(apiSrv.Close)

	handler := resyncSecretsHandler(resyncDeps{
		cfg:         cfg,
		reload:      reloadSecretsDeps{OpencodePassword: resyncTestPassword},
		apiURL:      apiSrv.URL,
		workspaceID: "ws-revoke",
		tokenPath:   tokenPath,
		batchPath:   batchPath,
		minInterval: time.Nanosecond,
	})
	post := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
		rec := httptest.NewRecorder()
		handler(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	api.set(http.StatusOK, `{"secrets":`+envelope("REVOKED_VAR", "present", 3, "mh-3")+`}`)
	code, body := post()
	require.Equal(t, http.StatusOK, code, "%v", body)

	empty := `{"entries":[],"revision":{"seq":4,"manifestHash":"mh-4","batchHash":"bh-4"}}`
	api.set(http.StatusOK, `{"secrets":`+empty+`}`)
	code, body = post()
	require.Equal(t, http.StatusOK, code, "%v", body)
	assert.Equal(t, "applied", body["status"])
	assert.Equal(t, "4:mh-4", body["appliedRev"])

	if got, rErr := os.ReadFile(cfg.secretsEnvPath); rErr == nil {
		assert.NotContains(t, string(got), "REVOKED_VAR",
			"the empty envelope must revoke the previously bound env secret")
	}
	// An absent secrets-env is the equally-valid revoked state (reset()
	// removed it and no entry rewrote it).

	anchor, err := readRevAnchor(revAnchorPath(cfg.secretsEnvPath))
	require.NoError(t, err)
	assert.Equal(t, "4:mh-4", anchor.Rev)
}

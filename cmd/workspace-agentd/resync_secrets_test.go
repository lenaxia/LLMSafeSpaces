// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// resync_secrets_test.go — US-70.3 Part A: the /v1/resync-secrets
// handler contract. TDD-authored before the implementation:
//
//   - Auth: the §D1 Basic pair, identical gate to reload-secrets.
//   - Method: POST only.
//   - 304 → 200 not_modified with the ANCHOR's rev; no materialize
//     (pinned by the secrets-env file staying byte-identical).
//   - 200 → envelope written verbatim, applied through the same
//     pipeline the reload handler uses, anchor written post-apply,
//     appliedRev read back from the anchor (I4).
//   - Pull failures: unreachable → 502 pull_failed; 401 → 502
//     pull_unauthorized; missing token → pull_failed. Last batch kept.
//   - The W2 apply-guard: a fetched seq ≤ the applied anchor seq is a
//     no-op (never downgrades).
//   - Rate limit (I15): a resync admitted < minInterval ago → 429.
//   - Integrity (0050 finding 3): a request body is NEVER interpreted
//     as a batch — the applied state comes from the API pull only.

import (
	"context"
	"encoding/json"
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
)

const resyncTestPassword = "resync-pw"

// recordingProc records restart() calls for the session-aware restart
// decision pins.
type recordingProc struct{ restarts atomic.Int32 }

func (p *recordingProc) restart() { p.restarts.Add(1) }

// fakeResyncClock is the deterministic rate-limiter seam.
type fakeResyncClock struct{ now atomic.Value }

func (c *fakeResyncClock) t() time.Time    { return c.now.Load().(time.Time) }
func (c *fakeResyncClock) set(t time.Time) { c.now.Store(t) }

// resyncTestEnv is the fixture bundle: temp tree + materializeConfig +
// resyncDeps wired against a configurable fake bootstrap API.
type resyncTestEnv struct {
	dir       string
	cfg       materializeConfig
	api       *conditionalServer
	apiSrv    *httptest.Server
	proc      *recordingProc
	fakeClock *fakeResyncClock
	handler   http.HandlerFunc
}

func newResyncTestEnv(t *testing.T, api *conditionalServer) *resyncTestEnv {
	t.Helper()
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
	apiSrv := httptest.NewServer(api.handler(t, nil))
	t.Cleanup(apiSrv.Close)

	clk := &fakeResyncClock{}
	clk.set(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	proc := &recordingProc{}
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("tok-v1"), 0o600))

	handler := resyncSecretsHandler(resyncDeps{
		cfg: cfg,
		reload: reloadSecretsDeps{
			Proc:             proc,
			OpencodePassword: resyncTestPassword,
		},
		apiURL:      apiSrv.URL,
		workspaceID: "ws-resync",
		tokenPath:   tokenPath,
		batchPath:   filepath.Join(dir, "secrets.json"),
		now:         clk.t,
	})
	return &resyncTestEnv{dir: dir, cfg: cfg, api: api, apiSrv: apiSrv, proc: proc, fakeClock: clk, handler: handler}
}

func (e *resyncTestEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
	rec := httptest.NewRecorder()
	e.handler(rec, req)
	return rec
}

func (e *resyncTestEnv) anchorPath() string {
	return revAnchorPath(e.cfg.secretsEnvPath)
}

func writeAnchorFile(t *testing.T, path, rev string, seq int64) {
	t.Helper()
	require.NoError(t, writeRevAnchor(path, revAnchor{Rev: rev, AppliedSeq: seq}))
}

// --- auth + method ----------------------------------------------------------

func TestResyncSecrets_RequiresAuth(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
	rec := httptest.NewRecorder()
	env.handler(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Basic realm="agentd"`, rec.Header().Get("WWW-Authenticate"))
}

func TestResyncSecrets_WrongPassword(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("wrong"))
	rec := httptest.NewRecorder()
	env.handler(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestResyncSecrets_ControlPlanePasswordAccepted(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusInternalServerError})
	handler := resyncSecretsHandler(resyncDeps{
		cfg:    env.cfg,
		reload: reloadSecretsDeps{ControlPlanePassword: "cp-pw"},
		apiURL: env.apiSrv.URL, workspaceID: "ws", tokenPath: filepath.Join(env.dir, "token"),
		batchPath: filepath.Join(env.dir, "secrets.json"), now: env.fakeClock.t,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("cp-pw"))
	rec := httptest.NewRecorder()
	handler(rec, req)
	require.Equal(t, http.StatusBadGateway, rec.Code,
		"the §D1 control-plane credential must gate this endpoint (reaches the pull, fails on the 500)")
}

func TestResyncSecrets_MethodMatrix(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/resync-secrets", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
		rec := httptest.NewRecorder()
		env.handler(rec, req)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
	}
}

// --- 304: not_modified, no apply --------------------------------------------

func TestResyncSecrets_304_NotModified(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified, etag: `"5:mh-5"`})
	batchPath := filepath.Join(env.dir, "secrets.json")
	require.NoError(t, os.WriteFile(batchPath, []byte(testEnvelope), 0o600))
	writeAnchorFile(t, env.anchorPath(), "5:mh-5", 5)
	envFile := filepath.Join(env.dir, "secrets-env")
	require.NoError(t, os.WriteFile(envFile, []byte("export PRIOR='state'\n"), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "not_modified", resp.Status)
	assert.Equal(t, "5:mh-5", resp.AppliedRev, "appliedRev comes from the rev anchor")

	got, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.Equal(t, "export PRIOR='state'\n", string(got),
		"304 must not materialize — the materialized state is untouched")
	batch, err := os.ReadFile(batchPath)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(batch), "304 keeps the prior batch byte-for-byte")
}

func TestResyncSecrets_304_NoAnchor_EmptyAppliedRev(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	legacy := `[{"type":"env-secret","name":"e","plaintext":"p"}]`
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "secrets.json"), []byte(legacy), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "not_modified", resp.Status)
	assert.Empty(t, resp.AppliedRev, "a legacy (unanchored) state has no applied rev to report")
}

// --- 200: applied through the reload pipeline --------------------------------

func TestResyncSecrets_200_AppliesAndAnchors(t *testing.T) {
	envelope := `{"entries":[{"secretId":"sec-1","version":4,"type":"env-secret","name":"db","value":"v-6","metadata":{"var_name":"RESYNC_DB"}}],"revision":{"seq":6,"manifestHash":"mh-6","batchHash":"bh-6"}}`
	env := newResyncTestEnv(t, &conditionalServer{payload: `{"secrets":` + envelope + `}`})

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
		Restarted  bool   `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "applied", resp.Status)
	assert.Equal(t, "6:mh-6", resp.AppliedRev)
	assert.True(t, resp.Restarted, "an env-class change honors the #852 session-aware restart semantics")

	anchor, err := readRevAnchor(env.anchorPath())
	require.NoError(t, err)
	assert.Equal(t, "6:mh-6", anchor.Rev, "I4: appliedRev is the anchor the apply produced")
	assert.Equal(t, int64(6), anchor.AppliedSeq)

	envOut, err := os.ReadFile(env.cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "export RESYNC_DB=")

	batch, err := os.ReadFile(filepath.Join(env.dir, "secrets.json"))
	require.NoError(t, err)
	assert.Equal(t, envelope, string(batch), "the envelope is written verbatim")

	cache, err := os.ReadFile(env.cfg.reloadCachePath)
	require.NoError(t, err)
	assert.Contains(t, string(cache), "RESYNC_DB",
		"the applied batch is persisted to the reload cache (container-restart replay, #443)")
}

func TestResyncSecrets_200_LLMOnly_NoRestart(t *testing.T) {
	envelope := `{"entries":[{"secretId":"sec-2","version":2,"type":"llm-provider","name":"anthropic","value":"{\"kind\":\"anthropic\",\"slug\":\"anthropic\",\"apiKey\":\"sk-6\"}"}],"revision":{"seq":7,"manifestHash":"mh-7","batchHash":"bh-7"}}`
	env := newResyncTestEnv(t, &conditionalServer{payload: `{"secrets":` + envelope + `}`})

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status     string `json:"status"`
		Restarted  bool   `json:"restarted"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "applied", resp.Status)
	assert.Equal(t, "7:mh-7", resp.AppliedRev)
	assert.False(t, resp.Restarted, "llm-provider-only changes do not force a restart (same semantics as reload-secrets)")
	assert.Equal(t, int32(0), env.proc.restarts.Load())
}

// --- pull failures -----------------------------------------------------------

func TestResyncSecrets_PullUnreachable_502_LastGoodKept(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	withTestLogger(t)
	dir := t.TempDir()
	cfg := materializeConfig{home: dir, secretsEnvPath: filepath.Join(dir, "secrets-env")}
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("t"), 0o600))
	batchPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(batchPath, []byte(testEnvelope), 0o600))

	handler := resyncSecretsHandler(resyncDeps{
		cfg: cfg, reload: reloadSecretsDeps{OpencodePassword: resyncTestPassword},
		apiURL: closed.URL, workspaceID: "ws", tokenPath: token, batchPath: batchPath,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
	rec := httptest.NewRecorder()
	handler(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	var resp struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "failed", resp.Status)
	assert.Equal(t, "pull_failed", resp.Reason)

	got, err := os.ReadFile(batchPath)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(got), "a failed pull keeps the last-good batch")
}

func TestResyncSecrets_PullUnauthorized_502(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusUnauthorized})
	rec := env.post(t, "")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	var resp struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "failed", resp.Status)
	assert.Equal(t, "pull_unauthorized", resp.Reason)
}

func TestResyncSecrets_TokenUnreadable_502(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	require.NoError(t, os.Remove(filepath.Join(env.dir, "token")))
	rec := env.post(t, "")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	var resp struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "failed", resp.Status)
	assert.Equal(t, "pull_failed", resp.Reason)
}

// --- conditional-pull wire shape ----------------------------------------------

func TestResyncSecrets_PresentsPriorEnvelopeHashAndFreshToken(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{payload: `{"secrets":[]}`})
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "secrets.json"), []byte(testEnvelope), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	req := lastBootstrapRequest(t, env.api)
	assert.Equal(t, 2, req.ContractVersion)
	assert.Equal(t, "mh-5", req.ClientManifestHash, "the prior envelope's manifest hash is presented")
	assert.Equal(t, "ws-resync", req.WorkspaceID)
	assert.Equal(t, "Bearer tok-v1", env.api.lastAuth.Load())
}

func TestResyncSecrets_TokenReadFreshPerPull(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	tokenPath := filepath.Join(env.dir, "token")

	require.Equal(t, http.StatusOK, env.post(t, "").Code)
	require.NoError(t, os.WriteFile(tokenPath, []byte("tok-v2-rotated"), 0o600))
	env.fakeClock.set(env.fakeClock.t().Add(time.Hour))
	require.Equal(t, http.StatusOK, env.post(t, "").Code)

	assert.Equal(t, "Bearer tok-v2-rotated", env.api.lastAuth.Load(),
		"the SA token is read fresh from disk on every pull (AC-14: kubelet rotates it in place)")
}

// --- W2 apply-guard -----------------------------------------------------------

func TestResyncSecrets_304_CrashWindow_HealsUnappliedEnvelope(t *testing.T) {
	// Crash between the envelope write and the apply: the file holds seq 6
	// (presented → the API 304s) but only seq 5 was ever APPLIED. A plain
	// not_modified here is a liveness hole — the batch would never apply
	// until a container restart. The resync must detect file-seq >
	// applied-seq and apply the on-disk envelope.
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified, etag: `"6:mh-6"`})
	fresh := `{"entries":[{"secretId":"sec-1","version":4,"type":"env-secret","name":"db","value":"v-6","metadata":{"var_name":"DB"}}],"revision":{"seq":6,"manifestHash":"mh-6","batchHash":"bh-6"}}`
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "secrets.json"), []byte(fresh), 0o600))
	writeAnchorFile(t, env.anchorPath(), "5:mh-5", 5)
	require.NoError(t, os.WriteFile(env.cfg.secretsEnvPath, []byte("export OLD='v5'\n"), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
		Restarted  bool   `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "applied", resp.Status, "the unapplied on-disk envelope is applied on 304 (crash-window heal)")
	assert.Equal(t, "6:mh-6", resp.AppliedRev)

	envOut, err := os.ReadFile(env.cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "DB=", "the on-disk envelope's entry materialized")
	anchor, err := readRevAnchor(env.anchorPath())
	require.NoError(t, err)
	assert.Equal(t, "6:mh-6", anchor.Rev)
}

func TestResyncSecrets_304_NoAnchor_DoesNotRevertPushState(t *testing.T) {
	// Mixed-fleet window: a legacy PUSH landed after the last revisioned
	// pull (the anchor was invalidated). A 304 must NOT apply the stale
	// on-disk envelope over the pushed live state.
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "secrets.json"), []byte(testEnvelope), 0o600))
	require.NoError(t, os.WriteFile(env.cfg.secretsEnvPath, []byte("export PUSHED='live-state'\n"), 0o600))
	require.NoError(t, os.WriteFile(env.cfg.reloadCachePath, []byte(`[{"type":"env-secret","name":"p","metadata":{"var_name":"PUSHED"},"plaintext":"live-state"}]`), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "not_modified", resp.Status, "an absent anchor means the live state is not the envelope — no heal")

	envOut, err := os.ReadFile(env.cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Equal(t, "export PUSHED='live-state'\n", string(envOut),
		"the pushed live state must survive a 304 resync")
}

func TestResyncSecrets_ApplyGuard_StaleSeqIsNoOp(t *testing.T) {
	stale := `{"entries":[{"secretId":"sec-1","version":3,"type":"env-secret","name":"db","value":"stale","metadata":{"var_name":"OLD"}}],"revision":{"seq":4,"manifestHash":"mh-4","batchHash":"bh-4"}}`
	env := newResyncTestEnv(t, &conditionalServer{payload: `{"secrets":` + stale + `}`})
	batchPath := filepath.Join(env.dir, "secrets.json")
	require.NoError(t, os.WriteFile(batchPath, []byte(testEnvelope), 0o600))
	writeAnchorFile(t, env.anchorPath(), "7:mh-7", 7)
	require.NoError(t, os.WriteFile(env.cfg.secretsEnvPath, []byte("export CURRENT='applied'\n"), 0o600))

	rec := env.post(t, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "not_modified", resp.Status, "a fetched seq ≤ the applied seq never downgrades")
	assert.Equal(t, "7:mh-7", resp.AppliedRev)

	got, err := os.ReadFile(env.cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Equal(t, "export CURRENT='applied'\n", string(got), "no materialize ran")
	batch, err := os.ReadFile(batchPath)
	require.NoError(t, err)
	assert.Equal(t, testEnvelope, string(batch), "the stale envelope is not persisted over the applied one")
}

// --- rate limiting (I15) --------------------------------------------------------

func TestResyncSecrets_RateLimited(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	handler := resyncSecretsHandler(resyncDeps{
		cfg:    env.cfg,
		reload: reloadSecretsDeps{OpencodePassword: resyncTestPassword},
		apiURL: env.apiSrv.URL, workspaceID: "ws",
		tokenPath: filepath.Join(env.dir, "token"), batchPath: filepath.Join(env.dir, "secrets.json"),
		now: env.fakeClock.t,
	})

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", nil)
		req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, post().Code)
	second := post()
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Contains(t, second.Body.String(), `"rate_limited"`)
	// US-70.3 PR-4: the refusal carries the remaining floor so the
	// secrets_resync MCP tool can report a real retryAfterMs (the fake
	// clock is at the admission instant, so the whole floor remains).
	var rl struct {
		Status       string `json:"status"`
		RetryAfterMs int64  `json:"retryAfterMs"`
	}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &rl))
	assert.Equal(t, "rate_limited", rl.Status)
	assert.Equal(t, resyncDefaultMinInterval.Milliseconds(), rl.RetryAfterMs)

	env.fakeClock.set(env.fakeClock.t().Add(resyncDefaultMinInterval + time.Second))
	require.Equal(t, http.StatusOK, post().Code, "admission resumes after the interval elapses")
}

func TestResyncSecrets_RateLimit_EnvOverride(t *testing.T) {
	t.Setenv(resyncMinIntervalEnv, "250ms")
	assert.Equal(t, 250*time.Millisecond, resyncMinIntervalFromEnv())
	t.Setenv(resyncMinIntervalEnv, "not-a-duration")
	assert.Equal(t, resyncDefaultMinInterval, resyncMinIntervalFromEnv(),
		"an unparseable value falls back to the default, never to zero (unguarded)")
}

// --- integrity: a body never influences the applied batch ----------------------

func TestResyncSecrets_RequestBodyNeverInfluencesAppliedBatch(t *testing.T) {
	envelope := `{"entries":[{"secretId":"sec-1","version":1,"type":"env-secret","name":"real","value":"api-value","metadata":{"var_name":"FROM_API"}}],"revision":{"seq":9,"manifestHash":"mh-9","batchHash":"bh-9"}}`
	env := newResyncTestEnv(t, &conditionalServer{payload: `{"secrets":` + envelope + `}`})

	hostile := `[{"type":"env-secret","name":"smuggled","metadata":{"var_name":"SMUGGLED"},"plaintext":"caller-supplied"}]`
	rec := env.post(t, hostile)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	envOut, err := os.ReadFile(env.cfg.secretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envOut), "FROM_API=")
	assert.NotContains(t, string(envOut), "SMUGGLED",
		"0050 finding-3: the endpoint can never be talked into applying caller-supplied secrets")
	batch, err := os.ReadFile(filepath.Join(env.dir, "secrets.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(batch), "smuggled")
}

// --- env-coordinate helpers ------------------------------------------------------

func TestResyncSecrets_BatchPathHelpers(t *testing.T) {
	assert.Equal(t, "/sandbox-cfg/secrets.json", bootstrapSecretsOutFromEnv())
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT", "/custom/secrets.json")
	assert.Equal(t, "/custom/secrets.json", bootstrapSecretsOutFromEnv())

	assert.Equal(t, bootstrapTokenPath, bootstrapTokenPathFromEnv())
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", "/custom/token")
	assert.Equal(t, "/custom/token", bootstrapTokenPathFromEnv())
}

// --- mux registration -------------------------------------------------------------

func TestBuildUserMux_RegistersResyncEndpoint(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	api := &conditionalServer{status: http.StatusNotModified}
	apiSrv := httptest.NewServer(api.handler(t, nil))
	defer apiSrv.Close()

	t.Setenv("LLMSAFESPACE_API_URL", apiSrv.URL)
	t.Setenv("WORKSPACE_ID", "ws-mux")
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", filepath.Join(dir, "token"))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT", filepath.Join(dir, "secrets.json"))
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", filepath.Join(dir, "secrets-env"))
	t.Setenv("LLMSAFESPACES_AGENT_CONFIG_PATH", filepath.Join(dir, "agent-config.json"))
	t.Setenv("LLMSAFESPACES_SSH_DIR", filepath.Join(dir, "ssh"))
	t.Setenv("LLMSAFESPACES_SECRETS_BASE_DIR", filepath.Join(dir, "secrets"))
	t.Setenv("LLMSAFESPACES_GIT_CREDS_PATH", filepath.Join(dir, "git-credentials"))
	t.Setenv("LLMSAFESPACES_RELOAD_CACHE_PATH", filepath.Join(dir, "last-reload-secrets.json"))
	t.Setenv("LLMSAFESPACES_ENRICHER_CACHE_DIR", filepath.Join(dir, "enricher"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte("mux-tok"), 0o600))

	mux := buildUserMux(context.Background(), nil, serverDeps{
		password:             "pw",
		controlPlanePassword: "cp",
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/resync-secrets", nil)
	req.SetBasicAuth("opencode", "cp")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "not_modified", got.Status, "the mux-wired handler performs the real conditional pull")
}

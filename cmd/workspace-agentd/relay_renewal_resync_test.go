// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_renewal_resync_test.go — US-72.4 review amendment (#1529,
// required item 5): the story AC "token renewal applies behind busy
// sessions (no SSE drops)" pinned for the RELAY batch through the REAL
// conditional-pull resync path (resyncSecretsHandler → applySecretsBatch),
// not just the monitor-side pickup.
//
// As-landed semantics being pinned (verified against shouldRestart):
// an llm-provider-only renewal batch is NOT restart-worthy — the token
// lands on disk (agent-config via the ConfigWriter + the batch file
// verbatim) with ZERO restarts, which makes "behind busy sessions, no
// SSE drops" hold trivially; and when the renewal rides a batch that
// ALSO carries a restart-worthy class (env-secret), the #852 deferral
// machinery governs exactly as before — applied when the turn ends.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// switchingAPI serves the bootstrap envelope from a swappable payload
// (old token batch → renewed token batch).
type switchingAPI struct {
	mu      sync.Mutex
	payload string
}

func (s *switchingAPI) set(payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload = payload
}

func (s *switchingAPI) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var req struct {
			WorkspaceID     string `json:"workspaceID"`
			ContractVersion int    `json:"contractVersion"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(s.payload))
	}
}

// relayEnvelopeFor renders a v2 envelope whose single llm-provider
// entry is the relay token entry the US-72.4 builder emits (metadata
// included — the shape the liveness registry reads off the batch file).
// The entry value is the double-encoded LLMProviderData STRING (the
// v2 envelope's Value contract).
func relayEnvelopeFor(t *testing.T, seq int64, token, revision, expiresAt string) string {
	t.Helper()
	pd, err := json.Marshal(map[string]any{
		"kind": "openai", "slug": "openai", "apiKey": token,
		"baseURL": "http://llm-relay-router.llm-relay.svc.cluster.local/w/ws-rn/openai/v1",
		"models":  []map[string]any{{"id": "gpt-4o"}},
	})
	require.NoError(t, err)
	entry := map[string]any{
		"secretId": "cred-openai",
		"version":  1,
		"type":     "llm-provider",
		"name":     "openai",
		"value":    string(pd),
		"metadata": map[string]string{
			"relay":          "true",
			"relayExpiresAt": expiresAt,
			"relayRevision":  revision,
		},
	}
	envelope := map[string]any{
		"entries": []any{entry},
		"revision": map[string]any{
			"seq": seq, "manifestHash": "mh-" + revision, "batchHash": "bh-" + revision,
		},
	}
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)
	return `{"secrets":` + string(raw) + `}`
}

// withEnvEntry appends a restart-worthy env-secret entry to an envelope
// payload string (re-encoded through the map form for simplicity).
func withEnvEntry(t *testing.T, payload, varName string) string {
	t.Helper()
	var wrap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(payload), &wrap))
	var batch map[string]any
	require.NoError(t, json.Unmarshal(wrap["secrets"], &batch))
	entries := batch["entries"].([]any)
	entries = append(entries, map[string]any{
		"secretId": "sec-env", "version": 1, "type": "env-secret", "name": varName,
		"value": "v-1", "metadata": map[string]string{"var_name": varName},
	})
	batch["entries"] = entries
	raw, err := json.Marshal(map[string]any{"secrets": batch})
	require.NoError(t, err)
	return string(raw)
}

// renewalEnv is the resync fixture with the full #852 apply machinery:
// real tracker, streaming busy session, pending surface, real
// ConfigWriter, recording proc.
type renewalEnv struct {
	dir     string
	api     *switchingAPI
	handler http.HandlerFunc
	proc    *recordingProc
	pending *pendingApplyTracker
	cfg     materializeConfig
	tracker *sessionStatusTracker
}

func newRenewalEnv(t *testing.T, initialPayload string) *renewalEnv {
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
		enricherCacheDir: filepath.Join(dir, "enricher"),
	}
	api := &switchingAPI{payload: initialPayload}
	apiSrv := httptest.NewServer(api.handler())
	t.Cleanup(apiSrv.Close)

	proc := &recordingProc{}
	pending := newPendingApplyTracker()
	tracker := newSessionStatusTracker()
	bgCtx, bgCancel := context.WithCancel(context.Background())
	t.Cleanup(bgCancel)

	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("tok-rn"), 0o600))

	handler := resyncSecretsHandler(resyncDeps{
		cfg: cfg,
		apply: applySecretsDeps{
			Proc:                    proc,
			OpencodePassword:        resyncTestPassword,
			Tracker:                 tracker,
			BgCtx:                   bgCtx,
			Lister:                  func(context.Context) []string { return nil },
			PendingApply:            pending,
			RestartReasonMarkerPath: filepath.Join(dir, "restart-reason"),
			AgentConfigWriter:       opencode.NewConfigWriter(filepath.Join(dir, "agent-config.json")),
		},
		apiURL:      apiSrv.URL,
		workspaceID: "ws-rn",
		tokenPath:   tokenPath,
		batchPath:   filepath.Join(dir, "secrets.json"),
		// Collapse the I15 rate-limit floor so back-to-back pulls (old
		// batch then renewal) don't 429 in-test.
		minInterval: time.Millisecond,
	})
	return &renewalEnv{dir: dir, api: api, handler: handler, proc: proc, pending: pending, cfg: cfg, tracker: tracker}
}

func (e *renewalEnv) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/resync-secrets", strings.NewReader(""))
	req.Header.Set("Authorization", "Basic "+basicAuth(resyncTestPassword))
	rec := httptest.NewRecorder()
	e.handler(rec, req)
	return rec
}

// TestRelayRenewal_TokenOnlyBatch_AppliesBehindBusySession_NoRestartNoDrops:
// a renewed relay batch (fresh token, new staged revision → rotated
// manifest → 200) lands while a turn STREAMS: zero restarts, zero
// interrupts, nothing pending — and the renewed token + relay metadata
// are on disk (agent-config via the ConfigWriter, batch file verbatim).
func TestRelayRenewal_TokenOnlyBatch_AppliesBehindBusySession_NoRestartNoDrops(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	env := newRenewalEnv(t, relayEnvelopeFor(t, 10, "lrt_fakeOLD0000000000", "rOLD7777", exp))

	// The old batch is the working state.
	rec := env.post(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A long turn starts streaming; the renewal (manifest rotated by the
	// staged revision) arrives through the conditional pull.
	stop := streamingTurn(env.tracker, "ses_turn", 30*time.Millisecond)
	defer stop()
	env.api.set(relayEnvelopeFor(t, 11, "lrt_fakeNEW0000000000", "rNEW8888", exp))

	rec = env.post(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Status     string `json:"status"`
		Restarted  bool   `json:"restarted"`
		AppliedRev string `json:"appliedRev"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "applied", resp.Status)
	assert.False(t, resp.Restarted, "an llm-provider-only renewal is not restart-worthy (shouldRestart) — it applies live, which is strictly better than the #852 deferral")
	assert.Equal(t, "11:mh-rNEW8888", resp.AppliedRev, "the anchor advanced to the renewed revision")

	// No SSE drops: nothing restarted, nothing interrupted, nothing pending.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(0), env.proc.restarts.Load(),
		"an llm-provider-only renewal never restarts — zero across BOTH applies (pre-stream and mid-stream)")
	assert.Nil(t, env.pending.snapshot(), "nothing deferred — nothing surfaces as pending")

	// The renewed TOKEN is on disk: agent-config (the ConfigWriter seam)
	// and the batch file (verbatim, metadata included — the liveness
	// registry's source).
	cfgRaw, err := os.ReadFile(env.cfg.agentConfigPath)
	require.NoError(t, err)
	assert.Contains(t, string(cfgRaw), "lrt_fakeNEW0000000000", "the renewed token is in agent-config.json")
	assert.NotContains(t, string(cfgRaw), "lrt_fakeOLD0000000000", "the old token is gone")

	batchRaw, err := os.ReadFile(filepath.Join(env.dir, "secrets.json"))
	require.NoError(t, err)
	assert.Contains(t, string(batchRaw), "lrt_fakeNEW0000000000")
	assert.Contains(t, string(batchRaw), `"relayRevision":"rNEW8888"`)

	// The liveness registry (US-72.4's own consumer) picks the renewed
	// batch up from the durable file — no restart anywhere in the loop.
	mon := newRelayLivenessMonitor(filepath.Join(env.dir, "secrets.json"), &http.Client{Timeout: 500 * time.Millisecond})
	mon.registry.rescan()
	refs, revision := mon.registry.snapshot()
	require.Len(t, refs, 1)
	assert.Equal(t, "lrt_fakeNEW0000000000", refs[0].Token)
	assert.Equal(t, "rNEW8888", revision)
}

// TestRelayRenewal_WithRestartWorthyClass_Rides852Deferral: when the
// renewal arrives in a batch that ALSO carries a restart-worthy class
// (env-secret), the #852 machinery governs: NO restart and NO interrupt
// while the turn streams; the restart applies when the session goes
// idle — with the renewed relay batch aboard.
func TestRelayRenewal_WithRestartWorthyClass_Rides852Deferral(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	env := newRenewalEnv(t, relayEnvelopeFor(t, 20, "lrt_fakeOLD0000000000", "rOLD2020", exp))
	rec := env.post(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	baseRestarts := env.proc.restarts.Load()

	stop := streamingTurn(env.tracker, "ses_turn2", 30*time.Millisecond)
	defer stop()
	env.api.set(withEnvEntry(t, relayEnvelopeFor(t, 21, "lrt_fakeNEW0000000000", "rNEW2121", exp), "RELAY_DB"))

	rec = env.post(t)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Status    string `json:"status"`
		Restarted bool   `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "applied", resp.Status)
	assert.False(t, resp.Restarted, "busy session — the #852 deferral holds with the relay renewal aboard")

	// Streaming: deferred, no interrupt.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, baseRestarts, env.proc.restarts.Load(), "no restart while the turn streams")
	require.Eventually(t, func() bool { return env.pending.snapshot() != nil },
		2*time.Second, 10*time.Millisecond, "the deferred apply surfaces (relay renewal + env change)")

	// The relay half is ALREADY applied to disk — only the restart is deferred.
	cfgRaw, err := os.ReadFile(env.cfg.agentConfigPath)
	require.NoError(t, err)
	assert.Contains(t, string(cfgRaw), "lrt_fakeNEW0000000000")

	// The turn ends → idle → the deferred restart applies → pending clears
	// (the production 5s poll tick bounds this).
	stop()
	env.tracker.set("ses_turn2", "idle")
	require.Eventually(t, func() bool { return env.proc.restarts.Load() > baseRestarts },
		15*time.Second, 50*time.Millisecond, "the deferred restart applies once idle")
	require.Eventually(t, func() bool { return env.pending.snapshot() == nil },
		2*time.Second, 10*time.Millisecond, "applied restart clears the pending surface")
}

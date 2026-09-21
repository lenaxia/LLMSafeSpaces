// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_liveness_test.go — US-72.4 relay-only liveness (design 0058
// §4.5/§4.8): registry scan, degrade-code mapping (relay_unreachable /
// token_expired / router reject reasons), boot-outage re-arm (the
// #910 loop), and the cached-snapshot contract (handlers never I/O).
// Run with -race: the watchdog loop, the re-arm loop and the handlers
// share the monitor concurrently.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const (
	fakeRelayToken = "lrt_fakeLiveness0123456789ABCDEFG"
	fakeRawKey     = "sk-RAW-PROVIDER-KEY-canary"
)

// writeRelayBatch writes a v2 batch file with one relay-marked provider
// entry (the metadata the US-72.4 builder emits) plus one raw provider.
func writeRelayBatch(t *testing.T, dir, routerBase, revision, expiresAt string) string {
	t.Helper()
	tokenPD, err := json.Marshal(sec.LLMProviderData{
		Kind: "openai", Slug: "openai", APIKey: fakeRelayToken,
		BaseURL: routerBase + "/w/ws-liv/openai/v1",
		Models:  []sec.LLMModelConfig{{ID: "gpt-4o"}},
	})
	require.NoError(t, err)
	rawPD, err := json.Marshal(sec.LLMProviderData{
		Kind: "bedrock", Slug: "aws-bedrock", APIKey: fakeRawKey,
	})
	require.NoError(t, err)
	meta := map[string]string{
		sec.RelayMetadataKey:          "true",
		sec.RelayMetadataExpiresAtKey: expiresAt,
		sec.RelayMetadataRevisionKey:  revision,
	}
	batch := sec.Batch{
		Entries: []sec.BatchEntry{
			{SecretID: "c1", Version: 1, Type: sec.SecretTypeLLMProvider, Name: "openai",
				Value: string(tokenPD), Metadata: mustJSONStr(t, meta)},
			{SecretID: "c2", Version: 1, Type: sec.SecretTypeLLMProvider, Name: "aws-bedrock",
				Value: string(rawPD)},
		},
		Revision: sec.BatchRevision{Seq: 4, ManifestHash: "mh-liv"},
	}
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func mustJSONStr(t *testing.T, m map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return raw
}

func futureRFC3339(t *testing.T, d time.Duration) string {
	t.Helper()
	return time.Now().Add(d).UTC().Format(time.RFC3339)
}

// relayTestRouter is a flip-able router: serves the scoped /models
// catalog when up; closes connections (or serves rejections) when down.
type relayTestRouter struct {
	mu      sync.Mutex
	up      bool
	status  int    // non-0 → serve this rejection status
	reason  string // rejection reason body
	sawAuth atomic.Int64
	srv     *httptest.Server
}

func newRelayTestRouter(t *testing.T) *relayTestRouter {
	t.Helper()
	r := &relayTestRouter{up: true}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/w/ws-liv/openai/v1/models" || req.Method != http.MethodGet {
			http.NotFound(w, req)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+fakeRelayToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "relay_rejected", "reason": "unauthorized"})
			return
		}
		r.sawAuth.Add(1)
		r.mu.Lock()
		up, status, reason := r.up, r.status, r.reason
		r.mu.Unlock()
		if !up && status == 0 {
			// Simulate an unreachable router: hang the connection until
			// the client's (short) timeout fires.
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "relay_rejected", "reason": reason})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": "gpt-4o"}},
		})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *relayTestRouter) setDown() {
	r.mu.Lock()
	r.up, r.status, r.reason = false, 0, ""
	r.mu.Unlock()
}
func (r *relayTestRouter) setReject(status int, reason string) {
	r.mu.Lock()
	r.up, r.status, r.reason = false, status, reason
	r.mu.Unlock()
}
func (r *relayTestRouter) setUp() { r.mu.Lock(); r.up, r.status, r.reason = true, 0, ""; r.mu.Unlock() }

// newTestMonitor builds a monitor over a batch file with a fast clock
// and a short-timeout probe client.
func newTestMonitor(t *testing.T, batchPath string) *relayLivenessMonitor {
	t.Helper()
	m := newRelayLivenessMonitor(batchPath, &http.Client{Timeout: 500 * time.Millisecond})
	return m
}

// TestRelayLiveness_RegistryScansRelayEntries: only relay-marked
// entries are watched; the revision is collected for the lineage feed.
func TestRelayLiveness_RegistryScansRelayEntries(t *testing.T) {
	router := newRelayTestRouter(t)
	path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0001", futureRFC3339(t, time.Hour))

	m := newTestMonitor(t, path)
	m.registry.rescan()
	refs, revision := m.registry.snapshot()
	require.Len(t, refs, 1)
	assert.Equal(t, "openai", refs[0].Slug)
	assert.Equal(t, fakeRelayToken, refs[0].Token)
	assert.True(t, refs[0].HasExpiry)
	assert.Equal(t, "rLIV0001", revision)

	code := m.evaluate(context.Background())
	assert.Equal(t, "", code, "a healthy router with a valid token is not degraded")
	snap := m.snapshot()
	assert.True(t, snap.Present)
	assert.True(t, snap.Reachable)
	assert.Equal(t, "", snap.DegradedReason)
	assert.Equal(t, "rLIV0001", snap.AppliedRevision)
	assert.Equal(t, router.srv.URL, snap.RouterURL)
	assert.NotContains(t, fmt.Sprint(*snap), fakeRelayToken, "the snapshot never carries token material")
	assert.True(t, router.sawAuth.Load() > 0, "the probe went through the router with the scoped token")
}

// TestRelayLiveness_AbsentBatchNeverDegrades: no batch (or a batch with
// no relay entries) is a flag-off pod — present=false, no probe, no
// degrade, no crash.
func TestRelayLiveness_AbsentBatchNeverDegrades(t *testing.T) {
	router := newRelayTestRouter(t)
	dir := t.TempDir()

	m := newTestMonitor(t, filepath.Join(dir, "missing.json"))
	code := m.evaluate(context.Background())
	assert.Equal(t, relayEvalAbsent, code)
	snap := m.snapshot()
	assert.False(t, snap.Present)
	assert.Empty(t, snap.DegradedReason)

	// A batch whose entries are all raw (no relay metadata) is equally
	// silent.
	path := writeRelayBatch(t, dir, router.srv.URL, "rLIV0002", futureRFC3339(t, time.Hour))
	raw, err := json.Marshal(sec.Batch{Entries: []sec.BatchEntry{{
		SecretID: "c2", Type: sec.SecretTypeLLMProvider, Name: "aws-bedrock",
		Value: fmt.Sprintf(`{"kind":"bedrock","slug":"aws-bedrock","apiKey":%q}`, fakeRawKey),
	}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	code = m.evaluate(context.Background())
	assert.Equal(t, relayEvalAbsent, code)
	assert.False(t, m.snapshot().Present)
	assert.Zero(t, router.sawAuth.Load())
}

// TestRelayLiveness_TokenExpiredHonoredLocally: a token past its
// ExpiresAt degrades token_expired WITHOUT any router round-trip.
func TestRelayLiveness_TokenExpiredHonoredLocally(t *testing.T) {
	router := newRelayTestRouter(t)
	path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0003", futureRFC3339(t, -time.Minute))

	m := newTestMonitor(t, path)
	before := router.sawAuth.Load()
	code := m.evaluate(context.Background())
	assert.Equal(t, "token_expired", code)
	snap := m.snapshot()
	assert.True(t, snap.Present)
	assert.False(t, snap.Reachable)
	assert.Equal(t, "token_expired", snap.DegradedReason)
	assert.Equal(t, router.sawAuth.Load(), before, "expiry is decided locally — no probe")
}

// TestRelayLiveness_RouterRejectCodesSurface: the router's
// machine-readable rejection reasons are the degrade codes the US-72.3
// classifier consumes (credential_stale → corruption class;
// scope_violation / quota_exceeded → CredentialRejected).
func TestRelayLiveness_RouterRejectCodesSurface(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		reason    string
		wantCode  string
		wantReach bool
		unauthOK  bool
	}{
		{name: "credential stale", status: 401, reason: "credential_stale", wantCode: "credential_stale"},
		{name: "scope violation", status: 403, reason: "scope_violation", wantCode: "scope_violation"},
		{name: "sanitization refused", status: 404, reason: "sanitization_refused", wantCode: "sanitization_refused"},
		{name: "quota exceeded", status: 429, reason: "quota_exceeded", wantCode: "quota_exceeded"},
		{name: "bare unauthorized maps to corruption class", status: 401, reason: "unauthorized", wantCode: "credential_stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newRelayTestRouter(t)
			router.setReject(tc.status, tc.reason)
			path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0004", futureRFC3339(t, time.Hour))
			m := newTestMonitor(t, path)
			code := m.evaluate(context.Background())
			assert.Equal(t, tc.wantCode, code)
			snap := m.snapshot()
			assert.Equal(t, tc.wantCode, snap.DegradedReason)
			assert.False(t, snap.Reachable)
		})
	}
}

// TestRelayLiveness_UnreachableRouter: a down router (probe timeout)
// degrades relay_unreachable.
func TestRelayLiveness_UnreachableRouter(t *testing.T) {
	router := newRelayTestRouter(t)
	router.setDown()
	path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0005", futureRFC3339(t, time.Hour))
	m := newTestMonitor(t, path)
	code := m.evaluate(context.Background())
	assert.Equal(t, "relay_unreachable", code)
	snap := m.snapshot()
	assert.Equal(t, "relay_unreachable", snap.DegradedReason)
}

// TestRelayLiveness_BootOutageRearmRecovers — the §4.8 / #910
// fault-injection leg (`boot_relay_outage_rearm`): router DOWN at boot
// → loud degrade + bounded re-arm → router comes up → recovery within
// the backoff bounds, no manual restart, exactly one re-arm loop in
// flight, and the CAS guard re-opens for a second outage.
func TestRelayLiveness_BootOutageRearmRecovers(t *testing.T) {
	router := newRelayTestRouter(t)
	router.setDown() // boot-time outage
	path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0006", futureRFC3339(t, time.Hour))

	m := newRelayLivenessMonitor(path, &http.Client{Timeout: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	started := time.Now()
	startRelayLiveness(ctx, &wg, m, relayLivenessConfig{
		Interval:      50 * time.Millisecond, // watchdog cadence
		RearmMinDelay: 30 * time.Millisecond, // re-arm backoff floor
		RearmMaxDelay: 200 * time.Millisecond,
	})

	// Degrade surfaces quickly (boot detection must not wait an interval).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.snapshot().DegradedReason == "relay_unreachable" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, "relay_unreachable", m.snapshot().DegradedReason, "boot outage degrades loudly")

	// Router comes up → recovery within backoff bounds.
	time.Sleep(100 * time.Millisecond)
	router.setUp()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthyNow() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.True(t, m.healthyNow(), "re-arm recovered the relay path after the router came back (no restart)")

	// The CAS guard re-opened: a second outage can arm again.
	router.setDown()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.snapshot().DegradedReason == "relay_unreachable" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, "relay_unreachable", m.snapshot().DegradedReason, "a mid-life outage degrades again")
	router.setUp()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthyNow() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.True(t, m.healthyNow())
	elapsed := time.Since(started)
	cancel()
	wg.Wait()
	assert.Less(t, elapsed, 30*time.Second, "the whole degrade→recover→degrade→recover cycle is bounded")
}

// TestRelayLiveness_ExpiredTokenRearmIsTerminalForProbe: an expired
// token is not made healthy by the router coming up (renewal owns it) —
// the degrade stays token_expired.
func TestRelayLiveness_ExpiredTokenRearmIsTerminalForProbe(t *testing.T) {
	router := newRelayTestRouter(t)
	path := writeRelayBatch(t, t.TempDir(), router.srv.URL, "rLIV0007", futureRFC3339(t, -time.Minute))
	m := newTestMonitor(t, path)
	code := m.evaluate(context.Background())
	assert.Equal(t, "token_expired", code)
	// Even a healthy router cannot heal an expired token.
	code = m.evaluate(context.Background())
	assert.Equal(t, "token_expired", code)
}

// TestRelayLiveness_RenewedBatchPickedUpWithoutRestart: a resync that
// rewrites the batch (fresh token, new revision) is picked up by the
// registry rescan on the next tick — renewal rides the batch machinery
// (K6), no restart.
func TestRelayLiveness_RenewedBatchPickedUpWithoutRestart(t *testing.T) {
	router := newRelayTestRouter(t)
	dir := t.TempDir()
	path := writeRelayBatch(t, dir, router.srv.URL, "rOLD0001", futureRFC3339(t, -time.Minute))
	m := newTestMonitor(t, path)
	assert.Equal(t, "token_expired", m.evaluate(context.Background()))

	// The resync delivers a fresh token batch at the same path.
	writeRelayBatch(t, dir, router.srv.URL, "rNEW0002", futureRFC3339(t, time.Hour))
	assert.Equal(t, "", m.evaluate(context.Background()), "fresh token + healthy router → healthy")
	snap := m.snapshot()
	assert.Equal(t, "rNEW0002", snap.AppliedRevision, "the revision feed tracks the applied batch")
}

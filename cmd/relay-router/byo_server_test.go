// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rigProviderKey = "qZ7#k9mW2p-unEchoableKey"

type recordedRequest struct {
	path   string
	method string
	auth   string
	body   string
	header http.Header
}

type upstreamRecorder struct {
	upstream *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *upstreamRecorder {
	t.Helper()
	rec := &upstreamRecorder{}
	rec.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recordedRequest{
			path: r.URL.Path, method: r.Method, auth: r.Header.Get("Authorization"),
			body: string(body), header: r.Header.Clone(),
		})
		rec.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(rec.upstream.Close)
	return rec
}

func (r *upstreamRecorder) last() recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[len(r.requests)-1]
}

// byoTestRig assembles a full server against a fake Secret store, a real
// HPKE keypair lineage, and a recorded upstream.
type byoTestRig struct {
	svc      *byoServer
	ts       *httptest.Server
	upstream *upstreamRecorder
	store    fakeByoStore
	keys     *byoKeyManager
	cache    *byoEnvelopeCache
	minter   byoMintService
	redactor *redact.Redactor
}

func newByoTestRig(t *testing.T) *byoTestRig {
	return newByoTestRigWithUpstream(t, nil)
}

func newByoTestRigWithUpstream(t *testing.T, handler http.HandlerFunc) *byoTestRig {
	t.Helper()
	store := newFakeByoStore()
	keys := newTestKeyManager(store, 0)
	require.NoError(t, keys.Bootstrap(context.Background()))
	kp, err := keys.currentPayload(context.Background())
	require.NoError(t, err)

	cache := newByoEnvelopeCache()
	redactor, err := redact.NewRedactor(nil)
	require.NoError(t, err)
	hook := secrets.RedactStagedKeys{Redactor: redactor}

	// The staged credential: sealed with the controller-side sealer wired
	// to the SAME redactor (the production symmetry — both ends register).
	sealer, err := secrets.NewHPKEStagingSealer(kp.PublicKey, secrets.HPKEKeyID(kp.Generation), hook)
	require.NoError(t, err)
	envelope, err := sealer.Seal(context.Background(), []byte(rigProviderKey))
	require.NoError(t, err)
	cache.Apply(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-relay-env-ws-1-zai",
			Namespace: "llm-relay",
			Labels:    map[string]string{byoEnvWorkspaceLabel: "ws-1", byoEnvProviderLabel: "zai"},
		},
		Data: map[string][]byte{byoEnvDataKey: []byte(envelope)},
	})

	rec := newUpstream(t, handler)
	minter := staticMintService{minter: newByoTokenMinter([]byte("mint-signing-key")), authKey: "controller-mint-key"}

	svc := &byoServer{
		cfg: byoServerConfig{
			maxBodyBytes: 1 << 20,
			maxRespBytes: 1 << 20,
		},
		minter:   minter,
		cache:    cache,
		resolve:  resolveDispatcher{keys: keys},
		quota:    newByoWorkspaceQuota(time.Minute, 100, 0),
		redactor: redactor,
		client:   rec.upstream.Client(),
		metrics:  newByoMetrics().withCacheLen(cache.Len),
	}
	ts := httptest.NewServer(svc.handler())
	t.Cleanup(ts.Close)

	return &byoTestRig{
		svc: svc, ts: ts, upstream: rec, store: store, keys: keys,
		cache: cache, minter: minter, redactor: redactor,
	}
}

func (r *byoTestRig) mint(t *testing.T, mutate func(*byoTokenPayload)) string {
	t.Helper()
	payload := byoTokenPayload{
		WorkspaceID:    "ws-1",
		ProviderSlug:   "zai",
		BaseURL:        r.upstream.upstream.URL + "/v1",
		ModelAllowlist: []string{"glm-4.7"},
		IssuedAt:       time.Now().Add(-time.Minute).Unix(),
		ExpiresAt:      time.Now().Add(time.Hour).Unix(),
		KeyID:          secrets.HPKEKeyID(1),
	}
	if mutate != nil {
		mutate(&payload)
	}
	token, err := r.minter.Mint(payload)
	require.NoError(t, err)
	return token
}

func (r *byoTestRig) do(method, path, token, body string, hdrs map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	return r.ts.Client().Do(req)
}

func rejectionBody(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// TestTokenScopeMatrix: all three scope conjuncts enforced per request —
// workspace, provider, model allowlist — plus expiry, forged HMAC,
// disallowed method/path, and missing token.
func TestTokenScopeMatrix(t *testing.T) {
	rig := newByoTestRig(t)
	good := rig.mint(t, nil)
	body := `{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`

	tests := []struct {
		name       string
		method     string
		token      string
		path       string
		body       string
		wantStatus int
		wantReason string
	}{
		{"happy path", http.MethodPost, good, "/w/ws-1/zai/v1/chat/completions", body, 200, ""},
		{"wrong workspace in path", http.MethodPost, good, "/w/ws-OTHER/zai/v1/chat/completions", body, 403, "scope_violation"},
		{"wrong provider in path", http.MethodPost, good, "/w/ws-1/other/v1/chat/completions", body, 403, "scope_violation"},
		{"off-allowlist model", http.MethodPost, good, "/w/ws-1/zai/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`, 403, "scope_violation"},
		{"missing model", http.MethodPost, good, "/w/ws-1/zai/v1/chat/completions", `{"messages":[]}`, 403, "scope_violation"},
		{"forged HMAC", http.MethodPost, "lrt_forged_forged", "/w/ws-1/zai/v1/chat/completions", body, 401, "unauthorized"},
		{"expired token", http.MethodPost, rig.mint(t, func(p *byoTokenPayload) {
			p.IssuedAt = time.Now().Add(-2 * time.Hour).Unix()
			p.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		}), "/w/ws-1/zai/v1/chat/completions", body, 401, "unauthorized"},
		{"disallowed method", http.MethodPut, good, "/w/ws-1/zai/v1/chat/completions", body, 404, "sanitization_refused"},
		{"disallowed path", http.MethodPost, good, "/w/ws-1/zai/v1/secret/admin", body, 404, "sanitization_refused"},
		{"no token", http.MethodPost, "", "/w/ws-1/zai/v1/chat/completions", body, 401, "unauthorized"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := rig.do(tc.method, tc.path, tc.token, tc.body, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, rejectionBody(t, resp)["reason"])
			} else {
				resp.Body.Close()
			}
		})
	}
}

// TestTokenBaseURLPinning: the forwarded target is always the token's
// baseURL and the resolved key is injected upstream.
func TestTokenBaseURLPinning(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	last := rig.upstream.last()
	assert.Equal(t, "/v1/chat/completions", last.path)
	assert.Equal(t, "Bearer "+rigProviderKey, last.auth, "resolved key injected upstream")
}

// TestRouterSanitizationSuite (§4.7): client Authorization replaced by the
// resolved key; identity/hop headers stripped; oversize bodies refused.
func TestRouterSanitizationSuite(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, map[string]string{
		"X-Workspace-Id":         "spoofed",
		"X-Llmsafespaces-Tenant": "spoofed",
		"X-Relay-Token":          "spoofed",
		"X-Forwarded-For":        "1.2.3.4",
		"Cookie":                 "session=leakme",
		"Connection":             "close",
	})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	up := rig.upstream.last()
	assert.Equal(t, "Bearer "+rigProviderKey, up.auth)
	for _, blocked := range []string{"X-Workspace-Id", "X-Llmsafespaces-Tenant", "X-Relay-Token", "X-Forwarded-For", "Cookie", "Connection"} {
		assert.Empty(t, up.header.Get(blocked), "%s leaked upstream", blocked)
	}

	// Oversize body refused.
	big := `{"model":"glm-4.7","pad":"` + strings.Repeat("a", 2<<20) + `"}`
	resp2, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, big, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp2.StatusCode)
	assert.Equal(t, "sanitization_refused", rejectionBody(t, resp2)["reason"])
}

// TestRevocationSecretDelete401 (K5): revocation = Secret deletion; an
// HMAC-valid token with no cached ciphertext fails closed with the
// credential_stale signal.
func TestRevocationSecretDelete401(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)
	body := `{"model":"glm-4.7"}`

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	// The informer delete event evicts the ciphertext.
	rig.cache.Evict("llm-relay-env-ws-1-zai")

	resp2, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	assert.Equal(t, 401, resp2.StatusCode)
	assert.Equal(t, "credential_stale", rejectionBody(t, resp2)["reason"])
}

// TestTokenKeyIDEnvelopeBinding: a token whose keyID names a different
// generation than the staged envelope is credential_stale (the rotation
// discrimination feeding the dual-key window).
func TestTokenKeyIDEnvelopeBinding(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, func(p *byoTokenPayload) { p.KeyID = secrets.HPKEKeyID(9) })
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
	require.NoError(t, err)
	assert.Equal(t, 401, resp2Status(t, resp))
}

func resp2Status(t *testing.T, resp *http.Response) int {
	t.Helper()
	defer resp.Body.Close()
	return resp.StatusCode
}

// lockedLogBuffer serializes log writes against test reads (the handler
// goroutine logs after the response is delivered).
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRouterLogsMetadataOnly (K7): drive a full request/response, capture
// every log emission, assert zero body bytes.
func TestRouterLogsMetadataOnly(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)

	logs := &lockedLogBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token,
		`{"model":"glm-4.7","messages":[{"content":"secret-ish body"}]}`, nil)
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "status=200")
	}, 3*time.Second, 10*time.Millisecond, "metadata log line should land")
	out := logs.String()
	assert.Contains(t, out, "ws=ws-1", "metadata line present")
	assert.NotContains(t, out, "secret-ish body", "request body bytes in logs")
	assert.NotContains(t, out, rigProviderKey, "resolved key in logs")
	assert.NotContains(t, out, "choices", "response body bytes in logs")
}

// TestResolvedKeyEchoScrubbedFromBodies (§4.9): a request body echoing the
// resolved key is scrubbed before forwarding; an upstream response echoing
// the key is scrubbed before reaching the client.
func TestResolvedKeyEchoScrubbedFromBodies(t *testing.T) {
	t.Run("request body echo", func(t *testing.T) {
		rig := newByoTestRig(t)
		token := rig.mint(t, nil)
		payload := fmt.Sprintf(`{"model":"glm-4.7","messages":[{"content":"the key is %s ok?"}]}`, rigProviderKey)
		resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, payload, nil)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		forwarded := rig.upstream.last().body
		assert.NotContains(t, forwarded, rigProviderKey)
		assert.Contains(t, forwarded, secrets.StagedKeyRedactionReplacement)
	})

	t.Run("response body echo", func(t *testing.T) {
		rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"error":"invalid key %s provided"}`, rigProviderKey)
		})
		token := rig.mint(t, nil)
		resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.NotContains(t, string(body), rigProviderKey)
		assert.Contains(t, string(body), secrets.StagedKeyRedactionReplacement)
	})
}

// TestStreamingRedactionAcrossChunkBoundary: a staged key split across two
// response chunks is redacted whole (carry window).
func TestStreamingRedactionAcrossChunkBoundary(t *testing.T) {
	m, err := redact.NewRedactor(nil)
	require.NoError(t, err)
	require.NoError(t, m.RegisterDynamic(redact.DynamicRule{
		ID: "staged:x", Value: "SPLITKEY0123456789", Replacement: "[X]",
	}))
	stream := newStreamRedactor(newExactValueMatcher(m))

	out1 := stream.Write([]byte("echo SPLITKEY"))
	out2 := stream.Write([]byte("0123456789 tail"))
	combined := string(out1) + string(out2) + string(stream.Flush())
	assert.Equal(t, "echo [X] tail", combined)
}

// TestQuotaEnforced: exceeding the per-workspace request budget → 429 with
// the quota reason.
func TestQuotaEnforced(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.quota = newByoWorkspaceQuota(time.Minute, 2, 0)
	token := rig.mint(t, nil)
	body := `{"model":"glm-4.7"}`

	for i := 0; i < 2; i++ {
		resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
		require.NoError(t, err)
		require.Equal(t, 200, resp2Status(t, resp))
	}
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "quota_exceeded", rejectionBody(t, resp)["reason"])
}

// TestMintEndpointAuthAndShape: the mint endpoint requires the controller
// mint key and honors the TTL bound.
func TestMintEndpointAuthAndShape(t *testing.T) {
	rig := newByoTestRig(t)

	// No auth → 401.
	resp, err := rig.do(http.MethodPost, "/internal/v1/tokens", "", `{}`, nil)
	require.NoError(t, err)
	assert.Equal(t, 401, resp2Status(t, resp))

	// Authed mint → token round-trips through Verify.
	mintReq := `{"workspaceID":"ws-9","providerSlug":"s","baseURL":"https://up.example/v1","modelAllowlist":["m"],"ttlSeconds":3600,"keyID":"hpke-g1"}`
	resp2, err := rig.do(http.MethodPost, "/internal/v1/tokens", "controller-mint-key", mintReq, nil)
	require.NoError(t, err)
	var minted struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&minted))
	resp2.Body.Close()
	_, err = rig.minter.Verify(minted.Token)
	require.NoError(t, err)

	// Out-of-range TTL → 400.
	resp3, err := rig.do(http.MethodPost, "/internal/v1/tokens", "controller-mint-key",
		fmt.Sprintf(`{"workspaceID":"w","providerSlug":"s","baseURL":"u","ttlSeconds":%d,"keyID":"k"}`, 30*24*3600), nil)
	require.NoError(t, err)
	assert.Equal(t, 400, resp2Status(t, resp3))
}

func (r *upstreamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// TestQuotaAdmissionCountsInFlight (post-CI-flake pin): an admitted
// request counts against the window AT ADMISSION — not at response
// completion. The CI flake (PR #1432 merge run): a client could observe
// a completed response and fire the next request before the previous
// handler's post-stream Record ran, so Allow saw an undercount and a
// request beyond budget was admitted (200 instead of 429). This test
// deterministically holds a response mid-stream: with budget=1 and one
// admitted in-flight request, a second request must be 429 even though
// the first has NOT completed (no Record-by-completion has run).
func TestQuotaAdmissionCountsInFlight(t *testing.T) {
	upstreamEntered := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstGate int32
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()                          // transmit headers — the proxy's Do unblocks on headers
		if atomic.CompareAndSwapInt32(&firstGate, 0, 1) { // ONLY the first entry blocks
			upstreamEntered <- struct{}{}
			<-release // hold the FIRST response open — admitted, not completed
		}
	})
	rig.svc.quota = newByoWorkspaceQuota(time.Minute, 1, 0)
	token := rig.mint(t, nil)
	body := `{"model":"glm-4.7"}`

	done := make(chan int, 1)
	go func() {
		resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
		if err != nil {
			done <- -1
			return
		}
		done <- resp.StatusCode
	}()
	<-upstreamEntered // request 1 is admitted and mid-flight

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "in-flight admitted request must count against the window at admission")

	close(release)
	require.NotEqual(t, -1, <-done, "first request must complete cleanly")
}

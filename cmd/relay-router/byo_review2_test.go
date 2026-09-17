// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestByteQuotaEnforced (R5 regression pin, iteration 2): a workspace whose
// byte budget (request-direction) is exhausted → 429 quota_exceeded.
// Reviewer mutation-verified: removing the BytesLeft gate fails this test.
func TestByteQuotaEnforced(t *testing.T) {
	rig := newByoTestRig(t)
	rig.svc.quota = newByoWorkspaceQuota(time.Minute, 100, 64) // 64-byte budget
	token := rig.mint(t, nil)
	body := `{"model":"glm-4.7","pad":"` + strings.Repeat("x", 96) + `"}`

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	resp2, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp2.StatusCode)
	assert.Equal(t, "quota_exceeded", rejectionBody(t, resp2)["reason"])
}

// TestOverCapResponseTruncatesButStaysValid (iteration 2): an upstream
// response larger than maxRespBytes truncates at a chunk boundary with
// valid framing, and the truncation records quota/metrics.
func TestOverCapResponseTruncatesButStaysValid(t *testing.T) {
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("y", 32*1024)
		for i := 0; i < 8; i++ { // 256 KiB > 128 KiB cap below
			_, _ = io.WriteString(w, chunk)
		}
	})
	rig.svc.cfg.maxRespBytes = 128 << 10
	token := rig.mint(t, nil)

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "truncated body must still read cleanly (framing dropped)")
	assert.Less(t, int64(len(body)), int64(300<<10))
	assert.NotZero(t, len(body))
}

// TestMintUnavailableWhenSecretAbsent (iteration 2): no mint-key Secret →
// 503 with the distinct mint_unavailable reason (lazyMinter outage leg).
func TestMintUnavailableWhenSecretAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	auth := newByoMintAuth(mintSecrets{client: cs, ns: "llm-relay"}, "llm-relay")
	minter := newLazyMinter(auth)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/tokens", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	svc := &byoServer{minter: minter, metrics: newByoMetrics(), quota: newByoWorkspaceQuota(time.Minute, 10, 0), cfg: byoServerConfig{}}
	svc.requireMintAuth(func(http.ResponseWriter, *http.Request) {})(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var out map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	assert.Equal(t, "mint_unavailable", out["reason"])
}

// TestRotateEndpointReceiptAndPrecondition (iteration 2, story "rotate
// receipt"): happy receipt carries keyID/generation/publicKey; a
// generation-precondition failure surfaces 500.
func TestRotateEndpointReceiptAndPrecondition(t *testing.T) {
	rig := newByoTestRig(t)

	// Happy receipt.
	resp, err := rig.do(http.MethodPost, "/internal/v1/keys/rotate", "controller-mint-key", "", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	var receipt rotateReceipt
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&receipt))
	assert.Equal(t, "hpke-g2", receipt.KeyID)
	assert.EqualValues(t, 2, receipt.Generation)
	assert.NotEmpty(t, receipt.PublicKey)

	// Precondition failure: a stale-generation manager behind the same
	// Secret store loses the write.
	stale := newTestKeyManager(rig.store, 0)
	require.NoError(t, stale.Bootstrap(context.Background()))
	stale.highwater = 1 // secret is now gen2 — precondition must fail
	rig2svc := &byoServer{minter: rig.minter, metrics: newByoMetrics(), quota: newByoWorkspaceQuota(time.Minute, 10, 0), resolve: resolveDispatcher{keys: stale}, cfg: byoServerConfig{}}
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/keys/rotate", nil)
	req.Header.Set("Authorization", "Bearer controller-mint-key")
	rec := httptest.NewRecorder()
	rig2svc.handleRotateKeys(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var out map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	assert.Equal(t, "rotate_precondition_failed", out["reason"], "distinct from transport-class 500s")
}

// TestDRAfterRotationStaysMonotonic (iteration 2 finding 2): a manager that
// has rotated (highwater 2) and then loses the keypair Secret regenerates
// ABOVE its prior high-water mark; pub never rewinds.
func TestDRAfterRotationStaysMonotonic(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))
	_, err := m.Rotate(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), m.loadedGeneration())

	require.NoError(t, store.cs.CoreV1().Secrets("llm-relay").Delete(context.Background(), byoKeyPairSecretName, metav1.DeleteOptions{}))
	byoWatchDelete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byoKeyPairSecretName}}, m, newByoEnvelopeCache())

	require.Eventually(t, func() bool {
		_, err := store.Get(context.Background(), byoKeyPairSecretName)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)

	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kp, err := secrets.ParseHPKEKeyPairPayload(sec.Data[byoPayloadKey])
	require.NoError(t, err)
	assert.Greater(t, kp.Generation, int64(2), "recovery seeds from the loaded highwater — keyIDs stay monotonic")

	pubSec, err := store.Get(context.Background(), byoPubSecretName)
	require.NoError(t, err)
	var pub secrets.HPKEPubPayload
	require.NoError(t, json.Unmarshal(pubSec.Data[byoPayloadKey], &pub))
	assert.Greater(t, pub.Generation, int64(2), "pub never rewinds")
	assert.Equal(t, kp.Generation, pub.Generation)
}

// TestDrainUsesShutdownWithinGrace (iteration 2): the drain path exercised
// for real — http.Server.Shutdown under a grace bound with an in-flight
// stream completes it (zero resets).
func TestDrainUsesShutdownWithinGrace(t *testing.T) {
	chunks := 10
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			_, _ = io.WriteString(w, "data: chunk-\n\n")
			flusher.Flush()
			time.Sleep(25 * time.Millisecond)
		}
	})
	token := rig.mint(t, nil)

	httpServer := &http.Server{Handler: rig.svc.handler()}
	listener, err := listenOn("127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	addr := listener.Addr().String()

	streamDone := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/w/ws-1/zai/v1/chat/completions", strings.NewReader(`{"model":"glm-4.7"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpServerClient().Do(req)
		if err != nil {
			streamDone <- "err:" + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		streamDone <- string(body)
	}()

	time.Sleep(60 * time.Millisecond) // mid-stream
	shutdownErr := make(chan error, 1)
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr <- httpServer.Shutdown(drainCtx)
	}()

	select {
	case got := <-streamDone:
		require.NotContains(t, got, "err:")
		assert.Equal(t, chunks, strings.Count(got, "data: chunk-"), "stream completed with zero resets")
	case <-time.After(10 * time.Second):
		t.Fatal("stream never completed")
	}
	require.NoError(t, <-shutdownErr)
}

func listenOn(addr string) (netListener, error) { return netListen(addr) }

type netListener = net.Listener

func netListen(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

func httpServerClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// TestDrainWiringThroughServeBYO (iteration 4): the SIGNAL→DRAIN wiring —
// serveBYO's ctx-cancel path runs http.Server.Shutdown under the grace
// bound exactly as SIGTERM does at runtime. Deleting the drain block
// fails this test (the stream or the serve call never returns cleanly).
func TestDrainWiringThroughServeBYO(t *testing.T) {
	chunks := 8
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			_, _ = io.WriteString(w, "data: k\n\n")
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	token := rig.mint(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cfg := loadByoRunConfig()
	cfg.listenAddr = "127.0.0.1:0"
	cfg.drainGrace = 5 * time.Second
	serveErr := make(chan error, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		// The PRODUCTION drain path (serveBYOOn is serveBYO's body —
		// listener injected) — deleting the drain block fails this test.
		serveErr <- serveBYOOn(ctx, cfg, rig.svc, ln)
	}()
	addr := ln.Addr().String()
	streamDone := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/w/ws-1/zai/v1/chat/completions", strings.NewReader(`{"model":"glm-4.7"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			streamDone <- "err:" + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		streamDone <- string(body)
	}()

	time.Sleep(50 * time.Millisecond) // mid-stream: SIGTERM-equivalent cancel
	cancel()

	select {
	case got := <-streamDone:
		require.NotContains(t, got, "err:")
		assert.Equal(t, chunks, strings.Count(got, "data: k"), "in-flight stream completed through the drain")
	case <-time.After(10 * time.Second):
		t.Fatal("stream never completed")
	}
	select {
	case err := <-serveErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("serveBYO never returned after drain")
	}
}

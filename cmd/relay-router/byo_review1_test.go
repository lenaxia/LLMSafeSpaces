// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/lenaxia/llmsafespaces/pkg/redact"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelsServedFromStagedCatalog (AC2 + §4.5): GET /models with a
// production-shaped token (non-empty allowlist) is served from the staged
// catalog — zero upstream fetches, zero key bytes.
func TestModelsServedFromStagedCatalog(t *testing.T) {
	rig := newByoTestRig(t)
	rig.cache.Apply(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llm-relay-env-ws-1-zai",
			Namespace: "llm-relay",
			Labels:    map[string]string{byoEnvWorkspaceLabel: "ws-1", byoEnvProviderLabel: "zai"},
		},
		Data: map[string][]byte{
			byoEnvDataKey:   rig.cache.mustEnvelopeFor(t, "ws-1", "zai"),
			byoEnvModelsKey: []byte(`["glm-4.7","glm-4.7-air"]`),
		},
	})

	token := rig.mint(t, nil) // non-empty allowlist
	resp, err := rig.do(http.MethodGet, "/w/ws-1/zai/v1/models", token, "", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "GET /models must not hit the model-allowlist check (empty body)")

	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listing))
	assert.Equal(t, "list", listing.Object)
	// The listing is scoped to the token's allowlist (glm-4.7 only — the
	// catalog's glm-4.7-air is outside the grant and never enumerated).
	assert.Len(t, listing.Data, 1)
	assert.Equal(t, "glm-4.7", listing.Data[0].ID)
	assert.Zero(t, rig.upstream.count(), "zero upstream fetches for /models")
}

// TestModelsWithoutCatalogFallsThrough: no staged catalog → the request
// forwards upstream (the catalog is US-72.3's writer; until it ships, the
// passthrough keeps enrichment working).
func TestModelsWithoutCatalogFallsThrough(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)
	resp, err := rig.do(http.MethodGet, "/w/ws-1/zai/v1/models", token, "", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 1, rig.upstream.count())
	assert.Equal(t, "/v1/models", rig.upstream.last().path)
}

// TestMetricsScrape: /metrics serves without panic and exposes the request
// counter after traffic.
func TestMetricsScrape(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
	require.NoError(t, err)
	// Drain to EOF: recordRequest fires after the server's copy loop
	// finishes — closing early races the inc on slow runners.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The counter lands once the server's stream loop observed EOF;
	// settle-bounded scrape instead of a single immediate read.
	require.Eventually(t, func() bool {
		metricsResp, err := rig.do(http.MethodGet, "/metrics", "", "", nil)
		if err != nil {
			return false
		}
		defer metricsResp.Body.Close()
		if metricsResp.StatusCode != 200 {
			return false
		}
		body, err := io.ReadAll(metricsResp.Body)
		if err != nil {
			return false
		}
		return strings.Contains(string(body), "llm_relay_byo_requests_total") &&
			strings.Contains(string(body), `workspace="ws-1"`)
	}, 5*time.Second, 25*time.Millisecond, "request counter must be exposed after the stream completes")
}

// TestRedactedResponseFramingIsValid (review R4): an upstream that sets
// Content-Length on a body whose staged key gets redacted must not produce
// a truncated/framed-invalid client read — length framing is dropped.
func TestRedactedResponseFramingIsValid(t *testing.T) {
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		body := fmt.Sprintf(`{"error":"invalid key %s provided"}`, rigProviderKey)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write([]byte(body))
	})
	token := rig.mint(t, nil)
	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7"}`, nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body) // fails with unexpected EOF under the framing bug
	require.NoError(t, err, "client must read the full redacted body")
	assert.NotContains(t, string(body), rigProviderKey)
}

// TestByoMintAuthConcurrent (review R6): concurrent current() calls are
// race-free and converge on the Secret's key.
func TestByoMintAuthConcurrent(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: byoMintAuthKeyName, Namespace: "llm-relay"},
		Data:       map[string][]byte{"mint-key": []byte("the-mint-key")},
	})
	auth := newByoMintAuth(mintSecrets{client: cs, ns: "llm-relay"}, "llm-relay")

	var wg sync.WaitGroup
	results := make(chan string, 32)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := auth.current(context.Background())
			if err != nil {
				results <- "err:" + err.Error()
				return
			}
			results <- key
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		assert.Equal(t, "the-mint-key", got)
	}
}

// TestSimultaneousColdStartConverges (review R7): two replicas bootstrap
// concurrently; both converge, neither errors.
func TestSimultaneousColdStartConverges(t *testing.T) {
	store := newFakeByoStore()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := newTestKeyManager(store, 0)
			if err := m.Bootstrap(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("simultaneous bootstrap failed: %v", err)
	}

	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kp, err := secrets.ParseHPKEKeyPairPayload(sec.Data[byoPayloadKey])
	require.NoError(t, err)
	assert.EqualValues(t, 1, kp.Generation, "one lineage, generation 1")

	pub, err := store.Get(context.Background(), byoPubSecretName)
	require.NoError(t, err)
	assert.NotEmpty(t, pub.Data[byoPayloadKey], "pub secret exists after simultaneous cold start")
}

// TestInformerRevocationBound (review R8 + story K5): the deletion→401
// window is the informer watch propagation alone — cache eviction is
// synchronous with the delivered delete event (tombstones included).
func TestInformerRevocationBound(t *testing.T) {
	rig := newByoTestRig(t)
	token := rig.mint(t, nil)
	body := `{"model":"glm-4.7"}`

	resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	// The watch delivers the delete (tombstone-wrapped, as stale watches
	// do); eviction must be synchronous with delivery.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "llm-relay-env-ws-1-zai"}}
	byoWatchDelete(context.Background(), cache.DeletedFinalStateUnknown{Key: "x", Obj: sec}, rig.keys, rig.cache)

	start := time.Now()
	resp2, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, body, nil)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, 401, resp2.StatusCode)
	assert.Equal(t, "credential_stale", rejectionBody(t, resp2)["reason"])
	assert.Less(t, time.Since(start), time.Second, "post-eviction rejection adds no delay of its own — the window is watch propagation")
}

// TestKeypairSecretDeleteTriggersRecovery (review R8): a running replica
// that observes the keypair Secret deleted regenerates via create-or-adopt
// (no restart).
func TestKeypairSecretDeleteTriggersRecovery(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	require.NoError(t, store.cs.CoreV1().Secrets("llm-relay").Delete(context.Background(), byoKeyPairSecretName, metav1.DeleteOptions{}))
	byoWatchDelete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byoKeyPairSecretName}}, m, newByoEnvelopeCache())

	// Recovery is a bounded-retry goroutine — wait for regeneration.
	require.Eventually(t, func() bool {
		_, err := store.Get(context.Background(), byoKeyPairSecretName)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond, "watch-time delete triggers regeneration without a restart")

	regenerated, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err, "watch-time delete triggers regeneration without a restart")
	kp, err := secrets.ParseHPKEKeyPairPayload(regenerated.Data[byoPayloadKey])
	require.NoError(t, err)
	require.NoError(t, secrets.AssertHPKEKeyPair(kp, 0))
}

// TestBuildBYOServerPinsNonNilRedactor (US-72.1 §4.9 amendment binding):
// the production construction path refuses a nil redactor.
func TestBuildBYOServerPinsNonNilRedactor(t *testing.T) {
	store := newFakeByoStore()
	keys := newTestKeyManager(store, 0)
	require.NoError(t, keys.Bootstrap(context.Background()))
	redactor, err := redact.NewRedactor(nil)
	require.NoError(t, err)

	_, err = buildBYOServer(loadByoRunConfig(), keys, newByoEnvelopeCache(), nil, staticMintService{}, defaultRouterClient())
	require.Error(t, err, "nil redactor must fail construction on the production path")

	keys.redaction = secrets.RedactStagedKeys{Redactor: redactor}
	svc, err := buildBYOServer(loadByoRunConfig(), keys, newByoEnvelopeCache(), redactor, staticMintService{}, defaultRouterClient())
	require.NoError(t, err)
	require.NotNil(t, svc.redactor)
	require.NotNil(t, keys.redaction, "key manager wired to the redaction hook")
}

// TestDrainCompletesInFlightStreams (#1078, e2e-leg substitute for the
// two-replica drill): Shutdown within the grace bound lets an in-flight
// SSE-style stream run to completion — zero client resets.
func TestDrainCompletesInFlightStreams(t *testing.T) {
	chunks := 10
	rig := newByoTestRigWithUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
			time.Sleep(25 * time.Millisecond)
		}
	})
	token := rig.mint(t, nil)

	streamDone := make(chan string, 1)
	go func() {
		resp, err := rig.do(http.MethodPost, "/w/ws-1/zai/v1/chat/completions", token, `{"model":"glm-4.7","stream":true}`, nil)
		if err != nil {
			streamDone <- "err:" + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		streamDone <- string(body)
	}()

	time.Sleep(60 * time.Millisecond) // mid-stream
	// httptest.Server.Close waits for outstanding requests — the same
	// graceful-drain semantic as http.Server.Shutdown within the grace
	// bound (#1078: a cap, not a delay).
	closed := make(chan struct{})
	go func() { rig.ts.Close(); close(closed) }()

	select {
	case got := <-streamDone:
		require.NotContains(t, got, "err:")
		assert.Equal(t, chunks, strings.Count(got, "data: chunk-"), "stream completed with zero resets")
	case <-time.After(10 * time.Second):
		t.Fatal("stream never completed")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("drain never returned after the stream completed")
	}
}

// mustEnvelopeFor returns the cached envelope for tests re-staging a Secret.
func (c *byoEnvelopeCache) mustEnvelopeFor(t *testing.T, ws, slug string) []byte {
	t.Helper()
	env, ok := c.Envelope(ws, slug)
	require.True(t, ok)
	return []byte(env)
}

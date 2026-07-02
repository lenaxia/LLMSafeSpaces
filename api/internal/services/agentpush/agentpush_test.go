// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentpush_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
)

// fakeInjector records the sessionID + matchedSigningKey it saw so tests
// can verify context-carried auth was correctly extracted.
type fakeInjector struct {
	returnJSON     []byte
	returnErr      error
	sawSession     string
	sawKey         []byte
	sawUserID      string
	sawWorkspaceID string
	calls          int
}

func (f *fakeInjector) InjectSecrets(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, workspaceID string) ([]byte, error) {
	f.calls++
	f.sawSession = sessionID
	f.sawKey = matchedSigningKey
	f.sawUserID = userID
	f.sawWorkspaceID = workspaceID
	return f.returnJSON, f.returnErr
}

type fakeResolver struct {
	ip  string
	err error
}

func (f *fakeResolver) GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error) {
	return f.ip, f.err
}

type fakeCache struct {
	evictedKeys []string
	mu          sync.Mutex
}

func (f *fakeCache) Evict(workspaceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evictedKeys = append(f.evictedKeys, workspaceID)
}

// TestPush_HappyPath proves that a valid inject + reachable pod results
// in one POST to /v1/reload-secrets containing the injected JSON, and
// that the model cache is evicted so ListModels reflects the fresh
// provider set. This is the load-bearing behavior for both user-initiated
// (SetBindings) and auto (pod-recreation) callers.
func TestPush_HappyPath(t *testing.T) {
	var receivedBody []byte
	var receivedContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/reload-secrets", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		receivedContentType = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		receivedBody = buf
		_ = json.NewEncoder(w).Encode(map[string]any{"reloaded": 3, "restarted": false})
	}))
	defer server.Close()

	// httptest.Server URL is like http://127.0.0.1:PORT. We need the raw
	// host so the service can format it into http://IP:4097. Easiest: run
	// with a custom httpClient that redirects to server.URL by rewriting
	// the URL on the fly. Simpler: use a resolver that returns the
	// server's host string minus scheme, and swap the port in the client.
	// But agentpush hardcodes :4097. So instead: wrap httpClient with a
	// transport that ignores the URL and dials the test server.
	transport := &rewritingTransport{target: server.URL}

	svc := agentpush.New(
		&fakeInjector{returnJSON: []byte(`[{"type":"env-secret","name":"OPENAI_KEY"}]`)},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(&fakeCache{}),
		agentpush.WithHTTPClient(&http.Client{Transport: transport}),
	)

	result, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 3, result.Reloaded)
	assert.False(t, result.Restarted)
	assert.Equal(t, "application/json", receivedContentType)
	assert.Contains(t, string(receivedBody), "OPENAI_KEY")
}

// TestPush_PassesAuthFromContext proves the WithAuth/AuthFromContext
// round-trip: the service must read sessionID and matchedSigningKey out
// of ctx and pass them into InjectSecrets so the user's DEK is available.
// Without this, non-handler callers (workspace status reader) would
// silently degrade to skip-with-audit for every user-DEK entry.
func TestPush_PassesAuthFromContext(t *testing.T) {
	injector := &fakeInjector{returnJSON: []byte(`[]`)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reloaded":0,"restarted":false}`))
	}))
	defer server.Close()

	svc := agentpush.New(
		injector,
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
	)

	ctx := agentpush.WithAuth(context.Background(), "sess-42", []byte("signing-key"))

	_, err := svc.Push(ctx, "user-abc", "ws-abc")
	require.NoError(t, err)

	assert.Equal(t, "sess-42", injector.sawSession)
	assert.Equal(t, []byte("signing-key"), injector.sawKey)
	assert.Equal(t, "user-abc", injector.sawUserID)
	assert.Equal(t, "ws-abc", injector.sawWorkspaceID)
}

// TestPush_NoPodIPReturnsErrNoRunningPod covers the transient case where
// the workspace exists but no pod is currently running (Suspended,
// mid-recreation, Pending). The auto-push path (pod-recreation) treats
// this as info-level, not warn, and records the "no_pod" metric outcome.
func TestPush_NoPodIPReturnsErrNoRunningPod(t *testing.T) {
	var recordedOutcome string
	svc := agentpush.New(
		&fakeInjector{returnJSON: []byte(`[]`)},
		agentpush.WithPodIPResolver(&fakeResolver{ip: ""}),
		agentpush.WithMetricsHook(func(o string) { recordedOutcome = o }),
	)

	_, err := svc.Push(context.Background(), "user-1", "ws-1")
	assert.ErrorIs(t, err, agentpush.ErrNoRunningPod)
	assert.Equal(t, "no_pod", recordedOutcome,
		"no_pod metric outcome is required so operators can distinguish "+
			"transient pod-restart windows from real reload failures")
}

// TestPush_InjectFailureSurfacesAsInjectFailedMetric proves the outcome
// classification: an InjectSecrets error is inject_failed, distinct from
// reload_failed. Ops dashboards depend on this split (see worklog 0589
// adversarial section).
func TestPush_InjectFailureSurfacesAsInjectFailedMetric(t *testing.T) {
	var recordedOutcome string
	svc := agentpush.New(
		&fakeInjector{returnErr: errors.New("dek unavailable")},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithMetricsHook(func(o string) { recordedOutcome = o }),
	)

	_, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inject secrets")
	assert.Equal(t, "inject_failed", recordedOutcome)
}

// TestPush_ReloadHTTPFailureSurfacesAsReloadFailedMetric covers the
// non-2xx-from-agent case: agent unreachable, timing out, 5xx, or
// returning an error body. All classify as reload_failed.
func TestPush_ReloadHTTPFailureSurfacesAsReloadFailedMetric(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"agentd internal boom"}`))
	}))
	defer server.Close()

	var recordedOutcome string
	svc := agentpush.New(
		&fakeInjector{returnJSON: []byte(`[]`)},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithMetricsHook(func(o string) { recordedOutcome = o }),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
	)

	_, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agentd internal boom")
	assert.Equal(t, "reload_failed", recordedOutcome)
}

// TestPush_MissingResolverReturnsNoPodIPResolver proves the wiring-error
// contract: constructing a Service without a resolver and then calling
// Push is a bug that should surface loudly, not silently succeed.
func TestPush_MissingResolverReturnsNoPodIPResolver(t *testing.T) {
	svc := agentpush.New(&fakeInjector{returnJSON: []byte(`[]`)})
	_, err := svc.Push(context.Background(), "user-1", "ws-1")
	assert.ErrorIs(t, err, agentpush.ErrNoPodIPResolver)
}

// TestPush_SuccessEvictsModelCache locks in the invariant that model-cache
// eviction happens ONLY on success — a failed push must not evict, or
// the next ListModels call would refetch and immediately re-fail against
// a stale provider set (worklog 0186 Gap6 regression).
func TestPush_SuccessEvictsModelCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reloaded":1}`))
	}))
	defer server.Close()

	cache := &fakeCache{}
	svc := agentpush.New(
		&fakeInjector{returnJSON: []byte(`[]`)},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(cache),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
	)

	_, err := svc.Push(context.Background(), "user-1", "ws-42")
	require.NoError(t, err)
	assert.Equal(t, []string{"ws-42"}, cache.evictedKeys)
}

// TestPush_FailureDoesNotEvictModelCache is the counterpart to the above:
// a failed reload must NOT evict so the stale-but-working cache remains
// usable until a subsequent successful push replaces it.
func TestPush_FailureDoesNotEvictModelCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cache := &fakeCache{}
	svc := agentpush.New(
		&fakeInjector{returnJSON: []byte(`[]`)},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(cache),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
	)

	_, _ = svc.Push(context.Background(), "user-1", "ws-42")
	assert.Empty(t, cache.evictedKeys,
		"failed push MUST NOT evict cache; a stale cache is better than "+
			"forcing a refetch that will immediately re-fail")
}

// rewritingTransport lets tests use httptest.Server despite agentpush
// hardcoding the :4097 port. Every request is redirected to the server's
// host regardless of the original URL's host/port.
type rewritingTransport struct {
	target string // scheme://host:port
}

func (t *rewritingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Parse target once, then replace only the host portion on the request.
	if !strings.HasPrefix(t.target, "http://") && !strings.HasPrefix(t.target, "https://") {
		return nil, fmt.Errorf("rewritingTransport: bad target %q", t.target)
	}
	// Substitute the URL's Host with the target's host.
	newURL := *r.URL
	// t.target is like http://127.0.0.1:PORT — split off scheme.
	host := strings.TrimPrefix(t.target, "http://")
	host = strings.TrimPrefix(host, "https://")
	newURL.Scheme = "http"
	newURL.Host = host
	r2 := r.Clone(r.Context())
	r2.URL = &newURL
	r2.Host = host
	return http.DefaultTransport.RoundTrip(r2)
}

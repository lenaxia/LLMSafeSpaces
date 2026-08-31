// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentpush_test

import (
	"context"
	"encoding/base64"
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
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeBuilder records what it saw so tests can verify the builder
// contract, and returns a scriptable batch / degrade / error.
type fakeBuilder struct {
	returnBatch    *secrets.Batch
	returnDegrade  *secrets.BuildDegrade
	returnErr      error
	sawUserID      string
	sawWorkspaceID string
	calls          int
}

func (f *fakeBuilder) BuildWorkspaceBatch(_ context.Context, userID, workspaceID string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	f.calls++
	f.sawUserID = userID
	f.sawWorkspaceID = workspaceID
	if f.returnBatch == nil {
		f.returnBatch = &secrets.Batch{}
	}
	return f.returnBatch, f.returnDegrade, f.returnErr
}

// batchWithEnvSecret builds a one-entry batch for wire assertions.
func batchWithEnvSecret(name string) *secrets.Batch {
	return &secrets.Batch{Entries: []secrets.BatchEntry{{
		SecretID: "sec-1", Version: 1, Type: secrets.SecretTypeEnvSecret,
		Name: name, Value: "v-" + name,
	}}}
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
		&fakeBuilder{returnBatch: batchWithEnvSecret("OPENAI_KEY")},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(&fakeCache{}),
		agentpush.WithHTTPClient(&http.Client{Transport: transport}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
	)

	result, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 3, result.Reloaded)
	assert.False(t, result.Restarted)
	assert.Equal(t, "application/json", receivedContentType)
	assert.Contains(t, string(receivedBody), "OPENAI_KEY")
}

// TestPush_PostsLegacyBodyOfBuiltBatch proves the mixed-fleet wire
// contract: the posted body is exactly LegacyBatchJSON of the batch the
// builder returned — identified entries in, legacy bare array out
// (W15). Also verifies the builder saw the caller's user/workspace.
func TestPush_PostsLegacyBodyOfBuiltBatch(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		receivedBody = buf
		_, _ = w.Write([]byte(`{"reloaded":0,"restarted":false}`))
	}))
	defer server.Close()

	builder := &fakeBuilder{returnBatch: batchWithEnvSecret("OPENAI_KEY")}
	svc := agentpush.New(
		builder,
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
	)

	_, err := svc.Push(context.Background(), "user-abc", "ws-abc")
	require.NoError(t, err)

	assert.Equal(t, string(secrets.LegacyBatchJSON(*builder.returnBatch)), string(receivedBody))
	assert.Equal(t, "user-abc", builder.sawUserID)
	assert.Equal(t, "ws-abc", builder.sawWorkspaceID)
}

// TestPush_DegradedBuildStillPushesServerKEKSubset pins the I10 push
// behavior: a degraded build (user DEK unavailable) does not fail the
// push — the server-KEK subset keeps platform providers alive on the
// pod while the degrade reason surfaces in logs and audits.
func TestPush_DegradedBuildStillPushesServerKEKSubset(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		receivedBody = buf
		_, _ = w.Write([]byte(`{"reloaded":1,"restarted":false}`))
	}))
	defer server.Close()

	svc := agentpush.New(
		&fakeBuilder{
			returnBatch:   batchWithEnvSecret("platform-openai"),
			returnDegrade: &secrets.BuildDegrade{Reason: secrets.DegradeDEKUnwrapFailed},
		},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
	)

	result, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "a degraded build must still push the server-KEK subset")
	assert.Equal(t, 1, result.Reloaded)
	assert.Contains(t, string(receivedBody), "platform-openai")
}

// TestPush_NoPodIPReturnsErrNoRunningPod covers the transient case where
// the workspace exists but no pod is currently running (Suspended,
// mid-recreation, Pending). The auto-push path (pod-recreation) treats
// this as info-level, not warn, and records the "no_pod" metric outcome.
func TestPush_NoPodIPReturnsErrNoRunningPod(t *testing.T) {
	var recordedOutcome string
	svc := agentpush.New(
		&fakeBuilder{},
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
// classification: a builder error is inject_failed, distinct from
// reload_failed. Ops dashboards depend on this split (see worklog 0589
// adversarial section).
func TestPush_InjectFailureSurfacesAsInjectFailedMetric(t *testing.T) {
	var recordedOutcome string
	svc := agentpush.New(
		&fakeBuilder{returnErr: errors.New("revision store down")},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithMetricsHook(func(o string) { recordedOutcome = o }),
	)

	_, err := svc.Push(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build workspace batch")
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
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithMetricsHook(func(o string) { recordedOutcome = o }),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
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
	svc := agentpush.New(&fakeBuilder{})
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
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(cache),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
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
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithModelCache(cache),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: server.URL}}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
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

// --- #848: reload-secrets dispatch must authenticate ---

type fakePasswordProvider struct {
	password string
	err      error
}

func (f *fakePasswordProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return f.password, f.err
}

func TestPush_SendsBasicAuth(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"reloaded":0,"restarted":false}`))
	}))
	defer server.Close()
	transport := &rewritingTransport{target: server.URL}

	svc := agentpush.New(
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithHTTPClient(&http.Client{Transport: transport}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw-7"}),
	)
	_, err := svc.Push(context.Background(), "user-1", "ws-9")
	require.NoError(t, err)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:pw-7")), gotAuth,
		"reload-secrets dispatch must carry the workspace Basic credential")
}

func TestPush_MissingPasswordProviderErrors(t *testing.T) {
	svc := agentpush.New(
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
	)
	_, err := svc.Push(context.Background(), "user-1", "ws-9")
	require.ErrorIs(t, err, agentpush.ErrNoPasswordProvider)
}

func TestPush_PasswordProviderErrorSurfaces(t *testing.T) {
	svc := agentpush.New(
		&fakeBuilder{},
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{err: errors.New("secret gone")}),
	)
	_, err := svc.Push(context.Background(), "user-1", "ws-9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret gone")
}

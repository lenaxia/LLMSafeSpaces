// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentpush_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
)

// notifySpy captures what a notify dispatch actually sent.
type notifySpy struct {
	mu      sync.Mutex
	path    string
	method  string
	auth    string
	bodyLen int
}

func (n *notifySpy) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.path = r.URL.Path
	n.method = r.Method
	n.auth = r.Header.Get("Authorization")
	n.bodyLen = len(body)
}

func (n *notifySpy) snapshot() (path, method, auth string, bodyLen int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.path, n.method, n.auth, n.bodyLen
}

func newNotifyServer(t *testing.T, responder func(w http.ResponseWriter)) (*httptest.Server, *notifySpy) {
	t.Helper()
	spy := &notifySpy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.record(r)
		responder(w)
	}))
	t.Cleanup(server.Close)
	return server, spy
}

func newNotifyService(t *testing.T, target string, opts ...agentpush.Option) *agentpush.Service {
	t.Helper()
	all := append([]agentpush.Option{
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw-7"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: target}}),
	}, opts...)
	return agentpush.New(all...)
}

func TestNotify_PostsEmptyBodyToResyncEndpoint(t *testing.T) {
	server, spy := newNotifyServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"7:aa:bb","restarted":true}`))
	})
	svc := newNotifyService(t, server.URL)

	result, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	path, method, auth, bodyLen := spy.snapshot()
	assert.Equal(t, "/v1/resync-secrets", path)
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, 0, bodyLen, "notify carries NO batch body — the pod re-pulls")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:pw-7")), auth,
		"notify must use the same §D1 workspace-password Basic auth channel as the legacy push")

	assert.Equal(t, "applied", result.Status)
	assert.Equal(t, "7:aa:bb", result.AppliedRev)
	assert.True(t, result.Restarted)
}

func TestNotify_NotModifiedIsSuccess(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"not_modified","appliedRev":"3:cc:dd","restarted":false}`))
	})
	var outcome string
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(func(o string) { outcome = o }))

	result, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "not_modified", result.Status)
	assert.Equal(t, "success", outcome)
}

func TestNotify_RateLimitedIsSuccessShaped(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited"}`))
	})
	var outcome string
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(func(o string) { outcome = o }))

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "429 means the pod is rate-limiting and WILL resync — never an error to the caller")
	assert.Equal(t, "rate_limited", outcome)
}

func TestNotify_Failed502SurfacesError(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"failed","reason":"pull_failed"}`))
	})
	var outcome string
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(func(o string) { outcome = o }))

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull_failed")
	assert.Equal(t, "failed", outcome)
}

func TestNotify_PullUnauthorizedSurfacesError(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"failed","reason":"pull_unauthorized"}`))
	})
	svc := newNotifyService(t, server.URL)

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull_unauthorized")
}

func TestNotify_UnreachablePodSurfacesError(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()

	var outcome string
	svc := agentpush.New(
		agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}),
		agentpush.WithPasswordProvider(&fakePasswordProvider{password: "pw"}),
		agentpush.WithHTTPClient(&http.Client{Transport: &rewritingTransport{target: closed.URL}}),
		agentpush.WithNotifyMetricsHook(func(o string) { outcome = o }),
	)

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Equal(t, "failed", outcome)
}

func TestNotify_NoPodReturnsErrNoRunningPod(t *testing.T) {
	var outcome string
	svc := agentpush.New(
		agentpush.WithPodIPResolver(&fakeResolver{ip: ""}),
		agentpush.WithNotifyMetricsHook(func(o string) { outcome = o }),
	)

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	assert.ErrorIs(t, err, agentpush.ErrNoRunningPod)
	assert.Equal(t, "no_pod", outcome)
}

func TestNotify_MissingResolverReturnsNoPodIPResolver(t *testing.T) {
	svc := agentpush.New()
	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	assert.ErrorIs(t, err, agentpush.ErrNoPodIPResolver)
}

func TestNotify_MissingPasswordProviderErrors(t *testing.T) {
	svc := agentpush.New(agentpush.WithPodIPResolver(&fakeResolver{ip: "10.0.0.5"}))
	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	assert.ErrorIs(t, err, agentpush.ErrNoPasswordProvider)
}

func TestNotify_AppliedEvictsModelCache(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"2:a:b","restarted":false}`))
	})
	cache := &fakeCache{}
	svc := newNotifyService(t, server.URL, agentpush.WithModelCache(cache))

	_, err := svc.Notify(context.Background(), "user-1", "ws-42")
	require.NoError(t, err)
	assert.Equal(t, []string{"ws-42"}, cache.evictedKeys)
}

func TestNotify_NotModifiedDoesNotEvictModelCache(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"not_modified","appliedRev":"2:a:b","restarted":false}`))
	})
	cache := &fakeCache{}
	svc := newNotifyService(t, server.URL, agentpush.WithModelCache(cache))

	_, err := svc.Notify(context.Background(), "user-1", "ws-42")
	require.NoError(t, err)
	assert.Empty(t, cache.evictedKeys, "not_modified changed nothing on the pod — the model cache stays warm")
}

func TestNotify_RateLimitedDoesNotEvictModelCache(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited"}`))
	})
	cache := &fakeCache{}
	svc := newNotifyService(t, server.URL, agentpush.WithModelCache(cache))

	_, err := svc.Notify(context.Background(), "user-1", "ws-42")
	require.NoError(t, err)
	assert.Empty(t, cache.evictedKeys)
}

func TestNotify_MalformedSuccessBodyIsNotAnError(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`not-json`))
	})
	svc := newNotifyService(t, server.URL)

	result, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "a malformed success body is not the caller's problem — the notify was accepted")
	assert.Empty(t, result.Status)
}

// --- M2: a 429 carrying retryAfterMs schedules ONE deferred retry ---

// recordingHook captures the ordered outcome stream of a service's
// dispatches (initial + deferred retry).
type recordingHook struct {
	mu       sync.Mutex
	outcomes []string
}

func (h *recordingHook) record(outcome string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcomes = append(h.outcomes, outcome)
}

func (h *recordingHook) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]string, len(h.outcomes))
	copy(cp, h.outcomes)
	return cp
}

func (h *recordingHook) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := h.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d outcomes; got %v", n, h.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countingServer counts requests and answers from the scripted responder
// queue (each request pops the next response; the last repeats).
func countingServer(t *testing.T, respond func(call int, w http.ResponseWriter)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		respond(int(n), w)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// TestNotify_RateLimitedWithRetryAfterSchedulesDeferredRetry: 429 then
// 200 — the retry fires without any caller involvement, the schedule
// is counted as retry_scheduled, and the retried attempt records its
// own outcome (success).
func TestNotify_RateLimitedWithRetryAfterSchedulesDeferredRetry(t *testing.T) {
	server, calls := countingServer(t, func(call int, w http.ResponseWriter) {
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"status":"rate_limited","retryAfterMs":50}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"8:a:b","restarted":false}`))
	})
	hook := &recordingHook{}
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(hook.record))
	defer svc.Stop()

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "429 stays success-shaped to the caller")

	got := hook.waitFor(t, 2)
	assert.Equal(t, "retry_scheduled", got[0], "the initial 429 with retryAfterMs counts as a scheduled retry")
	assert.Equal(t, "success", got[1], "the retried attempt records its own outcome")
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "exactly one deferred retry fired")
}

// TestNotify_RetryAlsoRateLimitedGivesUp: 429 then 429 — ONE retry
// only; a second deferred retry must never be armed (the reconcile
// loop owns convergence from there).
func TestNotify_RetryAlsoRateLimitedGivesUp(t *testing.T) {
	server, calls := countingServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		// A huge retryAfterMs proves the 2.5s cap AND gives the test a
		// window to observe that no second retry is scheduled.
		_, _ = w.Write([]byte(`{"status":"rate_limited","retryAfterMs":60000}`))
	})
	hook := &recordingHook{}
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(hook.record))
	defer svc.Stop()

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	// The capped retry fires within ~2.5s; wait past its outcome.
	got := hook.waitFor(t, 2)
	assert.Equal(t, "retry_scheduled", got[0])
	assert.Equal(t, "rate_limited", got[1],
		"the retried 429 records rate_limited (no retryAfter honored) and gives up")

	// No third dispatch, even allowing scheduling slack.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 2, len(hook.snapshot()), "exactly ONE deferred retry — never a chain")
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))
}

// TestNotify_RateLimitedWithoutRetryAfterStaysPlain: a bare 429 (no
// retryAfterMs) keeps the old semantics — rate_limited, no retry.
func TestNotify_RateLimitedWithoutRetryAfterStaysPlain(t *testing.T) {
	server, calls := countingServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited"}`))
	})
	hook := &recordingHook{}
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(hook.record))
	defer svc.Stop()

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"rate_limited"}, hook.snapshot())
	assert.Equal(t, int32(1), atomic.LoadInt32(calls), "no deferred retry without retryAfterMs")
}

// TestNotify_StopCancelsPendingRetry: Stop before the deferred retry
// fires — no second dispatch reaches the pod, and Stop returns without
// waiting out the (capped) delay.
func TestNotify_StopCancelsPendingRetry(t *testing.T) {
	server, calls := countingServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited","retryAfterMs":60000}`))
	})
	hook := &recordingHook{}
	svc := newNotifyService(t, server.URL, agentpush.WithNotifyMetricsHook(hook.record))

	_, err := svc.Notify(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Equal(t, []string{"retry_scheduled"}, hook.snapshot(), "the retry must be pending")

	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	// notifyRetryMaxDelay (2.5s) is the capped delay a 60s retryAfterMs
	// yields; Stop must return well before it elapses.
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop must cancel the pending retry, not wait out its capped delay")
	}

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls),
		"the canceled retry must never reach the pod")
	svc.Stop() // idempotent
}

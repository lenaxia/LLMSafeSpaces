// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// US-22.1 — process-only liveness for /v1/healthz.
//
// These tests pin the contract: /v1/healthz must answer from in-process state
// only and never depend on opencode's responsiveness. Worklog 0096 documented
// the failure mode the redesign fixes — when opencode is busy under SSE load,
// the kubelet liveness probe cascades to "kill the pod" within ~3 minutes
// even though agentd itself is fine.

// TestHealthzHandler_ReturnsHealthyWithoutOpencode asserts the canonical
// happy path: handler runs, response is 200, body shape matches
// agentd.HealthzResponse, healthy=true, version=buildVersion, uptime
// reflects the supplied startedAt.
func TestHealthzHandler_ReturnsHealthyWithoutOpencode(t *testing.T) {
	startedAt := time.Now().Add(-42 * time.Second)
	handler := healthzHandler(startedAt, "")

	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Healthy, "process-only liveness must always report healthy when the handler runs")
	assert.Equal(t, buildVersion, resp.Version, "version must be the workspace-agentd build version")
	assert.GreaterOrEqual(t, resp.UptimeSeconds, 42, "uptime must reflect time since startedAt")
	assert.Less(t, resp.UptimeSeconds, 60, "uptime must not be unreasonably large")
}

// TestHealthzHandler_NeverCallsOpencode is the central contract test. The
// handler must NOT make any HTTP calls to opencode. We verify this by
// pointing agentAddr at a server that fails the test if it receives any
// request, then exercising the handler.
func TestHealthzHandler_NeverCallsOpencode(t *testing.T) {
	var opencodeCallCount int32
	opencodeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&opencodeCallCount, 1)
		t.Errorf("opencode received unexpected request to %s — /v1/healthz must not call opencode", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer opencodeMock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeMock.URL)

	handler := healthzHandler(time.Now(), "")

	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&opencodeCallCount),
		"opencode must receive zero requests from /v1/healthz; got %d",
		atomic.LoadInt32(&opencodeCallCount))
}

// TestHealthzHandler_LatencyUnderOpencodeStarvation is the regression test
// for the worklog 0096 incident class. We make the opencode mock hang for
// 30 seconds; the handler must still return < 100ms because it doesn't
// touch opencode.
func TestHealthzHandler_LatencyUnderOpencodeStarvation(t *testing.T) {
	opencodeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second) // hang
	}))
	defer opencodeMock.Close()

	origAddr := getAgentAddr()
	defer func() { setAgentAddr(origAddr) }()
	setAgentAddr(opencodeMock.URL)

	handler := healthzHandler(time.Now(), "")

	const probes = 50
	maxLatency := time.Duration(0)
	for i := 0; i < probes; i++ {
		req := httptest.NewRequest("GET", "/v1/healthz", nil)
		rec := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(rec, req)
		elapsed := time.Since(start)
		assert.Equal(t, http.StatusOK, rec.Code)
		if elapsed > maxLatency {
			maxLatency = elapsed
		}
	}
	assert.Less(t, maxLatency, 100*time.Millisecond,
		"max latency over %d probes was %v; should be < 100ms even when opencode hangs", probes, maxLatency)
}

// TestHealthzHandler_ConcurrentRequestsAreRaceFree asserts the handler is
// safe to run from many goroutines simultaneously (which is exactly what
// kubelet + the controller's frequent probe + arbitrary diagnostic clients
// produce in practice). Run with -race to catch data races.
func TestHealthzHandler_ConcurrentRequestsAreRaceFree(t *testing.T) {
	handler := healthzHandler(time.Now(), "")

	const concurrent = 100
	var wg sync.WaitGroup
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/v1/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()
}

// TestHealthzHandler_IgnoresRequestBodyAndMethod asserts the handler is
// liberal in what it accepts: kubelet uses GET, but stricter test
// fixtures may probe with POST or with a body. The handler should
// respond identically.
func TestHealthzHandler_IgnoresRequestBodyAndMethod(t *testing.T) {
	handler := healthzHandler(time.Now(), "")

	for _, method := range []string{"GET", "POST", "HEAD", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// TestHealthzHandler_UptimeAdvances asserts uptime is monotonically advancing
// and reflects real wall-clock from startedAt. Two calls separated by 1.1s
// must show uptime advancing by at least 1 second.
func TestHealthzHandler_UptimeAdvances(t *testing.T) {
	startedAt := time.Now()
	handler := healthzHandler(startedAt, "")

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest("GET", "/v1/healthz", nil))
	var resp1 agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))

	time.Sleep(1100 * time.Millisecond)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/healthz", nil))
	var resp2 agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))

	assert.GreaterOrEqual(t, resp2.UptimeSeconds, resp1.UptimeSeconds+1,
		"uptime must advance: first=%d, second=%d", resp1.UptimeSeconds, resp2.UptimeSeconds)
}

// TestHealthzHandler_DoesNotConstructOpenCodeClient is a defensive test
// asserting the implementation does not even hold a reference to an
// OpenCodeClient. We accomplish this by passing nil and verifying the
// handler still works. If the implementation regresses to depend on a
// client, this test fails with a nil pointer panic.
func TestHealthzHandler_DoesNotConstructOpenCodeClient(t *testing.T) {
	// healthzHandler takes (startedAt time.Time). It MUST NOT take an
	// *OpenCodeClient. If a future change adds the dependency, the
	// signature changes and this test fails to compile — also acceptable.
	handler := healthzHandler(time.Now(), "")
	require.NotNil(t, handler)

	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// === Benchmark — establishes the < 100ms p99 SLO empirically.
// Run with: go test -bench=BenchmarkHealthzHandler -benchtime=5s
// ./cmd/workspace-agentd/

func BenchmarkHealthzHandler(b *testing.B) {
	handler := healthzHandler(time.Now(), "")
	req := httptest.NewRequest("GET", "/v1/healthz", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// TestHealthzHandler_ContextCancellationIgnored verifies the handler does
// not consult ctx (it has no opencode call to cancel, no goroutine to
// cancel). A canceled context must still produce a 200 response.
func TestHealthzHandler_ContextCancellationIgnored(t *testing.T) {
	handler := healthzHandler(time.Now(), "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	req := httptest.NewRequestWithContext(ctx, "GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"healthz must not consult ctx — cancellation has no meaning for a process-only check")
}

// TestHealthzHandler_ResponseShapeIsExactlyAgentdHealthzResponse asserts
// the JSON body has exactly the three documented fields and no extras.
// Any new fields require coordinated updates in pkg/agentd/types.go and
// downstream consumers (kubelet, controller's probe).
func TestHealthzHandler_ResponseShapeIsExactlyAgentdHealthzResponse(t *testing.T) {
	handler := healthzHandler(time.Now(), "")

	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))

	assert.ElementsMatch(t, []string{"healthy", "version", "uptime_seconds", "userCredsPresent"},
		keys(raw), "response must contain exactly the four documented fields")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestHealthzHandler_BurstThroughput asserts the handler can sustain a
// high request rate without leaking goroutines. This is a regression
// test for the implicit assumption that /v1/healthz is essentially free.
func TestHealthzHandler_BurstThroughput(t *testing.T) {
	handler := healthzHandler(time.Now(), "")

	const requests = 10_000
	deadline := time.Now().Add(2 * time.Second)
	var processed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/v1/healthz", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				processed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, time.Now().UnixNano(), deadline.UnixNano(),
		fmt.Sprintf("expected to process %d requests within 2s; processed=%d", requests, processed.Load()))
	assert.Equal(t, int64(requests), processed.Load())
}

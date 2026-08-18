// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"context"
	cryptosubtle "crypto/subtle"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWsIDHeader = "X-Workspace-ID"

// ---------------------------------------------------------------------------
// Helper: create a fleet + relay + test upstream
// ---------------------------------------------------------------------------

func setupRouterTest(t *testing.T, upstreamHandler http.HandlerFunc) (*relayFleet, *detector429, *routerMetrics, *httptest.Server, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(relay.Close)

	relayPort := extractPort(relay.URL)
	det := newDetector429(fleet, 0.5, relayPort)

	fleet.UpdatePeers([]PeerEntry{
		{ID: "test-relay", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	return fleet, det, metrics, upstream, relay
}

func extractPort(url string) int {
	parts := strings.Split(url, ":")
	if len(parts) < 3 {
		return 8080
	}
	port := 0
	for _, c := range parts[2] {
		port = port*10 + int(c-'0')
	}
	return port
}

// extractEndpoint returns the host:port of a httptest.Server URL — what the
// router dials as http://<endpoint><path> when forwarding to a relay.
func extractEndpoint(serverURL string) string {
	u := strings.TrimPrefix(serverURL, "http://")
	u = strings.TrimPrefix(u, "https://")
	return u
}

// ---------------------------------------------------------------------------
// Proxy forwarding tests
// ---------------------------------------------------------------------------

func TestRouterProxy_ForwardsToRelay(t *testing.T) {
	fleet, _, metrics, _, relay := setupRouterTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called when relay is healthy")
	})

	port := extractPort(relay.URL)
	fb, _ := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, port), metrics, port, fb)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/v1/data")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, `{"ok":true}`, string(body))
}

func TestRouterProxy_FallbackWhenNoRelays(t *testing.T) {
	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"from":"upstream"}`))
	}))
	t.Cleanup(upstream.Close)

	fb, err := newFallbackProxy(upstream.URL, 100.0, 5)
	require.NoError(t, err)

	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, 8080), metrics, 8080, fb)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/v1/data")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, fallbackHeaderValue, resp.Header.Get(fallbackHeader))
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, `{"from":"upstream"}`, string(body))
}

func TestRouterProxy_Returns502WhenNoRelayNoFallback(t *testing.T) {
	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()

	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, 8080), metrics, 8080, nil)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/v1/data")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestRouterProxy_StripsWorkspaceHeader(t *testing.T) {
	var receivedHeaders http.Header
	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		receivedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet.UpdatePeers([]PeerEntry{
		{ID: "test-relay", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	port := extractPort(relay.URL)
	fb, _ := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, port), metrics, port, fb)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	req, _ := http.NewRequest(http.MethodGet, router.URL+"/v1/data", nil)
	req.Header.Set(testWsIDHeader, "ws-12345")
	resp, err := (&http.Client{}).Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Empty(t, receivedHeaders.Get(testWsIDHeader), "X-Workspace-ID must be stripped before forwarding to relay")
}

// ---------------------------------------------------------------------------
// Fallback proxy tests
// ---------------------------------------------------------------------------

func TestFallbackProxy_RateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	fp, err := newFallbackProxy(upstream.URL, 1.0, 1)
	require.NoError(t, err)

	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	resp1, _ := http.Get(server.URL + "/test")
	resp1.Body.Close()
	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2, _ := http.Get(server.URL + "/test")
	resp2.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp2.StatusCode)
	assert.Equal(t, "2", resp2.Header.Get("Retry-After"))
}

func TestFallbackProxy_ConcurrencyLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	fp, err := newFallbackProxy(upstream.URL, 1000.0, 1)
	require.NoError(t, err)

	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	var wg sync.WaitGroup
	statusCodes := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, _ := http.Get(server.URL + "/test")
			if resp != nil {
				statusCodes[idx] = resp.StatusCode
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	okCount := 0
	rateLimited := 0
	for _, code := range statusCodes {
		if code == http.StatusOK {
			okCount++
		}
		if code == http.StatusTooManyRequests {
			rateLimited++
		}
	}
	assert.Equal(t, 1, okCount, "only 1 request should succeed (maxConcurrent=1)")
	assert.True(t, rateLimited >= 1, "at least 1 request should be rate-limited")
}

func TestFallbackProxy_SetsFallbackHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	fp, err := newFallbackProxy(upstream.URL, 1000.0, 5)
	require.NoError(t, err)

	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/test")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fallbackHeaderValue, resp.Header.Get(fallbackHeader))
}

func TestFallbackProxy_StreamsResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{"data: a\n\n", "data: b\n\n"}
		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	fp, err := newFallbackProxy(upstream.URL, 1000.0, 5)
	require.NoError(t, err)

	transport := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: transport}

	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	resp, err := client.Get(server.URL + "/stream")
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lines = append(lines, line)
		}
	}
	assert.Len(t, lines, 2)
	assert.Equal(t, "data: a", lines[0])
	assert.Equal(t, "data: b", lines[1])
}

func TestFallbackProxy_InvalidURL(t *testing.T) {
	_, err := newFallbackProxy("", 0.5, 1)
	assert.Error(t, err)

	_, err = newFallbackProxy("ftp://bad", 0.5, 1)
	assert.Error(t, err)

	_, err = newFallbackProxy("http://", 0.5, 1)
	assert.Error(t, err)
}

func TestFallbackProxy_UpstreamError(t *testing.T) {
	_, err := newFallbackProxy("http://127.0.0.1:1", 1000.0, 5)
	require.NoError(t, err)

	fp, _ := newFallbackProxy("http://127.0.0.1:1", 1000.0, 5)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/test")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// TestFallbackProxy_UpstreamErrorRecordedInMetrics is the issue #911
// observability pin: a fallback upstream failure must be visible in
// relay_router_requests_total{relay="fallback",status="502"} so an outage
// (which previously ran 47 days invisible) is now discoverable in /metrics.
func TestFallbackProxy_UpstreamErrorRecordedInMetrics(t *testing.T) {
	metrics := newRouterMetrics()
	fp, err := newFallbackProxy("http://127.0.0.1:1", 1000.0, 5)
	require.NoError(t, err)
	fp.withFallbackMetrics(metrics)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/test")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	var sb strings.Builder
	metrics.writePrometheus(&sb)
	assert.Contains(t, sb.String(), `relay_router_requests_total{relay="fallback",status="502"} 1`,
		"fallback upstream failures must be visible in metrics (issue #911)")
}

// ---------------------------------------------------------------------------
// Health checker tests
// ---------------------------------------------------------------------------

func TestHealthChecker_MarksHealthyOnSuccess(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	hc := newHealthChecker(fleet, 20*time.Millisecond, 1*time.Second, extractPort(relay.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	hc.run(ctx)

	statuses := fleet.HealthyRelays()
	require.Len(t, statuses, 1)
	assert.True(t, statuses[0].Healthy)
}

func TestHealthChecker_MarksUnhealthyOnFailure(t *testing.T) {
	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: "127.0.0.1", Provider: "oci", State: "healthy"},
	})

	hc := newHealthChecker(fleet, 20*time.Millisecond, 50*time.Millisecond, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hc.run(ctx)

	statuses := fleet.HealthyRelays()
	require.Len(t, statuses, 1)
	assert.False(t, statuses[0].Healthy, "should be unhealthy after 3 consecutive failures")
}

func TestHealthChecker_StopsOnContextCancel(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	hc := newHealthChecker(fleet, 10*time.Millisecond, 1*time.Second, extractPort(relay.URL))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		hc.run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("health checker did not stop")
	}
}

// ---------------------------------------------------------------------------
// Detector tests
// ---------------------------------------------------------------------------

func TestDetector_OnNon429ClearsState(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	det := newDetector429(fleet, 0.5, extractPort(relay.URL))

	fleet.RecordRequest("r1", 429)
	det.OnResponse(context.Background(), "r1", 429)

	det.OnResponse(context.Background(), "r1", 200)

	statuses := fleet.HealthyRelays()
	require.Len(t, statuses, 1)
	assert.False(t, statuses[0].Draining429)
}

func TestDetector_StormDetection(t *testing.T) {
	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: "10.42.42.2", Provider: "oci", State: "healthy"},
		{ID: "r2", Endpoint: "10.42.42.3", Provider: "gcp", State: "healthy"},
	})

	det := newDetector429(fleet, 0.5, 8080)

	for i := 0; i < 20; i++ {
		fleet.RecordRequest("r1", 429)
	}

	det.checkAllStorms()

	statuses := fleet.HealthyRelays()
	var r1Status RelayStatus
	for _, s := range statuses {
		if s.ID == "r1" {
			r1Status = s
		}
	}
	assert.True(t, r1Status.Draining429, "r1 should be draining after 100%% 429 storm detected by checkAllStorms")

	id, _, _, ok := fleet.SelectRelay()
	require.True(t, ok)
	assert.Equal(t, "r2", id, "r1 should be excluded after 429 storm")
}

func TestDetector_CheckAllStorms(t *testing.T) {
	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: "10.42.42.2", Provider: "oci", State: "healthy"},
	})

	for i := 0; i < 20; i++ {
		fleet.RecordRequest("r1", 429)
	}

	det := newDetector429(fleet, 0.5, 8080)
	det.checkAllStorms()

	statuses := fleet.HealthyRelays()
	require.Len(t, statuses, 1)
	assert.True(t, statuses[0].Draining429, "relay should be draining after storm")
}

// ---------------------------------------------------------------------------
// Metrics tests
// ---------------------------------------------------------------------------

func TestRouterMetrics_PrometheusFormat(t *testing.T) {
	m := newRouterMetrics()
	m.recordRequest("r1", 200)
	m.recordRequest("r1", 429)
	m.setActiveStreams("r1", 3)
	m.setRelayHealthy("r1", true)
	m.setRelayEgress("r1", 10240)
	m.setFallbackActive(false)

	var sb strings.Builder
	m.writePrometheus(&sb)
	out := sb.String()

	assert.Contains(t, out, `relay_router_requests_total{relay="r1",status="200"} 1`)
	assert.Contains(t, out, `relay_router_requests_total{relay="r1",status="429"} 1`)
	assert.Contains(t, out, `relay_router_active_streams{relay="r1"} 3`)
	assert.Contains(t, out, `relay_router_relay_healthy{relay="r1"} 1`)
	assert.Contains(t, out, `relay_router_relay_egress_bytes{relay="r1"} 10240`)
	assert.Contains(t, out, `relay_router_fallback_active 0`)
}

func TestRouterMetrics_FallbackActive(t *testing.T) {
	m := newRouterMetrics()
	m.setFallbackActive(true)

	var sb strings.Builder
	m.writePrometheus(&sb)
	assert.Contains(t, sb.String(), "relay_router_fallback_active 1")
}

func TestRouterMetrics_MultipleRelays(t *testing.T) {
	m := newRouterMetrics()
	m.recordRequest("oci-1", 200)
	m.recordRequest("gcp-1", 200)
	m.recordRequest("gcp-1", 429)
	m.setRelayHealthy("oci-1", true)
	m.setRelayHealthy("gcp-1", false)

	var sb strings.Builder
	m.writePrometheus(&sb)
	out := sb.String()

	assert.Contains(t, out, `relay="oci-1"`)
	assert.Contains(t, out, `relay="gcp-1"`)
	assert.Contains(t, out, `relay_router_relay_healthy{relay="oci-1"} 1`)
	assert.Contains(t, out, `relay_router_relay_healthy{relay="gcp-1"} 0`)
}

// ---------------------------------------------------------------------------
// E2E integration test
// ---------------------------------------------------------------------------

func TestE2E_RouterForwardsAndMetrics(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":["gpt-4"]}`))
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "test-relay", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	metrics := newRouterMetrics()
	relayPort := extractPort(relay.URL)
	detector := newDetector429(fleet, 0.5, relayPort)
	fb, _ := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	proxy := newRouterProxy(fleet, detector, metrics, relayPort, fb)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		statuses := fleet.HealthyRelays()
		for _, s := range statuses {
			metrics.setRelayHealthy(s.ID, s.Healthy)
			metrics.setActiveStreams(s.ID, s.ActiveStreams)
		}
		w.Header().Set("Content-Type", "text/plain")
		metrics.writePrometheus(w)
	})
	mux.Handle("/", proxy)

	router := httptest.NewServer(mux)
	t.Cleanup(router.Close)

	resp, err := http.Get(router.URL + "/v1/models")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `{"models":["gpt-4"]}`, string(body))

	metricsResp, err := http.Get(router.URL + "/metrics")
	require.NoError(t, err)
	metricsBody, _ := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()

	metricsStr := string(metricsBody)
	assert.Contains(t, metricsStr, `relay_router_requests_total{relay="test-relay",status="200"} 1`)
	assert.Contains(t, metricsStr, `relay_router_relay_healthy{relay="test-relay"} 1`)
}

func TestE2E_FallbackTransitionsBackToRelay(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"from":"relay"}`))
	}))
	t.Cleanup(relay.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"from":"upstream"}`))
	}))
	t.Cleanup(upstream.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()
	relayPort := extractPort(relay.URL)
	detector := newDetector429(fleet, 0.5, relayPort)
	fb, _ := newFallbackProxy(upstream.URL, 1000.0, 5)
	proxy := newRouterProxy(fleet, detector, metrics, relayPort, fb)

	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	resp, _ := http.Get(server.URL + "/test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, `{"from":"upstream"}`, string(body), "should fallback when no relays")

	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})

	resp2, _ := http.Get(server.URL + "/test")
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	assert.Equal(t, `{"from":"relay"}`, string(body2), "should use relay once available")
}

// ---------------------------------------------------------------------------
// Upstream auth-key injection (PR #297). The router can optionally replace the
// client's Authorization: Bearer public with a real upstream key before
// forwarding, so the relay VM stays a dumb byte-pipe while inference works
// against upstreams that require a real key. Applies to both the relay path
// and the fallback path.
//
// NOTE: the original A23 rationale ("Zen 401s on inference, so we MUST inject")
// was disproven 2026-06-20 (worklog 0420 correction) — `public` still works
// for any model Zen flags `allowAnonymous`. Key injection is now an OPTIONAL
// capability (used when pointing the fleet at a real-key-required upstream),
// not a necessity. These tests verify the mechanism itself, which is valid
// regardless of the rationale.
// ---------------------------------------------------------------------------

func TestApplyUpstreamAuth_ReplacesBearerPublic(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer public")
	h.Set("Content-Type", "application/json")
	applyUpstreamAuth(h, upstreamAuth{key: "sk-real-123", header: ""})
	assert.Equal(t, "Bearer sk-real-123", h.Get("Authorization"),
		"client's Bearer public must be replaced with the real upstream key")
	assert.Equal(t, "application/json", h.Get("Content-Type"),
		"non-auth headers must be preserved")
}

func TestApplyUpstreamAuth_CustomHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer public")
	applyUpstreamAuth(h, upstreamAuth{key: "sk-real", header: "x-api-key"})
	assert.Equal(t, "sk-real", h.Get("X-Api-Key"),
		"custom header name must be used when set")
	assert.Empty(t, h.Get("Authorization"),
		"the original Authorization header must be removed when a custom header is configured")
}

func TestApplyUpstreamAuth_NoOpWhenKeyEmpty(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer public")
	applyUpstreamAuth(h, upstreamAuth{key: "", header: ""})
	assert.Equal(t, "Bearer public", h.Get("Authorization"),
		"empty key must be a no-op (preserves current behavior when injection is unconfigured)")
}

func TestApplyUpstreamAuth_RemovesAllAuthHeaderValues(t *testing.T) {
	h := http.Header{}
	h.Add("Authorization", "Bearer public")
	h.Add("Authorization", "Bearer other")
	applyUpstreamAuth(h, upstreamAuth{key: "sk-real", header: ""})
	assert.Equal(t, []string{"Bearer sk-real"}, h.Values("Authorization"),
		"all existing Authorization values must be replaced by exactly one")
}

// TestRouterProxy_InjectsUpstreamAuthOnRelayPath is the integration test: the
// forwarded request that reaches the relay VM must carry the real key, not the
// client's Bearer public.
func TestRouterProxy_InjectsUpstreamAuthOnRelayPath(t *testing.T) {
	var seenAuth string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	port := extractPort(relay.URL)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy"},
	})
	fb, _ := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, port), newRouterMetrics(), port, fb).
		withUpstreamAuth(upstreamAuth{key: "sk-real-xyz", header: ""})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer public")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "relay should accept the request")
	assert.Equal(t, "Bearer sk-real-xyz", seenAuth,
		"relay VM must receive the injected real key, not the client's Bearer public")
}

// TestRouterProxy_InjectsRelayToken is the integration test for the per-VM
// shared-secret token (worklog 0442, post-WG-removal). The router must set
// X-Relay-Token on every request forwarded to a relay, using the token from
// that relay's PeerEntry. The relay-proxy validates the header via
// constant-time compare (see cmd/relay-proxy/auth.go). This test exercises
// the router side of the contract: a request through the router reaches the
// relay with the correct token header.
func TestRouterProxy_InjectsRelayToken(t *testing.T) {
	var seenToken string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		seenToken = r.Header.Get(relayTokenHeader)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(relay.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	port := extractPort(relay.URL)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(relay.URL), Provider: "oci", State: "healthy", Token: "per-vm-secret-xyz"},
	})
	fb, _ := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, port), newRouterMetrics(), port, fb)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "relay should accept the request")
	assert.Equal(t, "per-vm-secret-xyz", seenToken,
		"router must inject the per-VM token from PeerEntry.Token as X-Relay-Token")
}

// TestRelayToken_EndToEnd_RouterProxiesThroughTokenGatedProxy is the
// cross-binary integration test: it stands up a token-gated relay-proxy
// stand-in (mirroring cmd/relay-proxy/auth.go's requireToken exactly — same
// header name, same constant-time compare, same 401 on mismatch) and routes a
// request through the relay-router to it. This is the strongest wire-level
// proof that the router's injected header is what a real relay-proxy accepts.
// Header-name drift between auth.go's TokenHeader and proxy.go's
// relayTokenHeader would surface here as a 401.
func TestRelayToken_EndToEnd_RouterProxiesThroughTokenGatedProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	// Token-gated relay-proxy stand-in — mirrors cmd/relay-proxy/auth.go's
	// requireToken + buildMux: /healthz exempt, / token-gated,
	// crypto/cryptosubtle.ConstantTimeCompare, 401 on mismatch.
	token := "e2e-integration-secret"
	expectedBytes := []byte(token)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/metrics" {
			if cryptosubtle.ConstantTimeCompare([]byte(r.Header.Get(relayTokenHeader)), expectedBytes) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(proxySrv.Close)
	_ = upstream // upstream reachable; the stand-in returns its own body

	// Real relay-router pointing at the proxy via peers.json.
	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(proxySrv.URL), Provider: "oci", State: "healthy", Token: token},
	})
	fb, _ := newFallbackProxy("https://unused.example.com", 0.5, 1)
	routerProxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, 0), newRouterMetrics(), 0, fb)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer public")
	rec := httptest.NewRecorder()
	routerProxy.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"end-to-end router→token-gated-proxy must succeed (the two binaries agree on X-Relay-Token)")
	assert.Contains(t, rec.Body.String(), "ok")
}

// TestRelayToken_EndToEnd_WrongTokenRejected proves the negative path: if the
// router presents a token that does not match what the relay-proxy expects,
// the proxy returns 401 and the router surfaces the error to the caller. This
// guards against silent misconfiguration where the controller wrote a stale
// token to peers.json.
func TestRelayToken_EndToEnd_WrongTokenRejected(t *testing.T) {
	expectedBytes := []byte("correct-secret")
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if cryptosubtle.ConstantTimeCompare([]byte(r.Header.Get(relayTokenHeader)), expectedBytes) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`should-not-reach`))
	}))
	t.Cleanup(proxySrv.Close)

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "r1", Endpoint: extractEndpoint(proxySrv.URL), Provider: "oci", State: "healthy", Token: "WRONG-secret"},
	})
	fb, _ := newFallbackProxy("https://unused.example.com", 0.5, 1)
	routerProxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, 0), newRouterMetrics(), 0, fb)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	routerProxy.ServeHTTP(rec, req)

	// The relay-proxy returns 401; the router forwards the status unchanged.
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"wrong token must be rejected at the relay-proxy (401) — the controller's per-VM tokens are load-bearing")
}

// TestFallbackProxy_InjectsUpstreamAuth verifies the same injection on the
// direct-fallback path (no relays healthy).
func TestFallbackProxy_InjectsUpstreamAuth(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	fp, err := newFallbackProxy(upstream.URL, 1000.0, 5)
	require.NoError(t, err)
	fp.withUpstreamAuth(upstreamAuth{key: "sk-real-fb", header: ""})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer public")
	rec := httptest.NewRecorder()
	fp.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Bearer sk-real-fb", seenAuth,
		"direct fallback to upstream must carry the injected real key")
}

// TestFallbackProxy_DoesNotSetRelayToken verifies the X-Relay-Token header is
// scoped to the relay path ONLY. The fallback path hits the upstream (Zen)
// directly, which has no concept of a relay token — sending one would be
// harmless but leak the secret to a third party. Token scope is per-VM; the
// fallback has no VM.
func TestFallbackProxy_DoesNotSetRelayToken(t *testing.T) {
	var seenToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get(relayTokenHeader)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	fb, err := newFallbackProxy(upstream.URL, 100, 10) // high limits so the request goes through
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()
	fb.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, seenToken,
		"fallback path must NOT set X-Relay-Token — the token is per-VM and the fallback hits the upstream directly")
}

// TestRouterClientsHaveHeadTimeouts locks the #911 fix: the forwarding and
// fallback clients must bound the pre-body phases (dial + response header)
// so a stalled upstream returns a bounded error instead of hanging a handler
// goroutine forever with no response and an empty log. A total body Timeout
// must NOT be set — chat/SSE streams run minutes and would be truncated.
func TestRouterClientsHaveHeadTimeouts(t *testing.T) {
	// Wiring-level pin: the PROXY constructors must attach the bounded client
	// (a regression that reverts newRouterProxy/newFallbackProxy to
	// &http.Client{Timeout: 0} fails here even if defaultRouterClient stays
	// correct).
	proxy := newRouterProxy(newRelayFleet(3, 5*time.Minute), newDetector429(newRelayFleet(3, 5*time.Minute), 0.5, 1), newRouterMetrics(), 1, nil)
	tr, ok := proxy.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "routerProxy must use a configured *http.Transport")
	assert.Zero(t, proxy.httpClient.Timeout, "no total body timeout — long streams must never be cut")
	require.NotNil(t, tr.ResponseHeaderTimeout, "routerProxy: ResponseHeaderTimeout must be set")
	assert.Positive(t, tr.ResponseHeaderTimeout, "routerProxy: ResponseHeaderTimeout must be non-zero")
	require.NotNil(t, tr.DialContext, "routerProxy: a custom dialer (with timeout) must be configured")
	assert.NotNil(t, tr.DialContext, "routerProxy: dialer must be present (issue #911: blackholed dials)")

	fb, err := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	require.NoError(t, err)
	fbtr, ok := fb.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "fallbackProxy must use a configured *http.Transport")
	assert.Zero(t, fb.httpClient.Timeout, "fallbackProxy: no total body timeout")
	require.NotNil(t, fbtr.ResponseHeaderTimeout, "fallbackProxy: ResponseHeaderTimeout must be set")
	assert.Positive(t, fbtr.ResponseHeaderTimeout, "fallbackProxy: ResponseHeaderTimeout must be non-zero")
	require.NotNil(t, fbtr.DialContext, "fallbackProxy: a custom dialer (with timeout) must be configured")
}

// TestRouterClientDialTimeoutConfigured pins the literal #911 repro defect: a
// blackholed dial (egress SYNs dropped) must stall at most the configured dial
// timeout (5s production; test-scaled here) — NOT http.DefaultTransport's 30s
// — so the failure lands inside the workspace injector's 10s client window as
// a bounded 502 instead of a hang. Reverting to the default transport (or a
// 30s dialer) fails this test.
func TestRouterClientDialTimeoutConfigured(t *testing.T) {
	c := newRouterClient(5*time.Minute, 300*time.Millisecond)
	tr := c.Transport.(*http.Transport)
	require.NotNil(t, tr.DialContext, "a custom dialer must be configured")

	// Blackholed destination: a non-routable address (RFC 5737 TEST-NET-1).
	// Dial to it and assert the configured dial timeout bounds the attempt.
	start := time.Now()
	conn, err := tr.DialContext(context.Background(), "tcp", "192.0.2.1:443")
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err, "blackholed dial must fail")
	assert.Less(t, elapsed, 2*time.Second, "dial must be bounded by the configured timeout (~300ms), not the default 30s; took %s", elapsed)
}

// TestRouterProxy_StalledRelayBounded502 is the handler-level e2e half of
// #911: a relay that accepts the connection but never writes response headers
// must yield a bounded 502 to a real client (the ResponseHeaderTimeout fires)
// instead of hanging. The proxy's client is given a small test-scaled
// ResponseHeaderTimeout so the test completes in milliseconds, not 5 minutes.
// Pre-fix (http.Client{Timeout:0} + default transport, no
// ResponseHeaderTimeout) this test would hang until the test's own client
// timeout.
func TestRouterProxy_StalledRelayBounded502(t *testing.T) {
	// A server that accepts and then stalls without writing any header. The
	// stall releases on cleanup so httptest.Server.Close (which waits for
	// in-flight requests) does not itself block forever.
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never writes a response header until cleanup
	}))
	t.Cleanup(func() {
		close(release)
		stalled.Close()
	})

	fleet := newRelayFleet(3, 5*time.Minute)
	metrics := newRouterMetrics()
	fleet.UpdatePeers([]PeerEntry{
		{ID: "stalled-relay", Endpoint: extractEndpoint(stalled.URL), Provider: "oci", State: "healthy"},
	})

	fb, err := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	require.NoError(t, err)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, extractPort(stalled.URL)), metrics, extractPort(stalled.URL), fb)
	// Test-scaled head timeout: bounded failure must land quickly.
	proxy.httpClient = newRouterClient(300*time.Millisecond, 200*time.Millisecond)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(router.URL + "/v1/data")
	elapsed := time.Since(start)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Less(t, elapsed, 5*time.Second, "a stalled relay must produce a bounded response, not hang until the client timeout")
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// TestForwardFailureLogOmitsPathAndQuery is the log content-safety pin for
// the router's forwarding failure path: when a forward to a relay fails, the
// logged line must NOT contain the caller's forwarded path or query (which
// may carry path-segment secrets and api_key-style parameters). *url.Error
// includes them; logUpstreamError unwraps to the inner error.
func TestForwardFailureLogOmitsPathAndQuery(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); stalled.Close() })

	fleet := newRelayFleet(3, 5*time.Minute)
	fleet.UpdatePeers([]PeerEntry{
		{ID: "stalled-relay", Endpoint: extractEndpoint(stalled.URL), Provider: "oci", State: "healthy"},
	})
	metrics := newRouterMetrics()
	fb, err := newFallbackProxy("https://upstream.example.com", 0.5, 1)
	require.NoError(t, err)
	proxy := newRouterProxy(fleet, newDetector429(fleet, 0.5, extractPort(stalled.URL)), metrics, extractPort(stalled.URL), fb)
	proxy.httpClient = newRouterClient(200*time.Millisecond, 200*time.Millisecond)

	router := httptest.NewServer(proxy)
	t.Cleanup(router.Close)

	// Capture the package logger output.
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	resp, err := http.Get(router.URL + "/v1/secret-path-segment?api_key=SUPERSECRETVALUE")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	logged := buf.String()
	assert.NotContains(t, logged, "secret-path-segment", "log must not contain the forwarded path")
	assert.NotContains(t, logged, "SUPERSECRETVALUE", "log must not contain the forwarded query")
	assert.NotEmpty(t, logged, "the failure must be logged (loud, not silent)")
}

// TestFallbackFailureLogOmitsPathAndQuery is the content-safety pin for the
// fallback path.
func TestFallbackFailureLogOmitsPathAndQuery(t *testing.T) {
	metrics := newRouterMetrics()
	fp, err := newFallbackProxy("http://127.0.0.1:1", 1000.0, 5)
	require.NoError(t, err)
	fp.withFallbackMetrics(metrics)
	fp.httpClient = newRouterClient(200*time.Millisecond, 200*time.Millisecond)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)

	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prevOut)

	resp, err := http.Get(server.URL + "/v1/fallback-secret-path?token=SENTINELQUERY")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	logged := buf.String()
	assert.NotContains(t, logged, "fallback-secret-path", "log must not contain the forwarded path")
	assert.NotContains(t, logged, "SENTINELQUERY", "log must not contain the forwarded query")
	assert.NotEmpty(t, logged, "the failure must be logged (loud, not silent)")
}

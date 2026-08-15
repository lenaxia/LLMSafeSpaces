// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/version"
)

// ---------------------------------------------------------------------------
// Metrics unit tests
// ---------------------------------------------------------------------------

func TestRelayMetrics_RecordRequest(t *testing.T) {
	m := newRelayMetrics()
	m.recordRequest(200)
	m.recordRequest(200)
	m.recordRequest(429)
	m.recordRequest(404)

	var buf strings.Builder
	m.writePrometheus(&buf)
	out := buf.String()

	if !strings.Contains(out, `relay_requests_total{status="200"} 2`) {
		t.Errorf("expected 2 requests with status 200\ngot:\n%s", out)
	}
	if !strings.Contains(out, `relay_requests_total{status="429"} 1`) {
		t.Errorf("expected 1 request with status 429\ngot:\n%s", out)
	}
	if !strings.Contains(out, `relay_requests_total{status="404"} 1`) {
		t.Errorf("expected 1 request with status 404\ngot:\n%s", out)
	}
}

func TestRelayMetrics_RecordEgressBytes(t *testing.T) {
	m := newRelayMetrics()
	m.recordEgressBytes(1024)
	m.recordEgressBytes(512)

	var buf strings.Builder
	m.writePrometheus(&buf)

	if !strings.Contains(buf.String(), "relay_egress_bytes_total 1536") {
		t.Errorf("expected 1536 egress bytes\ngot:\n%s", buf.String())
	}
}

func TestRelayMetrics_RecordKeepalive(t *testing.T) {
	m := newRelayMetrics()
	for i := 0; i < 5; i++ {
		m.recordKeepalive()
	}

	var buf strings.Builder
	m.writePrometheus(&buf)

	if !strings.Contains(buf.String(), "relay_keepalive_total 5") {
		t.Errorf("expected 5 keepalive probes\ngot:\n%s", buf.String())
	}
}

func TestRelayMetrics_ConcurrentAccess(t *testing.T) {
	m := newRelayMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.recordRequest(200)
			m.recordEgressBytes(10)
		}()
	}
	wg.Wait()

	var buf strings.Builder
	m.writePrometheus(&buf)
	out := buf.String()

	if !strings.Contains(out, `relay_requests_total{status="200"} 200`) {
		t.Errorf("expected 200 requests\ngot:\n%s", out)
	}
	if !strings.Contains(out, "relay_egress_bytes_total 2000") {
		t.Errorf("expected 2000 egress bytes\ngot:\n%s", out)
	}
}

func TestRelayMetrics_PrometheusFormat(t *testing.T) {
	m := newRelayMetrics()
	m.recordRequest(200)
	m.recordEgressBytes(100)
	m.recordKeepalive()

	var buf strings.Builder
	m.writePrometheus(&buf)
	out := buf.String()

	required := []string{
		"# HELP relay_requests_total",
		"# TYPE relay_requests_total counter",
		"# HELP relay_egress_bytes_total",
		"# TYPE relay_egress_bytes_total counter",
		"# HELP relay_keepalive_total",
		"# TYPE relay_keepalive_total counter",
	}
	for _, line := range required {
		if !strings.Contains(out, line) {
			t.Errorf("missing Prometheus line: %s\nfull output:\n%s", line, out)
		}
	}
}

func TestRelayMetrics_EmptyMetrics(t *testing.T) {
	m := newRelayMetrics()
	var buf strings.Builder
	m.writePrometheus(&buf)
	out := buf.String()

	if !strings.Contains(out, "relay_egress_bytes_total 0") {
		t.Errorf("expected zero egress bytes\ngot:\n%s", out)
	}
	if !strings.Contains(out, "relay_keepalive_total 0") {
		t.Errorf("expected zero keepalive\ngot:\n%s", out)
	}
}

func TestRelayMetrics_StatusCodesSorted(t *testing.T) {
	m := newRelayMetrics()
	m.recordRequest(500)
	m.recordRequest(200)
	m.recordRequest(429)
	m.recordRequest(200)

	var buf strings.Builder
	m.writePrometheus(&buf)
	out := buf.String()

	idx200 := strings.Index(out, `status="200"`)
	idx429 := strings.Index(out, `status="429"`)
	idx500 := strings.Index(out, `status="500"`)

	if idx200 == -1 || idx429 == -1 || idx500 == -1 {
		t.Fatalf("missing status codes in output:\n%s", out)
	}
	if idx200 >= idx429 || idx429 >= idx500 {
		t.Errorf("status codes not sorted numerically in output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// newProxyHandler validation tests
// ---------------------------------------------------------------------------

func TestNewProxyHandler_InvalidURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid http", "http://example.com", false},
		{"valid https with path", "https://example.com/zen/v1", false},
		{"valid http with port", "http://localhost:8080", false},
		{"empty string", "", true},
		{"missing scheme", "example.com", true},
		{"unsupported scheme", "ftp://example.com", true},
		{"missing host", "http://", true},
		{"garbage", "://://", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newProxyHandler(tt.url, &http.Client{}, newRelayMetrics())
			if (err != nil) != tt.wantErr {
				t.Errorf("newProxyHandler(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Proxy forwarding tests (happy paths)
// ---------------------------------------------------------------------------

func newTestProxy(t *testing.T, upstreamURL string) (*proxyHandler, *httptest.Server) {
	t.Helper()
	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstreamURL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)
	return proxy, relay
}

func TestProxyHandler_ForwardsGETRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("upstream got method %s, want GET", r.Method)
		}
		if r.URL.Path != "/models" {
			t.Errorf("upstream got path %s, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":["gpt-4"]}`))
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	resp, err := http.Get(relay.URL + "/models")
	if err != nil {
		t.Fatalf("GET relay: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"models":["gpt-4"]}` {
		t.Errorf("body = %q, want %q", body, `{"models":["gpt-4"]}`)
	}
}

func TestProxyHandler_ForwardsPOSTWithBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("upstream got method %s, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"prompt":"hello"}` {
			t.Errorf("upstream body = %q", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":"hi"}`))
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	resp, err := http.Post(
		relay.URL+"/chat/completions",
		"application/json",
		strings.NewReader(`{"prompt":"hello"}`),
	)
	if err != nil {
		t.Fatalf("POST relay: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"response":"hi"}` {
		t.Errorf("body = %q", body)
	}
}

func TestProxyHandler_ForwardsQueryParams(t *testing.T) {
	var upstreamQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	resp, err := http.Get(relay.URL + "/models?limit=10&verbose=true")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if upstreamQuery != "limit=10&verbose=true" {
		t.Errorf("upstream query = %q, want limit=10&verbose=true", upstreamQuery)
	}
}

func TestProxyHandler_PassesThroughStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"200 OK", 200},
		{"201 Created", 201},
		{"400 Bad Request", 400},
		{"404 Not Found", 404},
		{"429 Too Many Requests", 429},
		{"500 Internal Server Error", 500},
		{"502 Bad Gateway", 502},
		{"503 Service Unavailable", 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			t.Cleanup(upstream.Close)

			_, relay := newTestProxy(t, upstream.URL)

			resp, err := http.Get(relay.URL + "/test")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.statusCode {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.statusCode)
			}
		})
	}
}

func TestProxyHandler_PassesResponseHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "custom-value")
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	resp, err := http.Get(relay.URL + "/data")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Custom-Header") != "custom-value" {
		t.Errorf("X-Custom-Header = %q", resp.Header.Get("X-Custom-Header"))
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "42" {
		t.Errorf("X-RateLimit-Remaining = %q", resp.Header.Get("X-RateLimit-Remaining"))
	}
}

func TestProxyHandler_StripsHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("X-Preserved", "yes")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	resp, err := http.Get(relay.URL + "/data")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Connection") != "" {
		t.Errorf("hop-by-hop header Connection should not be forwarded")
	}
	if resp.Header.Get("Transfer-Encoding") != "" {
		t.Errorf("hop-by-hop header Transfer-Encoding should not be forwarded")
	}
	if resp.Header.Get("X-Preserved") != "yes" {
		t.Errorf("non-hop-by-hop header X-Preserved should be forwarded")
	}
}

// ---------------------------------------------------------------------------
// Proxy streaming test
// ---------------------------------------------------------------------------

func TestProxyHandler_StreamsResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			"data: chunk-1\n\n",
			"data: chunk-2\n\n",
			"data: chunk-3\n\n",
		}
		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	_, relay := newTestProxy(t, upstream.URL)

	transport := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: transport}

	resp, err := client.Get(relay.URL + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lines = append(lines, line)
		}
	}

	expected := []string{"data: chunk-1", "data: chunk-2", "data: chunk-3"}
	if len(lines) != len(expected) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(expected), lines)
	}
	for i, want := range expected {
		if lines[i] != want {
			t.Errorf("line[%d] = %q, want %q", i, lines[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// Proxy error tests (unhappy paths)
// ---------------------------------------------------------------------------

func TestProxyHandler_UpstreamUnreachable_Returns502(t *testing.T) {
	_, relay := newTestProxy(t, "http://127.0.0.1:1")

	resp, err := http.Get(relay.URL + "/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxyHandler_UpstreamTimeout_Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			return
		}
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	client := &http.Client{
		Timeout: 50 * time.Millisecond,
	}
	proxy, err := newProxyHandler(upstream.URL, client, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)

	resp, err := http.Get(relay.URL + "/slow")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestProxyHandler_ClientCancel_ProducesNoMetric(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, relay.URL+"/hang", nil)
	_, _ = (&http.Client{}).Do(req)

	var buf strings.Builder
	metrics.writePrometheus(&buf)
	out := buf.String()

	if strings.Contains(out, `relay_requests_total{status="502"}`) {
		t.Errorf("client cancel should not produce a 502 metric\ngot:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Proxy metrics recording tests
// ---------------------------------------------------------------------------

func TestProxyHandler_RecordsRequestMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if strings.Contains(r.URL.Path, "notfound") {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)

	for i := 0; i < 3; i++ {
		resp, _ := http.Get(relay.URL + "/ok")
		resp.Body.Close()
	}
	resp, _ := http.Get(relay.URL + "/notfound")
	resp.Body.Close()

	var buf strings.Builder
	metrics.writePrometheus(&buf)
	out := buf.String()

	if !strings.Contains(out, `relay_requests_total{status="200"} 3`) {
		t.Errorf("expected 3 requests with status 200\ngot:\n%s", out)
	}
	if !strings.Contains(out, `relay_requests_total{status="404"} 1`) {
		t.Errorf("expected 1 request with status 404\ngot:\n%s", out)
	}
}

func TestProxyHandler_RecordsEgressBytes(t *testing.T) {
	body := `{"response":"hello world this is a test response"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)

	resp, _ := http.Get(relay.URL + "/data")
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// Close the relay server to ensure all in-flight ServeHTTP goroutines
	// have completed (including recordEgressBytes) before reading metrics.
	relay.Close()

	var buf strings.Builder
	metrics.writePrometheus(&buf)

	expected := int64(len(body))
	if !strings.Contains(buf.String(), "relay_egress_bytes_total "+strconv.FormatInt(expected, 10)) {
		t.Errorf("expected egress bytes %d\ngot:\n%s", expected, buf.String())
	}
}

func TestProxyHandler_RecordsEgressBytesMultipleRequests(t *testing.T) {
	chunks := []string{"short", "a bit longer body", "the longest body of all three here"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chunks[0]))
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}
	relay := httptest.NewServer(proxy)
	t.Cleanup(relay.Close)

	var totalLen int64
	for range chunks {
		resp, _ := http.Get(relay.URL + "/data")
		n, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		totalLen += int64(len(n))
	}

	// Close the relay server to ensure all in-flight ServeHTTP goroutines
	// have completed (including recordEgressBytes) before reading metrics.
	relay.Close()

	var buf strings.Builder
	metrics.writePrometheus(&buf)

	// Each request returns the same chunk (chunks[0])
	expected := int64(len(chunks[0])) * int64(len(chunks))
	if !strings.Contains(buf.String(), "relay_egress_bytes_total "+strconv.FormatInt(expected, 10)) {
		t.Errorf("expected total egress %d\ngot:\n%s", expected, buf.String())
	}
}

// ---------------------------------------------------------------------------
// Handler tests (healthz, metrics)
// ---------------------------------------------------------------------------

func TestHealthzHandler_Returns200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", rec.Body.String())
	}
	// The version header is set BEFORE WriteHeader; a regression that moves
	// the Set after WriteHeader silently drops it, so assert it explicitly.
	if got := rec.Header().Get("X-Llmsafespaces-Version"); got != version.Version {
		t.Errorf("X-Llmsafespaces-Version = %q, want %q (the injected pkg/version.Version)", got, version.Version)
	}
}

// TestHealthz_VersionHeaderWiredThroughMux verifies the version header is
// present on the token-exempt /healthz route wired by buildMux (not just on
// the bare handler), while the catch-all / route stays token-gated.
func TestHealthz_VersionHeaderWiredThroughMux(t *testing.T) {
	mux := buildMux("secret-abc", dummyProxy{}, newRelayMetrics())

	// /healthz: no token, 200, version header present.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200 (must be reachable without a token)", rec.Code)
	}
	if got := rec.Header().Get("X-Llmsafespaces-Version"); got != version.Version {
		t.Errorf("X-Llmsafespaces-Version = %q, want %q on the wired /healthz route", got, version.Version)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("/healthz body should be empty, got %q", rec.Body.String())
	}

	// Catch-all /: still token-gated.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/ status = %d, want 401 (must remain token-gated)", rec.Code)
	}
}

func TestMetricsHandler_ReturnsPrometheusFormat(t *testing.T) {
	metrics := newRelayMetrics()
	metrics.recordRequest(200)
	metrics.recordEgressBytes(42)
	metrics.recordKeepalive()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	handler := metricsHandler(metrics)
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "relay_requests_total") {
		t.Errorf("metrics body missing relay_requests_total:\n%s", body)
	}
	if !strings.Contains(body, "relay_egress_bytes_total") {
		t.Errorf("metrics body missing relay_egress_bytes_total:\n%s", body)
	}
	if !strings.Contains(body, "relay_keepalive_total") {
		t.Errorf("metrics body missing relay_keepalive_total:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// E2E integration test: full request path
// ---------------------------------------------------------------------------

func TestE2E_FullProxyPath_RecordsAllMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","data":"hello"}`))
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/metrics", metricsHandler(metrics))
	mux.Handle("/", proxy)

	relay := httptest.NewServer(mux)
	t.Cleanup(relay.Close)

	resp, err := http.Get(relay.URL + "/v1/data")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"status":"ok","data":"hello"}` {
		t.Fatalf("body = %q", body)
	}

	healthResp, err := http.Get(relay.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz GET: %v", err)
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", healthResp.StatusCode)
	}

	metricsResp, err := http.Get(relay.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics GET: %v", err)
	}
	metricsBody, _ := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()
	metricsStr := string(metricsBody)

	if !strings.Contains(metricsStr, `relay_requests_total{status="200"} 1`) {
		t.Errorf("metrics missing 1 proxied request with status 200:\n%s", metricsStr)
	}
	expectedEgress := int64(len(`{"status":"ok","data":"hello"}`))
	if !strings.Contains(metricsStr, "relay_egress_bytes_total "+strconv.FormatInt(expectedEgress, 10)) {
		t.Errorf("metrics missing egress bytes %d:\n%s", expectedEgress, metricsStr)
	}
}

func TestE2E_HealthzNotProxied(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	metrics := newRelayMetrics()
	proxy, err := newProxyHandler(upstream.URL, &http.Client{}, metrics)
	if err != nil {
		t.Fatalf("newProxyHandler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/metrics", metricsHandler(metrics))
	mux.Handle("/", proxy)

	relay := httptest.NewServer(mux)
	t.Cleanup(relay.Close)

	resp, _ := http.Get(relay.URL + "/healthz")
	resp.Body.Close()

	if upstreamHits != 0 {
		t.Errorf("/healthz should not be proxied to upstream, got %d upstream hits", upstreamHits)
	}
}

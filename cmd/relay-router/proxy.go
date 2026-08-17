// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	fallbackHeader      = "X-Relay-Status"
	fallbackHeaderValue = "fallback"
	defaultAuthHeader   = "Authorization"
	// relayTokenHeader is the per-VM shared-secret header the router sends on
	// every request it forwards to a relay VM. Must match the constant in
	// cmd/relay-proxy/auth.go (TokenHeader) so both sides agree without runtime
	// coordination.
	relayTokenHeader = "X-Relay-Token"
)

// upstreamAuth configures router-side injection of a real upstream API key,
// overriding the client's Authorization header (which workspaces send as
// `Bearer public`). OPTIONAL capability, not a requirement: the original A23
// rationale ("zen rejects `public` on /chat/completions, so we must swap in a
// real key") was disproven 2026-06-20 (worklog 0420 correction) — `public`
// still authorizes inference for any model Zen flags `allowAnonymous`. This
// struct remains valuable when an operator points the fleet at an upstream
// that DOES require a real key (e.g. a paid gateway). Empty key = no-op
// (preserves prior behavior when injection is unconfigured, which is the
// correct default for a Zen+`public` free-model fleet). When configured, the
// key transits the router→relay HTTP path (per-VM token auth, worklog 0442)
// to the relay VM; it is never persisted on the VM's disk.
type upstreamAuth struct {
	key    string
	header string // header name; "" → Authorization (sent as "Bearer <key>")
}

// applyUpstreamAuth rewrites dst's auth header with the configured upstream
// key. No-op when key is empty. When header is the default Authorization, all
// existing Authorization values are replaced with a single "Bearer <key>".
// When a custom header is set, the Authorization header is removed entirely.
func applyUpstreamAuth(dst http.Header, auth upstreamAuth) {
	if auth.key == "" {
		return
	}
	headerName := defaultAuthHeader
	if auth.header != "" {
		headerName = auth.header
	}
	if http.CanonicalHeaderKey(headerName) != defaultAuthHeader {
		dst.Del(defaultAuthHeader)
	}
	dst.Set(headerName, auth.key)
	if http.CanonicalHeaderKey(headerName) == defaultAuthHeader {
		dst.Set(headerName, "Bearer "+auth.key)
	}
}

var routerHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"X-Workspace-Id":      {},
}

// defaultRouterClient returns the http.Client used for forwarding requests
// (routerProxy) and for the direct fallback path (fallbackProxy).
//
// The Client must NOT set a total Timeout — chat/SSE responses stream for
// minutes and a wall-clock deadline would truncate generations mid-stream.
// Instead the transport bounds the phases BEFORE the body streams:
//
//   - DialContext: connecting to a relay VM over WireGuard (or the fallback's
//     upstream) hangs indefinitely without this; a blackholed peer would
//     otherwise pin a handler goroutine forever with no response and no log
//     (issue #911: /provider timed out cluster-wide with an empty router log
//     because the request reached the upstream/relay but no headers ever
//     came back, and Timeout: 0 never fired).
//   - ResponseHeaderTimeout: the upstream accepted the connection but stalled
//     before writing response headers — the exact /provider symptom. A small
//     JSON response (provider catalog) must produce headers promptly; 30s is
//     generous while still bounding a dead peer. The relay→upstream hop is
//     the likeliest staller and this timeout covers the full relay round
//     trip on the router's side.
//
// The 504/502 surfaced here is a head-timeout, distinct from mid-body stalls,
// which intentionally remain unbounded so long generations are never cut.
func defaultRouterClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		},
	}
}

// routerProxy is the HTTP handler that selects a relay and forwards
// the request. If no relays are healthy, it enters fallback mode.
type routerProxy struct {
	fleet      *relayFleet
	detector   *detector429
	metrics    *routerMetrics
	httpClient *http.Client

	fallback       *fallbackProxy
	fallbackActive bool
	fallbackMu     sync.RWMutex

	auth upstreamAuth
}

func newRouterProxy(fleet *relayFleet, detector *detector429, metrics *routerMetrics, _ int, fallback *fallbackProxy) *routerProxy {
	return &routerProxy{
		fleet:      fleet,
		detector:   detector,
		metrics:    metrics,
		httpClient: defaultRouterClient(),
		fallback:   fallback,
	}
}

// withUpstreamAuth configures real-key injection (A23 fix). Builder so
// existing callers/tests are unchanged when injection is unconfigured.
func (rp *routerProxy) withUpstreamAuth(auth upstreamAuth) *routerProxy {
	rp.auth = auth
	return rp
}

func (rp *routerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	relayID, endpoint, token, ok := rp.fleet.SelectRelay()

	if !ok {
		rp.handleFallback(w, r)
		return
	}

	rp.setFallbackActive(false)
	rp.forwardToRelay(w, r, relayID, endpoint, token)
}

func (rp *routerProxy) forwardToRelay(w http.ResponseWriter, r *http.Request, relayID, endpoint, token string) {
	target := fmt.Sprintf("http://%s%s", endpoint, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body) //nolint:gosec // endpoint is from controller-written peers.json (trusted)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	upstreamReq.ContentLength = r.ContentLength
	copyRouterHeaders(upstreamReq.Header, r.Header)
	applyUpstreamAuth(upstreamReq.Header, rp.auth)
	if token != "" {
		upstreamReq.Header.Set(relayTokenHeader, token)
	}

	rp.fleet.RecordStreamStart(relayID)
	defer rp.fleet.RecordStreamEnd(relayID)

	resp, err := rp.httpClient.Do(upstreamReq) //nolint:gosec // target is trusted WG IP
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		rp.fleet.RecordHealthCheck(relayID, false)
		rp.metrics.recordRequest(relayID, http.StatusBadGateway)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyRouterHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var egress int64
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, _ := w.Write(buf[:n])
			egress += int64(written)
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	rp.fleet.RecordRequest(relayID, resp.StatusCode)
	rp.fleet.RecordEgress(relayID, egress)
	rp.metrics.recordRequest(relayID, resp.StatusCode)
	rp.detector.OnResponse(r.Context(), relayID, resp.StatusCode)
}

func (rp *routerProxy) handleFallback(w http.ResponseWriter, r *http.Request) {
	if rp.fallback == nil {
		http.Error(w, "no relay available", http.StatusBadGateway)
		return
	}

	rp.setFallbackActive(true)
	rp.fallback.ServeHTTP(w, r)
}

func (rp *routerProxy) setFallbackActive(active bool) {
	rp.fallbackMu.Lock()
	defer rp.fallbackMu.Unlock()
	if rp.fallbackActive != active {
		rp.fallbackActive = active
		rp.metrics.setFallbackActive(active)
	}
}

// fallbackProxy implements rate-limited direct routing to the upstream
// when all relay VMs are unhealthy. Per the design:
// - Global rate limit of 1 req/2s (token bucket)
// - Max 1 concurrent in-flight request
// - X-Relay-Status: fallback header on all responses
// - Requests exceeding limits get 429 + Retry-After: 2
type fallbackProxy struct {
	upstreamURL   string
	httpClient    *http.Client
	rateInterval  time.Duration
	maxConcurrent int
	mu            sync.Mutex
	lastRequest   time.Time
	inFlight      int

	auth upstreamAuth
}

func newFallbackProxy(upstreamURL string, rate float64, maxConcurrent int) (*fallbackProxy, error) {
	if upstreamURL == "" {
		return nil, fmt.Errorf("upstream URL is required")
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid scheme: %s", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("missing host in upstream URL")
	}

	interval := time.Duration(0)
	if rate > 0 {
		interval = time.Duration(float64(time.Second) / rate)
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return &fallbackProxy{
		upstreamURL:   upstreamURL,
		httpClient:    defaultRouterClient(),
		rateInterval:  interval,
		maxConcurrent: maxConcurrent,
	}, nil
}

// withUpstreamAuth configures real-key injection on the fallback path too
// (A23 fix).
func (fp *fallbackProxy) withUpstreamAuth(auth upstreamAuth) *fallbackProxy {
	fp.auth = auth
	return fp
}

func (fp *fallbackProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fp.mu.Lock()

	if fp.inFlight >= fp.maxConcurrent {
		fp.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	now := time.Now()
	if fp.rateInterval > 0 && now.Sub(fp.lastRequest) < fp.rateInterval {
		fp.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	fp.lastRequest = now
	fp.inFlight++
	fp.mu.Unlock()

	defer func() {
		fp.mu.Lock()
		fp.inFlight--
		fp.mu.Unlock()
	}()

	fp.forward(w, r)
}

func (fp *fallbackProxy) forward(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSuffix(fp.upstreamURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body) //nolint:gosec // target is the configured upstream URL
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	upstreamReq.ContentLength = r.ContentLength
	copyRouterHeaders(upstreamReq.Header, r.Header)
	applyUpstreamAuth(upstreamReq.Header, fp.auth)

	resp, err := fp.httpClient.Do(upstreamReq) //nolint:gosec // target is configured upstream
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyRouterHeaders(w.Header(), resp.Header)
	w.Header().Set(fallbackHeader, fallbackHeaderValue)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
}

func copyRouterHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, isHop := routerHopHeaders[http.CanonicalHeaderKey(key)]; isHop {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

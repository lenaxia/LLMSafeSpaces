// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func isHopByHop(header string) bool {
	_, ok := hopByHopHeaders[http.CanonicalHeaderKey(header)]
	return ok
}

type relayMetrics struct {
	mu               sync.Mutex
	requestsByStatus map[string]int64
	egressBytes      atomic.Int64
	keepaliveTotal   atomic.Int64
}

func newRelayMetrics() *relayMetrics {
	return &relayMetrics{
		requestsByStatus: make(map[string]int64),
	}
}

func (m *relayMetrics) recordRequest(statusCode int) {
	key := strconv.Itoa(statusCode)
	m.mu.Lock()
	m.requestsByStatus[key]++
	m.mu.Unlock()
}

func (m *relayMetrics) recordEgressBytes(n int64) {
	m.egressBytes.Add(n)
}

func (m *relayMetrics) recordKeepalive() {
	m.keepaliveTotal.Add(1)
}

func (m *relayMetrics) writePrometheus(w io.Writer) {
	m.mu.Lock()
	snapshot := make(map[string]int64, len(m.requestsByStatus))
	for k, v := range m.requestsByStatus {
		snapshot[k] = v
	}
	m.mu.Unlock()

	egress := m.egressBytes.Load()
	keepalive := m.keepaliveTotal.Load()

	statuses := make([]string, 0, len(snapshot))
	for s := range snapshot {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)

	var b strings.Builder

	b.WriteString("# HELP relay_requests_total Total number of proxied requests by HTTP status code\n")
	b.WriteString("# TYPE relay_requests_total counter\n")
	for _, s := range statuses {
		fmt.Fprintf(&b, "relay_requests_total{status=\"%s\"} %d\n", s, snapshot[s])
	}

	b.WriteString("# HELP relay_egress_bytes_total Total response body bytes proxied to clients\n")
	b.WriteString("# TYPE relay_egress_bytes_total counter\n")
	fmt.Fprintf(&b, "relay_egress_bytes_total %d\n", egress)

	b.WriteString("# HELP relay_keepalive_total Total keepalive probes sent to upstream\n")
	b.WriteString("# TYPE relay_keepalive_total counter\n")
	fmt.Fprintf(&b, "relay_keepalive_total %d\n", keepalive)

	_, _ = io.WriteString(w, b.String())
}

type proxyHandler struct {
	upstream *url.URL
	client   *http.Client
	metrics  *relayMetrics
}

func newProxyHandler(upstreamURL string, client *http.Client, metrics *relayMetrics) (*proxyHandler, error) {
	if upstreamURL == "" {
		return nil, fmt.Errorf("upstream URL is empty")
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL %q: %w", upstreamURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid upstream URL %q: scheme must be http or https", upstreamURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL %q: missing host", upstreamURL)
	}
	return &proxyHandler{
		upstream: u,
		client:   client,
		metrics:  metrics,
	}, nil
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upstreamURL := p.buildUpstreamURL(r)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body) //nolint:gosec // proxy by design: forwards to configured upstream
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	upstreamReq.ContentLength = r.ContentLength
	copyNonHopByHopHeaders(upstreamReq.Header, r.Header)

	resp, err := p.client.Do(upstreamReq) //nolint:gosec // proxy by design
	if err != nil {
		// Log BEFORE the client-context check (issue #911: the silent-error
		// signature "zero errors in its own log"). Never log the request URL:
		// *url.Error.Error() includes the full path+query, which may carry
		// secrets — unwrap to the inner error (contains neither).
		inner := err
		if ue, ok := err.(*url.Error); ok {
			inner = ue.Err
		}
		log.Printf("relay-proxy: upstream request failed: %v", inner)
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyNonHopByHopHeaders(w.Header(), resp.Header)
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

	p.metrics.recordRequest(resp.StatusCode)
	p.metrics.recordEgressBytes(egress)
}

func (p *proxyHandler) buildUpstreamURL(r *http.Request) string {
	base := strings.TrimSuffix(p.upstream.Path, "/")
	fullPath := base + r.URL.Path

	result := p.upstream.Scheme + "://" + p.upstream.Host + fullPath
	if r.URL.RawQuery != "" {
		result += "?" + r.URL.RawQuery
	}
	return result
}

func copyNonHopByHopHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHop(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

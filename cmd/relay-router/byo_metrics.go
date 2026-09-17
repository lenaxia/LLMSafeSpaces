// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// byoMetrics carries the BYO resolve router's Prometheus surface.
// Labels are metadata-only (K7): workspace, provider slug, status — never
// body content.
type byoMetrics struct {
	requests  *prometheus.CounterVec
	bytes     *prometheus.CounterVec
	internal  *prometheus.CounterVec
	envelopes prometheus.GaugeFunc

	registry *prometheus.Registry
	handler  http.Handler

	mu       sync.Mutex
	cacheLen func() int
}

func newByoMetrics() *byoMetrics {
	m := &byoMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_relay_byo_requests_total",
			Help: "BYO resolve requests by workspace, provider and status.",
		}, []string{"workspace", "provider", "status"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_relay_byo_response_bytes_total",
			Help: "BYO resolve response bytes by workspace and provider.",
		}, []string{"workspace", "provider"}),
		internal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_relay_byo_internal_requests_total",
			Help: "Internal API (mint/rotate) requests by path and status.",
		}, []string{"path", "status"}),
	}
	m.registry = prometheus.NewRegistry()
	m.registry.MustRegister(m.requests, m.bytes, m.internal)
	m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return m
}

func (m *byoMetrics) withCacheLen(f func() int) *byoMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheLen = f
	m.envelopes = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "llm_relay_byo_envelopes_cached",
		Help: "Staged envelope Secrets currently in the informer cache.",
	}, func() float64 {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.cacheLen == nil {
			return 0
		}
		return float64(m.cacheLen())
	})
	m.registry.MustRegister(m.envelopes)
	return m
}

func (m *byoMetrics) recordRequest(workspace, provider string, status int) {
	m.requests.WithLabelValues(workspace, provider, strconv.Itoa(status)).Inc()
}

func (m *byoMetrics) recordBytes(workspace string, n int64) {
	m.bytes.WithLabelValues(workspace, "aggregate").Add(float64(n))
}

func (m *byoMetrics) recordInternal(path string, status int) {
	m.internal.WithLabelValues(path, strconv.Itoa(status)).Inc()
}

func (m *byoMetrics) writePrometheus(w http.ResponseWriter) {
	m.handler.ServeHTTP(w, nil)
}

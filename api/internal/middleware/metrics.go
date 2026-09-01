// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
)

var (
	// Request metrics
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	httpResponseSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "HTTP response size in bytes",
			Buckets: prometheus.ExponentialBuckets(100, 10, 8),
		},
		[]string{"method", "path"},
	)

	// WebSocket metrics
	wsConnectionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ws_connections_active",
			Help: "Number of active WebSocket connections",
		},
		[]string{"type"},
	)

	wsConnectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ws_connections_total",
			Help: "Total number of WebSocket connections",
		},
		[]string{"type"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(httpResponseSize)
	prometheus.MustRegister(wsConnectionsActive)
	prometheus.MustRegister(wsConnectionsTotal)
}

// MetricsMiddleware returns a middleware that collects metrics
func MetricsMiddleware(metricsService interfaces.MetricsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip metrics for kubelet probe and observability paths.
		// /health is retained as a legacy alias for /livez.
		path := c.Request.URL.Path
		if path == "/metrics" || path == "/health" || path == "/livez" || path == "/readyz" {
			c.Next()
			return
		}

		// Use normalized path to reduce cardinality
		normalizedPath := getNormalizedPath(c.FullPath())

		// Start timer
		start := time.Now()

		// Process request
		c.Next()

		// Calculate request duration
		duration := time.Since(start)

		// Record metrics
		status := strconv.Itoa(c.Writer.Status())
		method := c.Request.Method

		// Update Prometheus metrics
		httpRequestsTotal.WithLabelValues(method, normalizedPath, status).Inc()
		httpRequestDuration.WithLabelValues(method, normalizedPath).Observe(duration.Seconds())
		httpResponseSize.WithLabelValues(method, normalizedPath).Observe(float64(c.Writer.Size()))

		// Record metrics using service
		metricsService.RecordRequest(
			method,
			normalizedPath,
			c.Writer.Status(),
			duration,
			c.Writer.Size(),
		)
	}
}

// WebSocketMetricsMiddleware returns a middleware that tracks WebSocket connections
func WebSocketMetricsMiddleware(metricsService interfaces.MetricsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		connType := c.Param("type")
		if connType == "" {
			connType = "websocket"
		}

		// Get userID from context if available
		userID := ""
		if id, exists := c.Get("userID"); exists {
			if idStr, ok := id.(string); ok {
				userID = idStr
			}
		}

		// Increment active connections before processing
		wsConnectionsActive.WithLabelValues(connType).Inc()
		wsConnectionsTotal.WithLabelValues(connType).Inc()

		metricsService.IncrementActiveConnections(connType, userID)

		// Process request
		c.Next()

		// Decrement active connections after processing
		wsConnectionsActive.WithLabelValues(connType).Dec()

		metricsService.DecrementActiveConnections(connType, userID)
	}
}

// getNormalizedPath returns a normalized path to reduce cardinality
func getNormalizedPath(path string) string {
	// Replace path parameters with placeholders
	// Example: /api/v1/workspaces/:id/sessions/:sessionId/message -> /api/v1/workspaces/{id}/sessions/{id}/message
	re := regexp.MustCompile(`:[^/]+`)
	return re.ReplaceAllString(path, "{id}")
}

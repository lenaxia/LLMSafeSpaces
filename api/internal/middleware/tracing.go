// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// TracingConfig defines configuration for the tracing middleware
type TracingConfig struct {
	// HeaderName is the name of the header to use for the request ID
	HeaderName string

	// PropagateHeader indicates whether to propagate the request ID in the response header
	PropagateHeader bool

	// GenerateIfMissing indicates whether to generate a request ID if one is not provided
	GenerateIfMissing bool

	// UseUUID indicates whether to use UUID for generated request IDs
	UseUUID bool

	// TracerName is the name of the tracer to use
	TracerName string

	// EnableOpenTelemetry indicates whether to use OpenTelemetry for tracing
	EnableOpenTelemetry bool
}

// DefaultTracingConfig returns the default tracing configuration
func DefaultTracingConfig() TracingConfig {
	return TracingConfig{
		HeaderName:          "X-Request-ID",
		PropagateHeader:     true,
		GenerateIfMissing:   true,
		UseUUID:             true,
		TracerName:          "api-service",
		EnableOpenTelemetry: true,
	}
}

// TracingMiddleware returns a middleware that adds request tracing
func TracingMiddleware(log interfaces.LoggerInterface, config ...TracingConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultTracingConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	// Get tracer if OpenTelemetry is enabled
	// Commented out until we properly import OpenTelemetry packages
	/*
		var tracer trace.Tracer
		if cfg.EnableOpenTelemetry {
			tracer = otel.Tracer(cfg.TracerName)
		}
	*/

	return func(c *gin.Context) {
		// Get request ID from header
		requestID := c.GetHeader(cfg.HeaderName)

		// Generate request ID if missing and configured to do so
		if requestID == "" && cfg.GenerateIfMissing {
			if cfg.UseUUID {
				id, err := uuid.NewV4()
				if err == nil {
					requestID = id.String()
				} else {
					// Fallback to timestamp if UUID generation fails
					requestID = fmt.Sprintf("%d", time.Now().UnixNano())
				}
			} else {
				requestID = fmt.Sprintf("%d", time.Now().UnixNano())
			}
		}

		// Store request ID in context
		if requestID != "" {
			c.Set("request_id", requestID)

			// Propagate request ID in response header if configured to do so
			if cfg.PropagateHeader {
				c.Header(cfg.HeaderName, requestID)
			}

			// Add request ID to logger context
			requestLogger := log.With("request_id", requestID)
			c.Set("logger", requestLogger)
		}

		// Add start time to context for latency calculation
		c.Set("start_time", time.Now())

		// Process request without OpenTelemetry for now
		c.Next()

		// Create OpenTelemetry span if enabled - commented out until we properly import OpenTelemetry packages
		/*
			if cfg.EnableOpenTelemetry && tracer != nil {
				// Extract context from incoming request
				ctx := c.Request.Context()
				propagator := otel.GetTextMapPropagator()
				ctx = propagator.Extract(ctx, propagation.HeaderCarrier(c.Request.Header))

				// Start a new span
				spanName := fmt.Sprintf("%s %s", c.Request.Method, c.FullPath())
				ctx, span := tracer.Start(ctx, spanName)
				defer span.End()

				// Set span attributes
				span.SetAttributes(
					attribute.String("http.method", c.Request.Method),
					attribute.String("http.url", c.Request.URL.String()),
					attribute.String("http.host", c.Request.Host),
					attribute.String("http.user_agent", c.Request.UserAgent()),
					attribute.String("http.request_id", requestID),
					attribute.String("http.client_ip", c.ClientIP()),
				)

				// Add userID to span if available
				if userID, exists := c.Get("userID"); exists {
					span.SetAttributes(attribute.String("user.id", userID.(string)))
				}

				// Update context with span
				c.Request = c.Request.WithContext(ctx)

				// Process request
				c.Next()

				// Add response attributes to span
				span.SetAttributes(
					attribute.Int("http.status_code", c.Writer.Status()),
					attribute.Int("http.response_size", c.Writer.Size()),
				)

				// Set span status based on HTTP status code
				if c.Writer.Status() >= 500 {
					span.SetStatus(trace.StatusCodeError, "server error")
				} else if c.Writer.Status() >= 400 {
					span.SetStatus(trace.StatusCodeError, "client error")
				} else {
					span.SetStatus(trace.StatusCodeOk, "")
				}

				// Add error information if any
				if len(c.Errors) > 0 {
					span.SetAttributes(attribute.String("error.message", c.Errors.String()))
				}
			} else {
				// Process request without OpenTelemetry
				c.Next()
			}
		*/
	}
}

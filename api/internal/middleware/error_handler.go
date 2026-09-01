// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"

	apiErrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	httputil "github.com/lenaxia/llmsafespaces/pkg/http"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// ErrorHandlerConfig defines configuration for the error handler middleware
type ErrorHandlerConfig struct {
	// IncludeStackTrace indicates whether to include stack traces in error responses
	IncludeStackTrace bool

	// LogStackTrace indicates whether to log stack traces
	LogStackTrace bool

	// MaxBodyLogSize is the maximum size of request/response bodies to log
	MaxBodyLogSize int

	// SensitiveFields are JSON fields that should be redacted in request/response bodies
	SensitiveFields []string
}

// DefaultErrorHandlerConfig returns the default error handler configuration
func DefaultErrorHandlerConfig() ErrorHandlerConfig {
	return ErrorHandlerConfig{
		IncludeStackTrace: false,
		LogStackTrace:     true,
		MaxBodyLogSize:    1024, // 1KB
		SensitiveFields:   []string{"password", "token", "secret", "key", "apiKey", "credit_card"},
	}
}

// ErrorHandlerMiddleware returns a middleware that handles errors
func ErrorHandlerMiddleware(log interfaces.LoggerInterface, config ...ErrorHandlerConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultErrorHandlerConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *gin.Context) {
		// Create a copy of the request body for logging
		var requestBody []byte
		if c.Request.Body != nil && c.Request.ContentLength > 0 {
			requestBody, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		}

		// Create a response writer that captures the response
		writer := httputil.NewBodyCaptureWriter(c)
		c.Writer = writer

		// Process request
		c.Next()

		// Check for errors
		if len(c.Errors) > 0 {
			// Get the last error
			err := c.Errors.Last().Err

			// Log the error with request details
			logError(log, c, err, requestBody, writer.GetBody(), cfg)

			// Handle the error
			handleError(c, err, cfg)
		}
	}
}

// logError logs an error with request details
func logError(log interfaces.LoggerInterface, c *gin.Context, err error, requestBody []byte, responseBody string, cfg ErrorHandlerConfig) {
	// Determine log level based on error type
	var logLevel string
	if apiErr, ok := err.(*apiErrors.APIError); ok {
		switch apiErr.Type {
		case apiErrors.ErrorTypeValidation, apiErrors.ErrorTypeBadRequest:
			logLevel = "warn"
		case apiErrors.ErrorTypeNotFound:
			logLevel = "info"
		default:
			logLevel = "error"
		}
	} else {
		logLevel = "error"
	}

	// Create log fields
	fields := []interface{}{
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"status", c.Writer.Status(),
		"client_ip", c.ClientIP(),
		"request_id", c.GetString("request_id"),
	}

	// Add user ID if available
	if userID, exists := c.Get("userID"); exists {
		fields = append(fields, "user_id", userID)
	}

	// Add request body if available (truncate if too large)
	if len(requestBody) > 0 {
		reqBodyStr := string(requestBody)
		if len(reqBodyStr) > cfg.MaxBodyLogSize {
			reqBodyStr = reqBodyStr[:cfg.MaxBodyLogSize] + "... (truncated)"
		}

		// Try to parse as JSON for prettier logging and to mask sensitive fields
		var prettyBody interface{}
		if json.Unmarshal(requestBody, &prettyBody) == nil {
			// Mask sensitive fields
			if mapBody, ok := prettyBody.(map[string]interface{}); ok {
				utilities.MaskSensitiveFieldsWithList(mapBody, cfg.SensitiveFields)
			}
			fields = append(fields, "request_body", prettyBody)
		} else {
			fields = append(fields, "request_body", reqBodyStr)
		}
	}

	// Add response body if available (truncate if too large)
	if responseBody != "" {
		if len(responseBody) > cfg.MaxBodyLogSize {
			responseBody = responseBody[:cfg.MaxBodyLogSize] + "... (truncated)"
		}

		// Try to parse as JSON for prettier logging and to mask sensitive fields
		var prettyBody interface{}
		if json.Unmarshal([]byte(responseBody), &prettyBody) == nil {
			// Mask sensitive fields
			if mapBody, ok := prettyBody.(map[string]interface{}); ok {
				utilities.MaskSensitiveFieldsWithList(mapBody, cfg.SensitiveFields)
			}
			fields = append(fields, "response_body", prettyBody)
		} else {
			fields = append(fields, "response_body", responseBody)
		}
	}

	// Add stack trace for internal errors
	if apiErr, ok := err.(*apiErrors.APIError); ok && apiErr.Type == apiErrors.ErrorTypeInternal || cfg.LogStackTrace {
		fields = append(fields, "stack_trace", string(debug.Stack()))
	}

	// Add error details
	if apiErr, ok := err.(*apiErrors.APIError); ok {
		fields = append(fields, "error_type", apiErr.Type)
		fields = append(fields, "error_code", apiErr.Code)

		if apiErr.Details != nil {
			fields = append(fields, "error_details", apiErr.Details)
		}

		if apiErr.Err != nil {
			fields = append(fields, "error_cause", apiErr.Err.Error())
		}
	}

	// Log to OpenTelemetry if available
	// Commented out until we properly import trace and attribute packages
	/*
		if span := trace.SpanFromContext(c.Request.Context()); span != nil {
			span.RecordError(err)
			span.SetStatus(trace.StatusCodeError, fmt.Sprintf("%v", err))

			// Add attributes to span
			span.SetAttributes(
				attribute.String("error.type", logLevel),
				attribute.String("error.message", err.Error()),
			)

			if apiErr, ok := err.(*apiErrors.APIError); ok {
				span.SetAttributes(attribute.String("error.code", apiErr.Code))
			}
		}
	*/

	// Log the error with the appropriate level
	if log != nil {
		switch logLevel {
		case "info":
			log.Info(fmt.Sprintf("Request error: %v", err), fields...)
		case "warn":
			log.Warn(fmt.Sprintf("Request error: %v", err), fields...)
		default:
			log.Error("Request error", err, fields...)
		}
	}
}

// handleError sends an appropriate error response
func handleError(c *gin.Context, err error, cfg ErrorHandlerConfig) {
	// Check if it's an API error
	if apiErr, ok := err.(*apiErrors.APIError); ok {
		// Get status code from API error
		statusCode := apiErr.StatusCode()

		// Add rate limit headers if applicable
		if apiErr.Type == apiErrors.ErrorTypeRateLimit {
			if limit, ok := apiErr.Details["limit"].(int); ok {
				c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
			}
			if reset, ok := apiErr.Details["reset"].(int64); ok {
				c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", reset))
			}
		}

		// Include stack trace if configured
		if cfg.IncludeStackTrace && apiErr.Type == apiErrors.ErrorTypeInternal {
			if apiErr.Details == nil {
				apiErr.Details = make(map[string]interface{})
			}
			apiErr.Details["stack"] = strings.Split(string(debug.Stack()), "\n")
		}

		// Send error response
		c.AbortWithStatusJSON(statusCode, gin.H{
			"error": gin.H{
				"code":    apiErr.Code,
				"message": apiErr.Message,
				"details": apiErr.Details,
			},
		})
		return
	}

	// Handle generic errors
	errorResponse := gin.H{
		"error": gin.H{
			"code":    "internal_error",
			"message": "An unexpected error occurred",
		},
	}

	// Include stack trace if configured
	if cfg.IncludeStackTrace {
		errorResponse["error"].(gin.H)["details"] = gin.H{
			"stack": strings.Split(string(debug.Stack()), "\n"),
		}
	}

	c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse)
}

// HandleAPIError handles an API error in a handler
func HandleAPIError(c *gin.Context, err error) {
	// Add error to gin context
	_ = c.Error(err)

	// Abort the request
	c.Abort()
}

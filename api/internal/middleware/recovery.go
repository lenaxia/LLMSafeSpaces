// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// RecoveryConfig defines configuration for the recovery middleware
type RecoveryConfig struct {
	// IncludeStackTrace indicates whether to include stack traces in error responses
	IncludeStackTrace bool

	// LogStackTrace indicates whether to log stack traces
	LogStackTrace bool

	// CustomRecoveryHandler is a custom function to handle recovery
	CustomRecoveryHandler func(*gin.Context, interface{})
}

// DefaultRecoveryConfig returns the default recovery configuration
func DefaultRecoveryConfig() RecoveryConfig {
	return RecoveryConfig{
		IncludeStackTrace:     false,
		LogStackTrace:         true,
		CustomRecoveryHandler: nil,
	}
}

// RecoveryMiddleware returns a middleware that recovers from panics
func RecoveryMiddleware(log interfaces.LoggerInterface, config ...RecoveryConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultRecoveryConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Check for a broken connection, as it is not really a
				// condition that warrants a panic stack trace.
				var brokenPipe bool
				if ne, ok := err.(*net.OpError); ok {
					if se, ok := ne.Err.(*os.SyscallError); ok {
						if strings.Contains(strings.ToLower(se.Error()), "broken pipe") ||
							strings.Contains(strings.ToLower(se.Error()), "connection reset by peer") {
							brokenPipe = true
						}
					}
				}

				// Get stack trace
				stack := string(debug.Stack())

				// Create error message
				errMsg := fmt.Sprintf("%v", err)

				// Log the error
				httpRequest := fmt.Sprintf("%s %s", c.Request.Method, c.Request.URL.Path)
				if brokenPipe {
					log.Error("Broken pipe", fmt.Errorf("%v", err),
						"request", httpRequest,
						"client_ip", c.ClientIP(),
						"request_id", c.GetString("request_id"),
					)
				} else {
					logFields := []interface{}{
						"request", httpRequest,
						"client_ip", c.ClientIP(),
						"request_id", c.GetString("request_id"),
						"error", errMsg,
					}

					if cfg.LogStackTrace {
						logFields = append(logFields, "stack", stack)
					}

					log.Error("Recovery from panic", fmt.Errorf("%v", err), logFields...)

					// Log to OpenTelemetry if available
					// Commented out until we properly import trace and attribute packages
					/*
						if span := trace.SpanFromContext(c.Request.Context()); span != nil {
							span.RecordError(fmt.Errorf("%v", err))
							span.SetStatus(trace.StatusCodeError, "panic recovered")
							if cfg.LogStackTrace {
								span.SetAttributes(attribute.String("error.stack", stack))
							}
						}
					*/
				}

				// If the connection is dead, we can't write a status to it.
				if brokenPipe {
					c.Abort()
					return
				}

				// Use custom recovery handler if provided
				if cfg.CustomRecoveryHandler != nil {
					cfg.CustomRecoveryHandler(c, err)
					return
				}

				// Create API error
				apiErr := errors.NewInternalError("Internal server error", fmt.Errorf("%v", err))

				// Include stack trace in response if configured
				if cfg.IncludeStackTrace {
					apiErr.Details = map[string]interface{}{
						"stack": strings.Split(stack, "\n"),
					}
				}

				// Send error response
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"code":    apiErr.Code,
						"message": apiErr.Message,
						"details": apiErr.Details,
					},
				})
				c.Abort()
			}
		}()

		c.Next()
	}
}

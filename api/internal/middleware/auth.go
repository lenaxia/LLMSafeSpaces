// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/utilities"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// AuthConfig defines configuration for the authentication middleware
type AuthConfig struct {
	// HeaderName is the name of the header containing the authentication token
	HeaderName string

	// QueryParamName is the name of the query parameter containing the authentication token
	QueryParamName string

	// CookieName is the name of the cookie containing the authentication token
	CookieName string

	// TokenType is the type of token (e.g., "Bearer")
	TokenType string

	// SkipPaths are paths that should not be authenticated
	SkipPaths []string

	// SkipPathPrefixes are path prefixes that should not be authenticated
	SkipPathPrefixes []string
}

// DefaultAuthConfig returns the default authentication configuration
func DefaultAuthConfig() AuthConfig {
	return AuthConfig{
		HeaderName:       "Authorization",
		QueryParamName:   "",
		CookieName:       "",
		TokenType:        "Bearer",
		SkipPaths:        []string{"/health", "/livez", "/readyz", "/metrics", "/api/v1/auth/login", "/api/v1/auth/register"},
		SkipPathPrefixes: []string{"/static/", "/docs/"},
	}
}

// AuthMiddleware returns a middleware that handles authentication
func AuthMiddleware(authService apiinterfaces.AuthService, log pkginterfaces.LoggerInterface, config ...AuthConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultAuthConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *gin.Context) {
		// Skip authentication for certain paths
		path := c.Request.URL.Path
		if shouldSkipAuth(path, cfg.SkipPaths, cfg.SkipPathPrefixes) {
			c.Next()
			return
		}

		// Extract token from request
		token := extractToken(c, cfg)
		if token == "" {
			if log != nil {
				log.Warn("Authentication failed: no token provided",
					"path", path,
					"method", c.Request.Method,
					"client_ip", c.ClientIP(),
					"request_id", c.GetString("request_id"),
				)
			}

			metrics.RecordAuthFailure("missing_token")
			metrics.RecordAuthAttempt(authMethodForToken(""), "failure")
			apiErr := errors.NewAuthenticationError("Authentication required", nil)
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error": apiErr.Message,
			})
			return
		}

		// Validate token
		userID, err := authService.ValidateToken(c.Request.Context(), token)
		if err != nil {
			if log != nil {
				log.Warn("Authentication failed: invalid token",
					"path", path,
					"method", c.Request.Method,
					"client_ip", c.ClientIP(),
					"request_id", c.GetString("request_id"),
					"error", err.Error(),
				)
			}

			metrics.RecordAuthFailure("invalid_token")
			metrics.RecordAuthAttempt(authMethodForToken(token), "failure")
			apiErr := errors.NewAuthenticationError("Invalid or expired token", nil)
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error": apiErr.Message,
			})
			return
		}

		// Successful authentication — record before any further work so
		// a panic deeper in the request lifecycle still leaves the
		// metric incremented.
		metrics.RecordAuthAttempt(authMethodForToken(token), "success")

		// Store authentication result in Gin context (for middleware/handlers)
		// and in the request context (for service layer via ctx.Value).
		c.Set("userID", userID)

		// Extract JWT's jti claim as sessionID for DEK cache lookup.
		// For API key sessions (no jti), derive a deterministic sessionID
		// from the token hash so DEK caching works for decrypt_access keys.
		if jti := utilities.ExtractJTI(token); jti != "" {
			c.Set("sessionID", jti)
		} else {
			c.Set("sessionID", "apikey:"+pkgutil.HashString(token))
		}

		ctx := context.WithValue(c.Request.Context(), types.ContextKeyUserID, userID)
		c.Request = c.Request.WithContext(ctx)

		// Add user ID to logger context
		if requestLogger, exists := c.Get("logger"); exists {
			if typedLogger, ok := requestLogger.(pkginterfaces.LoggerInterface); ok {
				newLogger := typedLogger.With("user_id", userID)
				c.Set("logger", newLogger)
			}
		}

		c.Next()
	}
}

// AuthorizationMiddleware returns a middleware that handles authorization
func AuthorizationMiddleware(authService apiinterfaces.AuthService, log pkginterfaces.LoggerInterface) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip authorization for certain paths
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		// Get required permissions from context
		requiredPermissions, exists := c.Get("requiredPermissions")
		if !exists {
			c.Next()
			return
		}

		// Get user permissions from context
		userPermissions, exists := c.Get("permissions")
		if !exists {
			log.Warn("Authorization failed: no permissions in context",
				"path", c.Request.URL.Path,
				"method", c.Request.Method,
				"client_ip", c.ClientIP(),
				"request_id", c.GetString("request_id"),
			)

			apiErr := errors.NewForbiddenError("Authorization required", nil)
			HandleAPIError(c, apiErr)
			return
		}

		// Check if user has required permissions
		hasPermission := false
		userPerms := userPermissions.([]string)
		requiredPerms := requiredPermissions.([]string)

		// Simple permission check - user must have all required permissions
		hasAll := true
		for _, required := range requiredPerms {
			found := false
			for _, userPerm := range userPerms {
				if required == userPerm {
					found = true
					break
				}
			}
			if !found {
				hasAll = false
				break
			}
		}
		hasPermission = hasAll

		if !hasPermission {
			log.Warn("Authorization failed: insufficient permissions",
				"path", c.Request.URL.Path,
				"method", c.Request.Method,
				"client_ip", c.ClientIP(),
				"request_id", c.GetString("request_id"),
				"user_id", c.GetString("userID"),
				"required_permissions", requiredPermissions,
			)

			apiErr := errors.NewForbiddenError("Insufficient permissions", nil)
			HandleAPIError(c, apiErr)
			return
		}

		c.Next()
	}
}

// RequirePermissions returns a middleware that requires specific permissions
func RequirePermissions(permissions ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("requiredPermissions", permissions)
		c.Next()
	}
}

// shouldSkipAuth checks if authentication should be skipped for a path
func shouldSkipAuth(path string, skipPaths, skipPathPrefixes []string) bool {
	// Check exact paths
	for _, skipPath := range skipPaths {
		if path == skipPath {
			return true
		}
	}

	// Check path prefixes
	for _, prefix := range skipPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}

	return false
}

// extractToken extracts the authentication token from the request
func extractToken(c *gin.Context, cfg AuthConfig) string {
	// Convert AuthConfig to TokenExtractorConfig
	extractorConfig := utilities.TokenExtractorConfig{
		HeaderName:     cfg.HeaderName,
		TokenType:      cfg.TokenType,
		QueryParamName: cfg.QueryParamName,
		CookieName:     cfg.CookieName,
	}

	return utilities.ExtractToken(c, extractorConfig)
}

// authMethodForToken classifies a credential string into a low-cardinality
// method label for the auth_attempts_total metric. The empty string maps
// to "missing" so the missing-token failure path still has a label and
// the dashboard's failure ratio includes those attempts. The "lsp_"
// prefix is the platform's standard API-key prefix (charts/.../values.yaml
// auth.apiKeyPrefix); the middleware does not have direct access to the
// configured prefix, so it pattern-matches on the standard literal. A
// future config plumbing would let admin-overridden prefixes also classify
// as "apikey" — for now those tokens classify as "jwt", which is benign:
// the dashboard's failure ratio is method-summed.
func authMethodForToken(token string) string {
	if token == "" {
		return "missing"
	}
	if utilities.IsAPIKey(token, "lsp_") {
		return "apikey"
	}
	return "jwt"
}

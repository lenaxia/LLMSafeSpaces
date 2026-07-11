// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestSecurityMiddleware_Headers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Maybe()

	router := gin.New()
	config := middleware.SecurityConfig{
		ContentSecurityPolicy: "default-src 'self'",
		ReferrerPolicy:        "strict-origin-when-cross-origin",
		RequireHTTPS:          false, // Disable HTTPS redirection for testing
		Development:           true,  // Set to development mode
	}

	router.Use(middleware.SecurityMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "success")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "default-src 'self'", w.Header().Get("Content-Security-Policy"))
	assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"))
	assert.Equal(t, "none", w.Header().Get("X-Permitted-Cross-Domain-Policies"))

	mockLogger.AssertExpectations(t)
}

func TestSecurityMiddleware_CORS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Maybe()

	router := gin.New()
	config := middleware.SecurityConfig{
		AllowedOrigins: []string{"https://example.com"},
		Development:    false,
	}

	router.Use(middleware.SecurityMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "success")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://example.com")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://example.com", w.Header().Get("Access-Control-Allow-Origin"))

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://evil.com")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))

	mockLogger.AssertExpectations(t)
}

func TestCSPReportingMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", "CSP violation report", mock.Anything).Once()

	router := gin.New()
	router.Use(middleware.CSPReportingMiddleware(mockLogger))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/csp-report", strings.NewReader(`{
		"csp-report": {
			"document-uri": "https://example.com",
			"blocked-uri": "https://evil.com/script.js",
			"violated-directive": "script-src",
			"effective-directive": "script-src",
			"original-policy": "default-src 'self'"
		}
	}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	mockLogger.AssertExpectations(t)
}

// TestSecurityMiddleware_TrustsXForwardedProto pins the contract that when
// the API runs behind a TLS-terminating reverse proxy (the production
// deployment shape), an inbound request carrying X-Forwarded-Proto=https
// must NOT be redirected to itself even though SSLRedirect is enabled.
//
// Regression: without SSLProxyHeaders set, every API request behind
// traefik/nginx-ingress hit a 301 self-redirect, producing the
// "ERR_TOO_MANY_REDIRECTS" / "Maximum redirects followed" symptom seen
// against safespace.thekao.cloud (worklog 2026-06-01).
func TestSecurityMiddleware_TrustsXForwardedProto(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Maybe()

	router := gin.New()
	config := middleware.SecurityConfig{
		RequireHTTPS: true,
		Development:  false,
	}
	router.Use(middleware.SecurityMiddleware(mockLogger, config))
	router.GET("/api/v1/auth/config", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	t.Run("X-Forwarded-Proto=https reaches handler", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/v1/auth/config", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code,
			"request with X-Forwarded-Proto=https must not be redirected; "+
				"got status %d location %q", w.Code, w.Header().Get("Location"))
		assert.Equal(t, "ok", w.Body.String())
	})

	t.Run("plain HTTP without forwarded header still redirects", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/api/v1/auth/config", nil)
		// No X-Forwarded-Proto: simulates a direct HTTP request that
		// bypassed the ingress. SSLRedirect must still fire.
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusMovedPermanently, w.Code)
	})
}

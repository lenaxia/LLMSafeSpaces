// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

// rate_limit_burst_test.go — US-46.12: Rate Limit bursting behavior tests.
//
// These tests cover the bursting behavior documented in MISSINGTESTS.md
// item 6: burst allowance honored, burst exceeded → 429, and the IP
// fallback path when no API key is in context (rate_limit.go:98-104).
//
// The existing rate_limit_test.go tests basic limit enforcement (limit=2,
// 3rd request denied) but does NOT exercise:
//   - Burst allowance distinctly from the limit
//   - IP fallback key derivation when apiKey is absent
//   - Window reset behavior (after window, counter resets)

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// TestRateLimitBurst_BurstAllowanceHonoured verifies that the burst size
// is respected independently of the rate limit. With limit=10 and burst=5,
// the first 5 requests should all succeed (burst allowance), and the 6th
// should be denied.
func TestRateLimitBurst_BurstAllowanceHonoured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	hashedKey := utilities.HashString("burst-key")
	mockRL := new(mocks.MockRateLimiterService)

	// Burst=5: first 5 calls return true (allowed), 6th returns false
	for i := 0; i < 5; i++ {
		mockRL.On("Allow", hashedKey, mock.Anything, 5).Return(true).Once()
	}
	mockRL.On("Allow", hashedKey, mock.Anything, 5).Return(false).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  10,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     5,
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "burst-key")
		c.Next()
	})
	router.Use(middleware.RateLimitMiddleware(mockRL, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// First 5 requests should succeed (burst allowance)
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed (within burst)", i+1)
	}

	// 6th request should be rate limited
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "request 6 should be rate limited (burst exceeded)")

	mockRL.AssertExpectations(t)
}

// TestRateLimitBurst_BurstExceeded_Returns429 verifies that exceeding the
// burst returns 429 with correct X-RateLimit headers.
func TestRateLimitBurst_BurstExceeded_Returns429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	hashedKey := utilities.HashString("test-burst")
	mockRL := new(mocks.MockRateLimiterService)
	// Deny immediately (burst already exhausted)
	mockRL.On("Allow", hashedKey, mock.Anything, 3).Return(false).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  100,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     3,
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "test-burst")
		c.Next()
	})
	router.Use(middleware.RateLimitMiddleware(mockRL, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "100", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Reset"))
}

// TestRateLimitBurst_IPFallback_WhenNoAPIKey verifies the IP fallback path
// (rate_limit.go:98-104): when no apiKey is in context, the rate limiter
// uses c.ClientIP() as the key instead.
func TestRateLimitBurst_IPFallback_WhenNoAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	// The key should be the hash of the client IP, NOT the hash of an API key.
	expectedIP := "192.168.1.100"
	hashedIP := utilities.HashString(expectedIP)

	mockRL := new(mocks.MockRateLimiterService)
	mockRL.On("Allow", hashedIP, mock.Anything, mock.Anything).Return(true).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  10,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     5,
	}

	router := gin.New()
	// NO apiKey set in context — forces IP fallback
	router.Use(middleware.RateLimitMiddleware(mockRL, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = expectedIP + ":12345"
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "request should succeed with IP-based key")
	mockRL.AssertExpectations(t)
}

// TestRateLimitBurst_WindowReset verifies window reset behavior: after the
// rate limiter allows requests up to the limit, a subsequent Allow call
// returns true again (simulating window reset in the backend).
func TestRateLimitBurst_WindowReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	hashedKey := utilities.HashString("reset-key")
	mockRL := new(mocks.MockRateLimiterService)

	// Phase 1: 2 requests allowed, 3rd denied (limit reached)
	mockRL.On("Allow", hashedKey, mock.Anything, 2).Return(true).Once()
	mockRL.On("Allow", hashedKey, mock.Anything, 2).Return(true).Once()
	mockRL.On("Allow", hashedKey, mock.Anything, 2).Return(false).Once()
	// Phase 2: after window reset, request allowed again
	mockRL.On("Allow", hashedKey, mock.Anything, 2).Return(true).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  2,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     2,
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "reset-key")
		c.Next()
	})
	router.Use(middleware.RateLimitMiddleware(mockRL, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// Phase 1: exhaust the limit
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should succeed", i+1)
	}
	// 3rd request denied
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "3rd request should be denied")

	// Phase 2: after window reset (mock returns true), request succeeds
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "request after window reset should succeed")

	mockRL.AssertExpectations(t)
}

// TestRateLimitBurst_CustomBurstOverride verifies that CustomBursts in the
// config overrides the default burst for a specific key.
func TestRateLimitBurst_CustomBurstOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	customKey := "vip-client"
	hashedKey := utilities.HashString(customKey)
	mockRL := new(mocks.MockRateLimiterService)

	// VIP client has burst=10; first 10 allowed, 11th denied
	for i := 0; i < 10; i++ {
		mockRL.On("Allow", hashedKey, mock.Anything, 10).Return(true).Once()
	}
	mockRL.On("Allow", hashedKey, mock.Anything, 10).Return(false).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  5,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     3, // default burst
		CustomBursts:  map[string]int{customKey: 10},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", customKey)
		c.Next()
	})
	router.Use(middleware.RateLimitMiddleware(mockRL, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "VIP request %d should succeed", i+1)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "VIP request 11 should be denied")

	mockRL.AssertExpectations(t)
}

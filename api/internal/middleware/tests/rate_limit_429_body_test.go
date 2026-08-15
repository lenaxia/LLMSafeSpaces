// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestRateLimitMiddleware_429BodyContract pins the 429 response body to the
// API-wide error contract: {"error": "<string>", "limit": <int>, "retryAfter":
// <int seconds>}. Every other middleware and handler (email.go, dev_preview.go,
// workspace_access.go) emits "error" as a string; the Go SDK's parseError and
// the frontend ApiClientError both decode it as a string. The object shape
// {"error": {code, message, details}} silently breaks both consumers and the
// s-error-format canary contract ("all error values are strings").
func TestRateLimitMiddleware_429BodyContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything).Maybe()

	hashedKey := utilities.HashString("test-key")

	mockRateLimiter := new(mocks.MockRateLimiterService)
	mockRateLimiter.On("Allow", hashedKey, mock.Anything, 2).Return(true).Twice()
	mockRateLimiter.On("Allow", hashedKey, mock.Anything, 2).Return(false).Once()

	config := middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  2,
		DefaultWindow: time.Minute,
		Strategy:      "token_bucket",
		BurstSize:     2,
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("apiKey", "test-key")
		c.Next()
	})
	router.Use(middleware.RateLimitMiddleware(mockRateLimiter, mockLogger, config, nil))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "success")
	})

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body is not valid JSON: %v (body: %s)", err, w.Body.String())
	}

	errStr, isString := body["error"].(string)
	assert.True(t, isString, `body["error"] must be a string, got %T (%v)`, body["error"], body["error"])
	assert.NotEmpty(t, errStr, `body["error"] must be non-empty`)

	limitNum, isNumber := body["limit"].(float64)
	assert.True(t, isNumber, `body["limit"] must be a number, got %T (%v)`, body["limit"], body["limit"])
	assert.Equal(t, float64(2), limitNum)

	retryAfter, isNumber := body["retryAfter"].(float64)
	assert.True(t, isNumber, `body["retryAfter"] must be a number, got %T (%v)`, body["retryAfter"], body["retryAfter"])
	assert.Greater(t, retryAfter, float64(0), `body["retryAfter"] must be positive`)

	mockRateLimiter.AssertExpectations(t)
	mockLogger.AssertExpectations(t)
}

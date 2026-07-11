// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// newMiniredisRateLimiter builds a real ratelimit.Service backed by miniredis
// so the per-route middleware exercises the full bucket logic.
func newMiniredisRateLimiter(t *testing.T) (*ratelimit.Service, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	log, _ := apilogger.New(true, "error", "console")
	svc := ratelimit.NewWithRedisClient(log, client)
	return svc, func() {
		_ = svc.Stop()
		_ = client.Close()
		mr.Close()
	}
}

func perRouteTestLogger() *logmock.MockLogger {
	l := logmock.NewMockLogger()
	l.On("Warn", mock.Anything, mock.Anything).Maybe()
	l.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	return l
}

// TestPerRouteRateLimit_AppliesStricterLimitToProtectedPath is the G35
// core: a request to /account/recover must hit a SEPARATE, stricter
// bucket than the global 100/min limiter. Without separate buckets,
// a user could spend 99 of their 100 global requests on /recover
// before the gate tripped — defeating the per-endpoint limit.
//
// Concretely: with the per-route limit set to 3/min and burst 3, the
// 4th request in the same second must 429. Meanwhile, requests to an
// UNPROTECTED path (/something-else) must still pass.
func TestPerRouteRateLimit_AppliesStricterLimitToProtectedPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, cleanup := newMiniredisRateLimiter(t)
	defer cleanup()

	cfg := PerRouteRateLimitConfig{
		Enabled: true,
		Routes: map[string]RouteRateLimit{
			"/api/v1/account/recover": {
				Limit:  3,
				Burst:  3,
				Window: time.Minute,
			},
		},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		// Set a deterministic identity so the test is repeatable.
		c.Set("apiKey", "test-key")
		c.Next()
	})
	router.Use(PerRouteRateLimitMiddleware(svc, perRouteTestLogger(), cfg))
	router.POST("/api/v1/account/recover", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.GET("/other", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// 3 requests to /recover pass
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/account/recover", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "recover request %d should pass", i+1)
	}

	// 4th request to /recover 429s
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/account/recover", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "4th recover request should be rate-limited")

	// An unprotected path is unaffected
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/other", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "unprotected path should not be rate-limited")
}

// TestPerRouteRateLimit_BucketsAreIsolatedPerPath confirms that
// requests to two different protected paths consume SEPARATE buckets.
// Without this, a user could deplete one path's budget with requests
// to another (defeating the per-endpoint limit).
func TestPerRouteRateLimit_BucketsAreIsolatedPerPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, cleanup := newMiniredisRateLimiter(t)
	defer cleanup()

	cfg := PerRouteRateLimitConfig{
		Enabled: true,
		Routes: map[string]RouteRateLimit{
			"/api/v1/account/recover": {Limit: 2, Burst: 2, Window: time.Minute},
			"/api/v1/secrets/reveal":  {Limit: 2, Burst: 2, Window: time.Minute},
		},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("apiKey", "user-x"); c.Next() })
	router.Use(PerRouteRateLimitMiddleware(svc, perRouteTestLogger(), cfg))
	router.POST("/api/v1/account/recover", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	router.POST("/api/v1/secrets/reveal", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Exhaust /recover bucket
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/account/recover", nil)
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}

	// /secrets/reveal still has its own budget
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/secrets/reveal", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "secrets/reveal should have its own bucket")
}

// TestPerRouteRateLimit_DisabledWhenConfigDisabled confirms the
// middleware is a no-op when Enabled=false. Production can disable
// per-route limiting without removing the middleware from the chain.
func TestPerRouteRateLimit_DisabledWhenConfigDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, cleanup := newMiniredisRateLimiter(t)
	defer cleanup()

	cfg := PerRouteRateLimitConfig{
		Enabled: false,
		Routes: map[string]RouteRateLimit{
			"/api/v1/account/recover": {Limit: 1, Burst: 1, Window: time.Minute},
		},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("apiKey", "test"); c.Next() })
	router.Use(PerRouteRateLimitMiddleware(svc, perRouteTestLogger(), cfg))
	router.POST("/api/v1/account/recover", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/account/recover", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should pass when middleware disabled", i)
	}
}

// TestPerRouteRateLimit_UnprotectedPathsPassThrough confirms the
// middleware does NOT touch requests to paths that aren't in the
// Routes map. The global rate limiter (applied separately) handles
// those — this middleware only adds per-route stricter limits.
func TestPerRouteRateLimit_UnprotectedPathsPassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, cleanup := newMiniredisRateLimiter(t)
	defer cleanup()

	cfg := PerRouteRateLimitConfig{
		Enabled: true,
		Routes: map[string]RouteRateLimit{
			"/api/v1/account/recover": {Limit: 1, Burst: 1, Window: time.Minute},
		},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("apiKey", "test"); c.Next() })
	router.Use(PerRouteRateLimitMiddleware(svc, perRouteTestLogger(), cfg))
	router.GET("/anywhere", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for i := 0; i < 50; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/anywhere", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "unprotected request %d should pass", i)
	}
}

// TestPerRouteRateLimit_NilServiceIsNoOp confirms the middleware
// degrades gracefully when no rate limiter is wired (e.g. test
// harnesses, alternative deployments). Mirrors the global
// RateLimitMiddleware's nil-service guard.
func TestPerRouteRateLimit_NilServiceIsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := PerRouteRateLimitConfig{
		Enabled: true,
		Routes: map[string]RouteRateLimit{
			"/api/v1/account/recover": {Limit: 1, Burst: 1, Window: time.Minute},
		},
	}

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("apiKey", "test"); c.Next() })
	router.Use(PerRouteRateLimitMiddleware(nil, perRouteTestLogger(), cfg))
	router.POST("/api/v1/account/recover", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/v1/account/recover", nil)
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "nil service should be no-op (request %d)", i)
	}
}

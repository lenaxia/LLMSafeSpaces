// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

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

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
)

// TestRouter_G41_RevealSecretRateLimited is the G41 wiring regression.
// Mirrors TestRouter_G35_RecoverAccountRateLimited but for the
// /api/v1/secrets/:id/reveal endpoint. Without per-route limiting, a
// single IP can attempt 100 password guesses per minute against the
// reveal endpoint's re-authentication gate.
//
// This test uses a stub handler for the reveal route — we don't need
// the real SecretsHandler; we just need the route registered with the
// pattern the per-route middleware looks up. The rate limiter catches
// the (Burst+1)-th request before the handler runs.
func TestRouter_G41_RevealSecretRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	log, _ := apilogger.New(true, "error", "console")
	rlSvc := ratelimit.NewWithRedisClient(log, client)
	defer rlSvc.Stop()

	authSvc := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	dbSvc := &imocks.MockDatabaseService{}
	caSvc := &imocks.MockCacheService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	authSvc.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	authSvc.On("GetUserID", mock.Anything).Return("").Maybe()

	svc := &g41MockServices{
		auth: authSvc, metrics: met, db: dbSvc, cache: caSvc, rl: rlSvc,
	}

	// Use DefaultRouterConfig so the test asserts against production
	// wiring. Tighten the reveal route limit locally so we don't have
	// to fire 5 requests.
	cfg := DefaultRouterConfig()
	cfg.PerRouteRateLimitConfig.Routes["/api/v1/secrets/:id/reveal"] = middleware.RouteRateLimit{
		Limit:  3,
		Burst:  3,
		Window: time.Minute,
	}
	cfg.SecurityConfig.RequireHTTPS = false
	cfg.SecurityConfig.Development = true

	router := NewRouter(svc, log, nil, cfg)
	// Register a stub handler so c.FullPath() returns the pattern
	// "/api/v1/secrets/:id/reveal" that the per-route middleware
	// looks up. We don't need the real SecretsHandler — the rate
	// limiter catches the request before the handler runs.
	router.POST("/api/v1/secrets/:id/reveal", func(c *gin.Context) {
		c.String(http.StatusOK, "stub")
	})

	// Fire 5 POSTs from one IP. First 3 must NOT be 429; remaining 2 must be 429.
	passed := 0
	rejected := 0
	codes := []int{}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets/sec-1/reveal", nil)
		req.RemoteAddr = "10.0.0.42:1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
		switch rec.Code {
		case http.StatusTooManyRequests:
			rejected++
		default:
			passed++
		}
	}
	t.Logf("status codes: %v", codes)

	assert.Equal(t, 3, passed, "burst (3) requests should pass the rate limiter")
	assert.Equal(t, 2, rejected, "remaining (2) requests should be rate-limited (429)")
}

// g41MockServices mirrors g35MockServices with a distinct name to
// avoid collision in the server test package.
type g41MockServices struct {
	auth    *imocks.MockAuthMiddlewareService
	metrics *imocks.MockMetricsService
	db      *imocks.MockDatabaseService
	cache   *imocks.MockCacheService
	rl      *ratelimit.Service
}

func (s *g41MockServices) GetAuth() interfaces.AuthService               { return s.auth }
func (s *g41MockServices) GetDatabase() interfaces.DatabaseService       { return s.db }
func (s *g41MockServices) GetCache() interfaces.CacheService             { return s.cache }
func (s *g41MockServices) GetMetrics() interfaces.MetricsService         { return s.metrics }
func (s *g41MockServices) GetWorkspace() interfaces.WorkspaceService     { return nil }
func (s *g41MockServices) GetRateLimiter() interfaces.RateLimiterService { return s.rl }
func (s *g41MockServices) GetMetering() interfaces.MeteringService       { return nil }

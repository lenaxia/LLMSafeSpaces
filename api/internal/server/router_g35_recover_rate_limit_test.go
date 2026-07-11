// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
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

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestRouter_G35_RecoverAccountRateLimited is the G35 wiring regression:
// the router as built by NewRouter must apply the per-route rate limit
// to POST /api/v1/account/recover. Without this, the recover endpoint
// only sees the global 100/min limiter — a single IP can attempt 100
// recovery-key guesses per minute per IP, and a botnet can multiply
// that. The 20/min + burst-5 cap (from authRatePerMinute/authRateBurst
// constants, previously dead code) closes the gap.
//
// This test exercises the wiring through the real router construction
// path (NewRouter with DefaultRouterConfig), not a hand-rolled gin
// chain — proving the route is registered and the middleware is in the
// stack, not just that the middleware works in isolation.
func TestRouter_G35_RecoverAccountRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Real rate limiter backed by miniredis so the bucket persists
	// across requests within the test.
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

	svc := &g35MockServices{
		auth:    authSvc,
		metrics: met,
		db:      dbSvc,
		cache:   caSvc,
		rl:      rlSvc,
	}

	// Use DefaultRouterConfig so the test asserts against production
	// wiring. Tighten the recover route limit locally so we don't have
	// to fire 20 requests — the wiring under test is "this route has a
	// stricter limit than global", not "the specific numeric default".
	cfg := DefaultRouterConfig()
	cfg.PerRouteRateLimitConfig.Routes["/api/v1/account/recover"] = middleware.RouteRateLimit{
		Limit:  3,
		Burst:  3,
		Window: time.Minute,
	}
	// Relax HTTPS requirement for the test — httptest.NewRequest is
	// plain HTTP without X-Forwarded-Proto, so DefaultSecurityConfig's
	// SSLRedirect (true in production) would 301 the request before it
	// reaches the rate limiter. The rate-limit wiring is what's under
	// test here, not TLS termination.
	cfg.SecurityConfig.RequireHTTPS = false
	cfg.SecurityConfig.Development = true
	// RotateKeyHandler MUST be non-nil — otherwise the route is never
	// registered (router.go: if cfg.RotateKeyHandler != nil { ... })
	// and c.FullPath() returns "" so PerRouteRateLimitMiddleware skips.
	// We don't care about the handler's behavior — the rate limiter
	// rejects the (Burst+1)-th request before the handler runs.
	cfg.RotateKeyHandler = handlers.NewRotateKeyHandler(g35NoopKeyRotator{})

	router := NewRouter(svc, log, nil, cfg)

	// Fire 5 POSTs to /account/recover from one IP. The first Burst (3)
	// must NOT be 429; the remaining 2 must be 429.
	passed := 0
	rejected := 0
	codes := []int{}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/account/recover", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
		switch rec.Code {
		case http.StatusTooManyRequests:
			rejected++
		default:
			// The handler returns 400 for empty body (no JSON); what
			// matters is that it's NOT 429 — the rate limiter let it
			// through to the handler.
			assert.NotEqual(t, http.StatusTooManyRequests, rec.Code,
				"request %d should not be rate-limited", i+1)
			passed++
		}
	}
	t.Logf("status codes: %v", codes)

	assert.Equal(t, 3, passed, "burst (3) requests should pass the rate limiter")
	assert.Equal(t, 2, rejected, "remaining (2) requests should be rate-limited (429)")
}

// g35MockServices is a minimal interfaces.Services for the router test.
// Identical to authMockServices except GetRateLimiter returns a real
// service instead of nil — we need the rate limiter active to exercise
// the G35 wiring.
type g35MockServices struct {
	auth    *imocks.MockAuthMiddlewareService
	metrics *imocks.MockMetricsService
	db      *imocks.MockDatabaseService
	cache   *imocks.MockCacheService
	rl      *ratelimit.Service
}

func (s *g35MockServices) GetAuth() interfaces.AuthService               { return s.auth }
func (s *g35MockServices) GetDatabase() interfaces.DatabaseService       { return s.db }
func (s *g35MockServices) GetCache() interfaces.CacheService             { return s.cache }
func (s *g35MockServices) GetMetrics() interfaces.MetricsService         { return s.metrics }
func (s *g35MockServices) GetWorkspace() interfaces.WorkspaceService     { return nil }
func (s *g35MockServices) GetRateLimiter() interfaces.RateLimiterService { return s.rl }
func (s *g35MockServices) GetMetering() interfaces.MeteringService       { return nil }

// g35NoopKeyRotator is a KeyRotator implementation that always errors.
// The rate limiter rejects the (Burst+1)-th request before the handler
// runs, so the handler's behavior is never exercised — but it must
// exist so the route is registered.
type g35NoopKeyRotator struct{}

func (g35NoopKeyRotator) RotateKeyWithPassword(_ context.Context, _ string, _ []byte, _ string, _ time.Duration) (secrets.RotationResult, error) {
	return secrets.RotationResult{}, errG35Unreached
}
func (g35NoopKeyRotator) ChangePassword(_ context.Context, _ string, _ string, _ []byte, _ []byte) error {
	return errG35Unreached
}
func (g35NoopKeyRotator) ResetWithRecoveryKey(_ context.Context, _ string, _ string, _ []byte) (string, error) {
	return "", errG35Unreached
}

var errG35Unreached = g35Errorf("handler should not be reached — rate limiter rejects first")

func g35Errorf(msg string) error { return &g35Error{msg: msg} }

type g35Error struct{ msg string }

func (e *g35Error) Error() string { return e.msg }

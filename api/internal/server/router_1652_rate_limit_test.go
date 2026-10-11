// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
)

// TestRouter_1652_AuthAccountLifecycleRateLimits is the #1652 item-2 wiring
// regression: POST /auth/register and the password-reset / verify-email
// endpoints get per-route caps in DefaultRouterConfig, mirroring the
// login/passkey/reveal pattern. Without them only the global 100/min/IP
// limiter applies; register + reset-request are DB-write endpoints (a token
// row per request), making them account-spam / token-table-flood vectors
// when Turnstile is off.
//
// /auth/register is ALWAYS registered by registerAuthRoutes, so it is
// exercised through the real handler chain (the mock auth service's Register
// is never reached — requests carry no body and fail binding with 400 after
// passing the limiter; a non-429 counts as "passed"). The password-reset and
// verify-email routes are only registered when their handlers are wired, so
// stubs are registered at the exact paths the per-route middleware matches
// (same strategy as TestRouter_G41_RevealSecretRateLimited).
func TestRouter_1652_AuthAccountLifecycleRateLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	paths := []struct {
		path     string
		realRout bool // registered by NewRouter itself (no stub)
	}{
		{path: "/api/v1/auth/register", realRout: true},
		{path: "/api/v1/auth/password-reset/request", realRout: false},
		{path: "/api/v1/auth/password-reset/confirm", realRout: false},
		{path: "/api/v1/auth/verify-email", realRout: false},
		{path: "/api/v1/auth/verify-email/resend", realRout: false},
	}

	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
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
			// Register is reached only by requests that PASS the limiter and
			// carry a valid body — the test sends none, so it must never fire.
			authSvc.On("Register", mock.Anything, mock.Anything).Times(0)

			svc := &g41MockServices{auth: authSvc, metrics: met, db: dbSvc, cache: caSvc, rl: rlSvc}

			// Use DefaultRouterConfig so the test asserts against PRODUCTION
			// wiring (the #1652 entries must live there), tightened locally so
			// the test fires 5 requests instead of the real caps.
			cfg := DefaultRouterConfig()
			routeCfg, ok := cfg.PerRouteRateLimitConfig.Routes[tc.path]
			require.True(t, ok, "DefaultRouterConfig must carry a per-route cap for %s (#1652 item 2)", tc.path)
			routeCfg.Limit = 3
			routeCfg.Burst = 3
			cfg.PerRouteRateLimitConfig.Routes[tc.path] = routeCfg
			cfg.SecurityConfig.RequireHTTPS = false
			cfg.SecurityConfig.Development = true

			router := NewRouter(svc, log, nil, cfg)
			if !tc.realRout {
				router.POST(tc.path, func(c *gin.Context) {
					c.String(http.StatusOK, "stub")
				})
			}

			// Fire 5 POSTs from one IP. First 3 (burst) must NOT be 429;
			// remaining 2 must be 429 — same shape as the G41 pin.
			passed, rejected := 0, 0
			for i := 0; i < 5; i++ {
				req := httptest.NewRequest(http.MethodPost, tc.path, nil)
				req.RemoteAddr = "10.0.0.43:1234"
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				switch rec.Code {
				case http.StatusTooManyRequests:
					rejected++
				default:
					passed++
				}
			}

			assert.Equal(t, 3, passed, "burst (3) requests should pass the per-route limiter")
			assert.Equal(t, 2, rejected, "remaining (2) requests should be rate-limited (429)")
		})
	}
}

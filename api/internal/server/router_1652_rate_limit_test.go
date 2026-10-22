// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
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

// TestRouter_HealthProbesNeverRateLimited pins the SDK-canary contract
// (#1825 red leg: "FAIL /livez: not rate-limited") and the k8s
// operational requirement: /livez and /readyz must NEVER answer 429 —
// a rate-limited liveness probe restarts healthy pods. The global
// limiter is tightened to (1,1); an ordinary route from the same IP
// proves the budget IS exhausted (429 OBSERVED — the control leg never
// assumes it) before the probes are checked. /livez must answer 200
// (the real handler); /readyz answers its typed 503 (the STUBBED mock
// pings return "unreachable"). The control leg is what proves the
// exemption — not an empty bucket — is what serves the probes.
func TestRouter_HealthProbesNeverRateLimited(t *testing.T) {
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
	// /readyz renders its typed 503 only when the pings ANSWER; an
	// unstubbed mock PANICS (testify mock failure) and gin's recovery
	// turns that into a 500 — the r1 false-green finding. Stub both
	// pings to fail deterministically: the probe reaches its own JSON.
	dbSvc.On("Ping", mock.Anything).Return(errors.New("db unreachable")).Maybe()
	caSvc.On("Ping", mock.Anything).Return(errors.New("cache unreachable")).Maybe()

	svc := &g41MockServices{auth: authSvc, metrics: met, db: dbSvc, cache: caSvc, rl: rlSvc}

	cfg := DefaultRouterConfig()
	// Production wiring must exempt the probes (#1825): they are IN the
	// exempt set, then the global limiter is tightened to prove the
	// exemption — not an empty bucket — is what serves them.
	var livezExempt, readyzExempt bool
	for _, p := range cfg.RateLimitConfig.ExemptPaths {
		if p == "/livez" {
			livezExempt = true
		}
		if p == "/readyz" {
			readyzExempt = true
		}
	}
	assert.True(t, livezExempt, "DefaultRouterConfig must exempt /livez from rate limiting (k8s liveness probes must never 429)")
	assert.True(t, readyzExempt, "DefaultRouterConfig must exempt /readyz from rate limiting")

	// (1, 1) token bucket: burst of one, refilling at 1 token/SECOND
	// (the strategy computes rate = limit; DefaultWindow is unused by
	// it — r3 finding). Exhaustion is therefore EMPIRICAL, not assumed:
	// the control loop below fires fast enough that the per-second
	// refill cannot keep pace, and it requires the observed 429 before
	// any probe assertion runs.
	cfg.RateLimitConfig.DefaultLimit = 1
	cfg.RateLimitConfig.BurstSize = 1
	cfg.RateLimitConfig.DefaultWindow = time.Hour
	cfg.SecurityConfig.RequireHTTPS = false
	cfg.SecurityConfig.Development = true

	router := NewRouter(svc, log, nil, cfg)

	// CONTROL — the budget must actually exhaust: after the first
	// pass, further /api/v1/auth/login hits from this IP answer 429.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "10.9.9.9:777"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req) // burst token consumed
	exhausted := false
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.RemoteAddr = "10.9.9.9:777"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			exhausted = true
			break
		}
	}
	require.True(t, exhausted, "control: the (1,1) budget must exhaust on an ordinary route — otherwise this test proves nothing")

	// The probes, from the SAME exhausted IP: /livez serves 200 (the
	// real handler, no dependencies); /readyz serves its typed 503 (the
	// STUBBED pings answer "unreachable" — see the mock setup: the r1
	// panic class is structurally impossible with the stubs in place).
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/livez", nil)
		req.RemoteAddr = "10.9.9.9:777"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "/livez must serve 200 with the per-IP budget exhausted (the #1825 canary failure)")
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		req.RemoteAddr = "10.9.9.9:777"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
			"/readyz must answer its deterministic 503 (stubbed pings = unreachable), never 429 and never a panic recovery")
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// RouteRateLimit is the per-route limit configuration applied by
// PerRouteRateLimitMiddleware to a specific path (matched by gin's
// FullPath, e.g. "/api/v1/account/recover").
//
// Semantics (intentionally correct, unlike the global limiter's
// pre-existing per-second confusion): `Limit` is the maximum number of
// requests per `Window` per identity (API-key or IP). The middleware
// converts to a per-second refill rate internally
// (`Limit / Window.Seconds()`) so a config of {Limit: 20, Window: 1m}
// actually enforces 20 per minute, not 20 per second.
type RouteRateLimit struct {
	Limit  int
	Burst  int
	Window time.Duration
}

// PerRouteRateLimitConfig configures the per-route rate limiter. It is
// INTENTIONALLY separate from the global RateLimitConfig: the global
// limiter applies a wide budget across every endpoint; this layer adds
// STRICTER limits to specific paths that warrant them (G35 —
// /account/recover; G41 — /secrets/:id/reveal; future endpoints).
//
// The two layers do NOT share buckets: this middleware keys its buckets
// by "<path>:<hashed-identity>" while the global middleware keys by
// "<hashed-identity>" alone. This isolation is the whole point —
// without it, a user could spend 99 of their 100 global requests on
// /recover before any per-endpoint gate tripped.
type PerRouteRateLimitConfig struct {
	Enabled bool
	Routes  map[string]RouteRateLimit
}

// PerRouteRateLimitMiddleware applies stricter rate limits to specific
// routes on top of the global RateLimitMiddleware. For paths not listed
// in cfg.Routes, it is a no-op (the global limiter handles them).
//
// Identity resolution mirrors the global middleware: API-key if
// available (set by AuthMiddleware on authenticated routes), else
// client IP. Anonymous endpoints like /account/recover always fall back
// to IP — this matches the threat model (per-source throttling).
//
// The underlying RateLimiterService.Allow(key, rate, burst) keys
// buckets on `key` alone, so this middleware MUST prefix the key with
// the route to get bucket isolation between routes (see
// ratelimit.go:Allow). The prefix is the FULL gin route pattern
// (e.g. "/api/v1/account/recover"), not the request URL — parameterised
// routes (/secrets/:id) share one bucket per route, which is the
// intended behavior (a user scanning IDs is rate-limited as one).
func PerRouteRateLimitMiddleware(rl interfaces.RateLimiterService, log pkginterfaces.LoggerInterface, cfg PerRouteRateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil || !cfg.Enabled {
			c.Next()
			return
		}

		// FullPath returns the gin route pattern ("/api/v1/account/recover")
		// or empty string if no route matched. Empty means 404 — let the
		// not-found handler deal with it; nothing to rate-limit.
		route := c.FullPath()
		if route == "" {
			c.Next()
			return
		}

		routeCfg, ok := cfg.Routes[route]
		if !ok {
			// Not a protected path — global limiter handles it.
			c.Next()
			return
		}

		// Resolve identity. API-key first (authenticated routes), else IP.
		// Matches RateLimitMiddleware's resolution at rate_limit.go:98-104.
		apiKey, exists := c.Get("apiKey")
		var identity string
		if exists {
			identity = apiKey.(string)
		} else {
			identity = c.ClientIP()
		}

		// Key prefix gives per-route bucket isolation. Hashed so the
		// underlying bucket map (in-process for token-bucket strategy)
		// never sees raw IPs/API-keys in metrics or logs.
		bucketKey := utilities.HashString(route + ":" + identity)

		// token_bucket is the only strategy the per-route layer uses;
		// the global middleware exposes more, but the per-route layer is
		// for short, sharp per-endpoint caps where token bucket is the
		// right shape (steady-state allowance + bounded burst).
		//
		// Rate conversion: `Limit` is per-`Window` (e.g. 20 per minute).
		// The token-bucket refill is per-second, so divide. A zero window
		// is treated as 1 second (defensive — the caller is responsible
		// for setting Window, but division by zero is worse than a
		// per-second fallback).
		windowSeconds := routeCfg.Window.Seconds()
		if windowSeconds <= 0 {
			windowSeconds = 1
		}
		rate := float64(routeCfg.Limit) / windowSeconds
		if !rl.Allow(bucketKey, rate, routeCfg.Burst) {
			if log != nil {
				log.Warn("Per-route rate limit exceeded",
					"route", route,
					"limit", strconv.Itoa(routeCfg.Limit),
					"burst", strconv.Itoa(routeCfg.Burst),
				)
			}
			resetTime := time.Now().Add(routeCfg.Window).Unix()
			c.Header("X-RateLimit-Limit", strconv.Itoa(routeCfg.Limit))
			c.Header("X-RateLimit-Remaining", "0")
			c.Header("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
			apiErr := errors.NewRateLimitError(
				fmt.Sprintf("Too many requests to %s", route),
				routeCfg.Limit,
				resetTime,
				nil,
			)
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error": gin.H{
					"code":    apiErr.Code,
					"message": apiErr.Message,
					"details": apiErr.Details,
				},
			})
			return
		}

		c.Next()
	}
}

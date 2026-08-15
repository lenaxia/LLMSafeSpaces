// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
)

type RateLimitConfig struct {
	Enabled       bool
	DefaultLimit  int
	DefaultWindow time.Duration
	BurstSize     int
	Strategy      string
	ExemptPaths   []string // path prefixes exempt from rate limiting (e.g. SSE endpoints)
	CustomLimits  map[string]int
	CustomBursts  map[string]int
}

func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		// G13 / RT-2.4 / RT-2.5 (Epic 17): the previous default was
		// `false`, leaving auth + secret endpoints completely
		// unthrottled. Pentest confirmed: 200 sequential api-key
		// validation calls returned 0 rate-limited responses,
		// allowing unbounded brute-force from any IP. Enabled by
		// default; operators can opt out via the instance-settings
		// `rateLimiting.enabled=false` knob.
		Enabled:       true,
		DefaultLimit:  100,
		DefaultWindow: time.Minute,
		BurstSize:     20,
		Strategy:      "token_bucket",
	}
}

func RateLimitMiddleware(rl interfaces.RateLimiterService, log pkginterfaces.LoggerInterface, config RateLimitConfig, instanceSettings *settings.InstanceService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil {
			c.Next()
			return
		}

		// Read runtime overrides from instance settings (cached, ~0 cost)
		enabled := config.Enabled
		limit := config.DefaultLimit
		burst := config.BurstSize
		strategy := config.Strategy
		window := config.DefaultWindow
		if instanceSettings != nil {
			if v, err := instanceSettings.GetBool(c.Request.Context(), settings.KeyRateLimitingEnabled.Name()); err == nil {
				enabled = v
			}
			if v, err := instanceSettings.GetInt(c.Request.Context(), settings.KeyRateLimitingDefaultLimit.Name()); err == nil && v > 0 {
				limit = v
			}
			if v, err := instanceSettings.GetInt(c.Request.Context(), settings.KeyRateLimitingBurstSize.Name()); err == nil && v > 0 {
				burst = v
			}
			if v, err := instanceSettings.GetString(c.Request.Context(), settings.KeyRateLimitingStrategy.Name()); err == nil && v != "" {
				strategy = v
			}
			if v, err := instanceSettings.GetInt(c.Request.Context(), settings.KeyRateLimitingWindowMinutes.Name()); err == nil && v > 0 {
				window = time.Duration(v) * time.Minute
			}
		}

		if !enabled {
			c.Next()
			return
		}

		// Apply window override to config copy for sub-functions
		effectiveConfig := config
		effectiveConfig.DefaultWindow = window

		// Skip rate limiting for exempt paths (e.g. long-lived SSE connections)
		reqPath := c.FullPath()
		for _, exempt := range config.ExemptPaths {
			if strings.HasSuffix(reqPath, exempt) || reqPath == exempt {
				c.Next()
				return
			}
		}

		apiKey, exists := c.Get("apiKey")
		var keyStr string
		if exists {
			keyStr = apiKey.(string)
		} else {
			keyStr = c.ClientIP()
		}
		hashedKey := utilities.HashString(keyStr)

		// Check for custom limits (per-key overrides from static config)
		if customLimit, ok := config.CustomLimits[keyStr]; ok {
			limit = customLimit
		}
		if customBurst, ok := config.CustomBursts[keyStr]; ok {
			burst = customBurst
		}

		var err error
		switch strategy {
		case "token_bucket":
			err = applyTokenBucketRateLimit(c, rl, hashedKey, limit, burst, log)
		case "fixed_window":
			err = applyFixedWindowRateLimit(c, rl, effectiveConfig, hashedKey, limit, log)
		case "sliding_window":
			err = applySlidingWindowRateLimit(c, rl, effectiveConfig, hashedKey, limit, log)
		case "":
			// Default to token bucket if no strategy specified
			err = applyTokenBucketRateLimit(c, rl, hashedKey, limit, burst, log)
		default:
			err = fmt.Errorf("unsupported rate limit strategy: %s", strategy)
		}

		if err != nil {
		if apiErr, ok := err.(*errors.APIError); ok && apiErr.Type == errors.ErrorTypeRateLimit {
			c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
			c.Header("X-RateLimit-Remaining", "0")
			c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(window).Unix(), 10))
			c.Header("Retry-After", strconv.Itoa(int((window+time.Second-1)/time.Second)))
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error":      apiErr.Message,
				"limit":      limit,
				"retryAfter": int((window + time.Second - 1) / time.Second),
			})
			return
		}
			_ = c.AbortWithError(http.StatusInternalServerError, err)
			return
		}

		c.Next()
	}
}

func applyTokenBucketRateLimit(c *gin.Context, rl interfaces.RateLimiterService, key string, limit, burst int, log pkginterfaces.LoggerInterface) error {
	// Calculate rate from limit (requests per second)
	rate := float64(limit)

	// Use the RateLimiterService.Allow method
	if !rl.Allow(key, rate, burst) {
		log.Warn("Rate limit exceeded",
			"hashed_key", key,
			"limit", strconv.Itoa(limit),
			"burst", strconv.Itoa(burst),
			"path", c.FullPath(),
		)
		resetTime := time.Now().Add(time.Second).Unix() // Approximate reset time
		return errors.NewRateLimitError("Too many requests", limit, resetTime, nil)
	}

	c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
	// Since we're not tracking tokens directly anymore, we can approximate remaining
	remaining := burst - 1 // Assume one token used
	if remaining < 0 {
		remaining = 0
	}
	c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
	return nil
}

func applyFixedWindowRateLimit(c *gin.Context, rl interfaces.RateLimiterService, config RateLimitConfig, key string, limit int, log pkginterfaces.LoggerInterface) error {
	counterKey := fmt.Sprintf("ratelimit:%s:fixed_window", key)

	count, err := rl.Increment(c.Request.Context(), counterKey, 1, config.DefaultWindow)
	if err != nil {
		log.Error("Failed to increment rate limit counter", err,
			"hashed_key", key,
			"key", counterKey,
		)
		return errors.NewInternalError("Rate limit service unavailable", err)
	}

	ttl, err := rl.GetTTL(c.Request.Context(), counterKey)
	if err != nil {
		log.Error("Failed to get rate limit TTL", err,
			"hashed_key", key,
			"key", counterKey,
		)
	}

	if count > int64(limit) {
		log.Warn("Rate limit exceeded",
			"hashed_key", key,
			"count", count,
			"limit", limit,
			"window", config.DefaultWindow.String(),
		)
		resetTime := time.Now().Add(ttl).Unix()
		return errors.NewRateLimitError("Too many requests", limit, resetTime, nil)
	}

	c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
	c.Header("X-RateLimit-Remaining", strconv.Itoa(limit-int(count)))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(ttl).Unix(), 10))
	return nil
}

func applySlidingWindowRateLimit(c *gin.Context, rl interfaces.RateLimiterService, config RateLimitConfig, key string, limit int, log pkginterfaces.LoggerInterface) error {
	now := time.Now().UnixNano()
	windowKey := fmt.Sprintf("ratelimit:%s:sliding_window", key)

	// Add current timestamp to the window
	err := rl.AddToWindow(c.Request.Context(), windowKey, now, strconv.FormatInt(now, 10), config.DefaultWindow)
	if err != nil {
		log.Error("Failed to add timestamp to rate limit window", err,
			"hashed_key", key,
			"key", windowKey,
		)
		return errors.NewInternalError("Rate limit service unavailable", err)
	}

	// Remove old timestamps
	cutoff := time.Now().Add(-config.DefaultWindow).UnixNano()
	err = rl.RemoveFromWindow(c.Request.Context(), windowKey, cutoff)
	if err != nil {
		log.Error("Failed to clean up rate limit window", err,
			"hashed_key", key,
			"key", windowKey,
		)
	}

	// Count remaining requests
	count, err := rl.CountInWindow(c.Request.Context(), windowKey, cutoff, now)
	if err != nil {
		log.Error("Failed to count rate limit window entries", err,
			"hashed_key", key,
			"key", windowKey,
		)
		return errors.NewInternalError("Rate limit service unavailable", err)
	}

	if count > limit {
		log.Warn("Rate limit exceeded",
			"hashed_key", key,
			"count", count,
			"limit", limit,
			"window", config.DefaultWindow.String(),
		)
		resetTime := time.Now().Add(config.DefaultWindow).Unix()
		return errors.NewRateLimitError("Too many requests", limit, resetTime, nil)
	}

	// Get remaining TTL for the window
	ttl, err := rl.GetTTL(c.Request.Context(), windowKey)
	if err != nil {
		log.Error("Failed to get rate limit window TTL", err,
			"hashed_key", key,
			"key", windowKey,
		)
	}

	c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
	c.Header("X-RateLimit-Remaining", strconv.Itoa(limit-count))
	c.Header("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(ttl).Unix(), 10))
	return nil
}

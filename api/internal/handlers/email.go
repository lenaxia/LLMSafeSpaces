// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
)

const (
	// testSendRateLimit is the per-admin cap on test-send calls per hour.
	// Guards against SES quota/cost abuse even from a compromised admin
	// account. US-49.4 requirement.
	testSendRateLimit = 5
	// testSendWindow is the rolling window for the test-send rate limit.
	testSendWindow = time.Hour
)

// rateCounter is the minimal subset of RateLimiterService the email handler
// needs. Declared here (not imported from interfaces) so the handler is
// testable with a fake without pulling the full interface. Caller-shaped.
type rateCounter interface {
	Increment(ctx context.Context, key string, value int64, expiration time.Duration) (int64, error)
}

// emailLogger is the minimal logging surface the email handler needs. Mirrors
// the invitationLogger shape so the same concrete loggers satisfy both.
type emailLogger interface {
	Error(msg string, err error, args ...any)
}

// EmailHandler exposes admin-only email operations (test-send). It wraps the
// EmailService which resolves the configured provider (SES/Noop).
type EmailHandler struct {
	svc    *emailsvc.Service
	rl     rateCounter
	log    emailLogger
	userID func(*gin.Context) string
}

// NewEmailHandler constructs an EmailHandler. svc must be non-nil. rl may be
// nil — when nil, the per-endpoint test-send rate limit is skipped (the
// global RateLimitMiddleware still applies); this is intended for tests and
// for deployments that prefer to rely solely on the global limiter. log may
// be nil in tests; when nil, send-failure errors are not logged (the mapped
// category is still returned to the caller).
func NewEmailHandler(svc *emailsvc.Service, rl rateCounter, log emailLogger) *EmailHandler {
	return &EmailHandler{svc: svc, rl: rl, log: log, userID: userIDFromContext}
}

// TestSend handles POST /api/v1/admin/email/test.
//
// Sends a test email to the given address so an admin can verify the SES
// wiring end-to-end. The endpoint is admin-only (wired behind AdminGuard in
// the router) and rate-limited to testSendRateLimit per admin per hour.
//
// Response contract:
//   - SES success:  200 { "sent": true,  "provider": "ses"  }
//   - Noop mode:    200 { "sent": false, "provider": "noop" }
//   - Rate limited: 429 { "error": "rate limit exceeded", "limit": 5 }
//   - Send failure: 502 { "error": "<mapped category>" }
//
// Noop mode reports sent=false so the admin knows no real email was delivered
// even though the call "succeeded" (NoopProvider logs to stderr and returns nil).
func (h *EmailHandler) TestSend(c *gin.Context) {
	var req struct {
		To string `json:"to" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to is required"})
		return
	}
	parsed, err := mail.ParseAddress(strings.TrimSpace(req.To))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to must be a valid email address"})
		return
	}
	addr := parsed.Address

	providerName := h.svc.ProviderName()

	// Rate limit applies even in noop mode so the contract is uniform and so
	// a noop-mode instance promoted to SES mid-flight doesn't suddenly allow
	// a backlog of queued requests through.
	if h.rl != nil {
		uid := h.userID(c)
		if uid != "" {
			count, err := h.rl.Increment(c.Request.Context(), "email:test-send:"+uid, 1, testSendWindow)
			if err == nil && count > testSendRateLimit {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"error": "test-send rate limit exceeded",
					"limit": testSendRateLimit,
				})
				return
			}
		}
	}

	if providerName == "noop" {
		c.JSON(http.StatusOK, gin.H{"sent": false, "provider": "noop"})
		return
	}

	if err := h.svc.SendTest(c.Request.Context(), addr); err != nil {
		if errors.Is(err, emailsvc.ErrNotConfigured) {
			c.JSON(http.StatusOK, gin.H{"sent": false, "provider": "noop"})
			return
		}
		if h.log != nil {
			h.log.Error("email test-send failed", err, "to", addr, "provider", providerName)
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": mapSESError(err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": true, "provider": providerName})
}

// userIDFromContext extracts the authenticated user ID from the Gin context
// (set by AuthMiddleware). Returns "" if unset.
func userIDFromContext(c *gin.Context) string {
	if v, ok := c.Get("userID"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mapSESError converts a raw provider error into a generic, actionable
// category for the admin. US-49.4 requires errors are mapped, not leaked
// verbatim — raw SES errors can contain AWS account IDs, region names, and
// internal infrastructure details. The mapping is conservative: anything
// unrecognized falls back to a generic "email send failed" with no detail.
// The caller is responsible for logging the original error before mapping;
// the generic fallback message points the admin to those logs.
func mapSESError(err error) string {
	if err == nil {
		return "email send failed"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not verified"),
		strings.Contains(msg, "email address is not verified"),
		strings.Contains(msg, "domain is not verified"):
		return "sender or recipient not verified in SES; verify the fromAddress domain in the configured region"
	case strings.Contains(msg, "credentials"),
		strings.Contains(msg, "no ec2"),
		strings.Contains(msg, "token"),
		strings.Contains(msg, "irsa"),
		strings.Contains(msg, "web identity"):
		return "SES credentials unavailable; check the IRSA role annotation on the API ServiceAccount"
	case strings.Contains(msg, "region"),
		strings.Contains(msg, "invalid region"):
		return "SES region misconfigured; check email.sesRegion"
	case strings.Contains(msg, "throttl"),
		strings.Contains(msg, "rate"),
		strings.Contains(msg, "quota"):
		return "SES throttled or quota exceeded; wait and retry, or request a quota increase"
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline"),
		strings.Contains(msg, "context canceled"):
		return "SES request timed out; check network egress and SES endpoint reachability"
	default:
		return "email send failed; check API server logs for details"
	}
}

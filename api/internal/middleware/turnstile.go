// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Turnstile is a gin middleware that validates the client-supplied
// Cloudflare Turnstile token against Cloudflare's siteverify endpoint
// before letting the request reach the wrapped handler.
//
// The token can be supplied in either of two places, in priority order:
//  1. Header  `cf-turnstile-response` (recommended for APIs).
//  2. Form/JSON field `cfTurnstileResponse` (client-side convenience).
//
// The middleware fails closed: any of {token missing, verify request
// fails, verify response says not-success} → HTTP 401 with a
// {"error":"turnstile_failed", "reason":<detail>} JSON body. It does
// NOT proceed to the wrapped handler on failure.
//
// When Turnstile is disabled at config time, callers must NOT install
// this middleware at all — there's no runtime `enabled` bypass here
// because a disabled middleware in the chain would just reject every
// request. The router should look at config.Turnstile.Enabled and only
// wire this on when true.
//
// Cloudflare's siteverify contract: <https://developers.cloudflare.com/turnstile/get-started/server-side-validation/>
//   POST application/x-www-form-urlencoded
//   {secret, response, remoteip?}
//   → 200 OK with JSON:
//     {"success":bool, "challenge_ts":..., "hostname":..., "error-codes":[...]}
//
// Timeout is 5s — Cloudflare's own SLO is well under 1s, but network
// blips + AWS→Cloudflare cross-region latency call for a generous
// upper bound. If verify times out, the middleware treats that as a
// failure (fail-closed).

// TurnstileConfig is the minimum surface the middleware needs.
// Zero-value SecretKey or VerifyURL results in a permanently-failing
// middleware (fail-closed).
type TurnstileConfig struct {
	SecretKey string
	VerifyURL string
	// Optional HTTP client override — tests substitute a stub.
	HTTPClient *http.Client
	// Optional logger override — tests use a nop; production wires the
	// service's zap logger.
	Logger *zap.Logger
}

const (
	turnstileHeader     = "cf-turnstile-response"
	turnstileFormField  = "cfTurnstileResponse"
	turnstileTimeoutSec = 5
)

// turnstileVerifyResponse is Cloudflare's siteverify response shape.
type turnstileVerifyResponse struct {
	Success     bool     `json:"success"`
	ChallengeTS string   `json:"challenge_ts,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	ErrorCodes  []string `json:"error-codes,omitempty"`
}

// Turnstile returns a middleware that enforces CAPTCHA validation.
func Turnstile(cfg TurnstileConfig) gin.HandlerFunc {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: turnstileTimeoutSec * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.VerifyURL == "" {
		cfg.VerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}

	return func(c *gin.Context) {
		token := extractTurnstileToken(c)
		if token == "" {
			respondTurnstileFail(c, "missing_token", "no cf-turnstile-response header or cfTurnstileResponse field")
			return
		}

		remoteIP := clientIP(c)

		ok, reason, err := verifyTurnstileToken(c.Request.Context(), cfg, token, remoteIP)
		if err != nil {
			// Network / parse error — log with detail but return a
			// generic reason to the client (don't leak backend errors).
			cfg.Logger.Warn("turnstile verify request failed",
				zap.String("reason", reason),
				zap.Error(err),
			)
			respondTurnstileFail(c, "verify_unavailable", "captcha verification service unavailable")
			return
		}
		if !ok {
			cfg.Logger.Info("turnstile verify rejected token",
				zap.String("reason", reason),
				zap.String("remote_ip", remoteIP),
			)
			respondTurnstileFail(c, "rejected", reason)
			return
		}

		c.Next()
	}
}

// extractTurnstileToken pulls the token from the request, header first
// then form/JSON field. Returns empty string if not found.
func extractTurnstileToken(c *gin.Context) string {
	if t := c.GetHeader(turnstileHeader); t != "" {
		return t
	}
	// JSON body: peek without consuming (using Gin's already-parsed body
	// via BindJSON would exhaust the reader). Callers who need to bind
	// the body after Turnstile can do so as long as they use
	// ShouldBindBodyWith / ShouldBindJSON (Gin caches once). To keep the
	// middleware simple and not-body-consuming, prefer the header path.
	if t := c.PostForm(turnstileFormField); t != "" {
		return t
	}
	return ""
}

// verifyTurnstileToken POSTs to Cloudflare's siteverify and interprets
// the response. Returns (ok, reason, err) — reason is a stable
// short-code suitable for logging (never client-facing PII).
func verifyTurnstileToken(ctx context.Context, cfg TurnstileConfig, token, remoteIP string) (bool, string, error) {
	if cfg.SecretKey == "" {
		return false, "no_secret_configured", nil
	}

	form := url.Values{}
	form.Set("secret", cfg.SecretKey)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.VerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, "request_build_failed", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return false, "http_error", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "body_read_failed", err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("http_%d", resp.StatusCode), fmt.Errorf("siteverify HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed turnstileVerifyResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, "json_parse_failed", err
	}
	if !parsed.Success {
		return false, strings.Join(parsed.ErrorCodes, ","), nil
	}
	return true, "", nil
}

// respondTurnstileFail writes the fail-closed response. Always 401.
// The `reason` code is a short stable string for client-side error UX
// (e.g. "missing_token" → "Please complete the CAPTCHA and try again").
// The `detail` is a longer human-readable string, useful for logs but
// safe to show users too (never contains secrets).
func respondTurnstileFail(c *gin.Context, reason, detail string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error":  "turnstile_failed",
		"reason": reason,
		"detail": detail,
	})
}

// clientIP returns the request's client IP, preferring the
// X-Forwarded-For header's leftmost entry (which Cloudflare and AWS
// LBC both populate). Falls back to gin's ClientIP() (which respects
// Gin's TrustedProxies config).
func clientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		// Leftmost = original client (before proxies).
		parts := strings.SplitN(xff, ",", 2)
		if ip := strings.TrimSpace(parts[0]); ip != "" && isValidIP(ip) {
			return ip
		}
	}
	if cf := c.GetHeader("CF-Connecting-IP"); cf != "" && isValidIP(cf) {
		return cf
	}
	return c.ClientIP()
}

func isValidIP(s string) bool {
	return net.ParseIP(s) != nil
}

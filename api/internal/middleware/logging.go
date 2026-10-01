// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	httputil "github.com/lenaxia/llmsafespaces/pkg/http"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
)

type LoggingConfig struct {
	// LogRequestBody indicates whether to log request bodies
	LogRequestBody bool

	// LogResponseBody indicates whether to log response bodies
	LogResponseBody bool

	// MaxBodyLogSize is the maximum size of request/response bodies to log
	MaxBodyLogSize int

	// SensitiveFields are JSON fields that should be redacted in request/response bodies.
	// Field-name matching is exact (case-sensitive). See pkg/utilities/masking.go.
	//
	// G25: "value" is intentionally included. The secrets endpoint carries
	// plaintext credentials in the "value" field; even though /api/v1/secrets/*
	// is in SkipPathPrefixes (defense in depth — bodies never logged at all
	// for that path), other endpoints may also pass through sensitive values
	// in a "value" field (env-var updates, settings updates with a secret
	// subtype, etc.). Masking "value" globally errs on the side of caution;
	// legitimate non-secret uses (e.g. settings PUT {"value":"20Gi"}) become
	// "********" in logs, which is acceptable for log readability.
	SensitiveFields []string

	// SkipPaths are exact paths that should not be logged at all
	// (typical use: liveness/readiness probes that flood logs).
	SkipPaths []string

	// SkipPathPrefixes are URL path prefixes that should not be logged
	// (G25). Prefix matching (not exact) so a single entry like
	// "/api/v1/secrets/" catches every secrets sub-path
	// (/api/v1/secrets/:id, /api/v1/secrets/:id/reveal, etc.). Bodies
	// on these paths can carry plaintext credentials in non-standard
	// fields; the safest policy is to not log them at all.
	SkipPathPrefixes []string
}

// DefaultLoggingConfig returns the default logging configuration
func DefaultLoggingConfig() LoggingConfig {
	return LoggingConfig{
		LogRequestBody:  true,
		LogResponseBody: true,
		MaxBodyLogSize:  1024, // 1KB
		SensitiveFields: []string{"password", "token", "secret", "key", "apiKey", "credit_card", "value"},
		SkipPaths:       []string{"/health", "/livez", "/readyz", "/metrics"},
		// G25: /api/v1/secrets/* bodies carry plaintext credentials in
		// the "value" field. Skip body+response logging entirely for
		// these paths. Combined with the "value" entry in
		// SensitiveFields, this is defense in depth — either layer
		// alone prevents the leak.
		//
		// Two prefix forms per resource so both the collection path
		// (/api/v1/secrets, no trailing slash — used by POST/GET) and
		// nested paths (/api/v1/secrets/:id/reveal) are caught.
		//
		// Note: prefix matching is not boundary-aware —
		// strings.HasPrefix("/api/v1/secretslist", "/api/v1/secrets")
		// would also match. We accept this because no such path exists
		// in this codebase AND the consequence of an accidental match
		// is "this path is not logged" (a debuggability loss, not a
		// correctness or security issue). If a future endpoint name
		// collides with one of these prefixes, scope the prefix more
		// tightly (e.g. switch to regex).
		SkipPathPrefixes: []string{
			"/api/v1/secrets",
			"/api/v1/secrets/",
			"/api/v1/account",
			"/api/v1/account/",
			// G25 defense-in-depth: auth responses carry JWTs and freshly-
			// issued API keys. Login request bodies carry passwords (already
			// masked by SensitiveFields "password" entry, but skipping the
			// response body entirely is safer — the JWT in the response is
			// a session credential, not just a random value).
			"/api/v1/auth",
			"/api/v1/auth/",
			// #1214: the pod-bootstrap internal endpoint (US-70.1
			// spawn-env pull) returns the FULL materialized credential
			// set in its response body — llm-provider keys, SSH private
			// keys, env-secret tokens, MCP bearers — the same plaintext
			// class as /api/v1/secrets/*. Skip it for the same reason;
			// the array-masker fix is the second layer, this is the first.
			"/internal/v1/pod-bootstrap",
			// G25 defense-in-depth: admin and org credential endpoints
			// accept provider API keys (OpenAI sk-..., Anthropic sk-ant-...,
			// etc.) in the "apiKey" JSON field. "apiKey" is in
			// SensitiveFields so the value is masked, but skipping the
			// body entirely is safer — a future field rename (e.g.
			// "api_key" snake_case) would silently unmask.
			"/api/v1/admin/provider-credentials",
			"/api/v1/admin/provider-credentials/",
		},
	}
}

func LoggingMiddleware(log interfaces.LoggerInterface, config ...LoggingConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultLoggingConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	return func(c *gin.Context) {
		// Skip logging for certain paths (exact match — typical use:
		// liveness/readiness probes).
		path := c.Request.URL.Path
		for _, skipPath := range cfg.SkipPaths {
			if path == skipPath {
				c.Next()
				return
			}
		}
		// G25: Skip logging entirely for path prefixes that carry
		// plaintext credentials in their bodies (secrets CRUD, account
		// key operations). The "value" field masking in
		// SensitiveFields is defense in depth; this is the primary
		// gate.
		for _, prefix := range cfg.SkipPathPrefixes {
			if strings.HasPrefix(path, prefix) {
				c.Next()
				return
			}
		}

		start := time.Now()
		// Read the request ID set by TracingMiddleware (which runs before
		// LoggingMiddleware in the router chain). Falls back to empty string
		// for requests that somehow bypass TracingMiddleware (e.g. tests that
		// wire LoggingMiddleware alone), which is safe — the field will be
		// omitted from log output rather than causing a panic.
		requestID := c.GetString("request_id")

		// Log request details
		logRequest(c, log, requestID, cfg)

		// Capture response
		writer := httputil.NewBodyCaptureWriter(c)
		c.Writer = writer

		// Process request
		c.Next()

		// Log response details
		logResponse(c, log, requestID, start, writer.GetBody(), cfg)
	}
}

func logRequest(c *gin.Context, log interfaces.LoggerInterface, requestID string, cfg LoggingConfig) {
	fields := []interface{}{
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"remote_addr", c.Request.RemoteAddr,
		"user_agent", c.Request.UserAgent(),
		"request_id", requestID,
	}

	if apiKey, exists := c.Get("apiKey"); exists {
		fields = append(fields, "api_key", utilities.MaskString(apiKey.(string)))
	}

	// Log request body if present and configured to do so
	if cfg.LogRequestBody && c.Request.Body != nil && c.Request.ContentLength > 0 {
		body, err := readAndReplaceBody(c)
		if err == nil {
			// Add content length
			fields = append(fields, "request_body_size", len(body))

			// Try to parse as JSON first
			var jsonBody map[string]interface{}
			if err := json.Unmarshal(body, &jsonBody); err == nil {
				// Create a copy of the map to avoid modifying the original
				maskedBody := make(map[string]interface{})
				for k, v := range jsonBody {
					maskedBody[k] = v
				}
				// Use the utilities.MaskSensitiveFieldsWithList function to mask sensitive fields
				utilities.MaskSensitiveFieldsWithList(maskedBody, cfg.SensitiveFields)
				fields = append(fields, "request_body", maskedBody)
			} else {
				// If not JSON or too large, truncate it
				if len(body) > cfg.MaxBodyLogSize {
					truncatedBody := string(body[:cfg.MaxBodyLogSize]) + "... (truncated)"
					fields = append(fields, "request_body", truncatedBody)
				} else {
					fields = append(fields, "request_body", string(body))
				}
			}
		} else {
			// The capture stopped at requestBodyCaptureLimit (or the
			// client died mid-read): log the declared size, never the
			// partial bytes — the truncated-string branch would bypass
			// JSON field masking.
			size := c.Request.ContentLength
			if size < 0 {
				size = 0
			}
			fields = append(fields, "request_body_size", int(size))
		}
	}

	log.Info("Request received", fields...)
}

func logResponse(c *gin.Context, log interfaces.LoggerInterface, requestID string, start time.Time, responseBody string, cfg LoggingConfig) {
	duration := time.Since(start)
	fields := []interface{}{
		"status", c.Writer.Status(),
		"duration", duration.String(),
		"response_size", c.Writer.Size(),
		"request_id", requestID,
	}

	// Include user_id when available (set by auth middleware for authenticated routes).
	if userID := c.GetString("userID"); userID != "" {
		fields = append(fields, "user_id", userID)
	}

	// Log response body if configured to do so and either:
	// 1. It's an error response (status >= 400)
	// 2. LogResponseBody is true for all responses
	if (cfg.LogResponseBody || c.Writer.Status() >= 400) && responseBody != "" {
		// Try to parse as JSON first
		var jsonBody map[string]interface{}
		if err := json.Unmarshal([]byte(responseBody), &jsonBody); err == nil {
			// Create a copy of the map to avoid modifying the original
			maskedBody := make(map[string]interface{})
			for k, v := range jsonBody {
				maskedBody[k] = v
			}
			// Use the utilities.MaskSensitiveFieldsWithList function to mask sensitive fields
			utilities.MaskSensitiveFieldsWithList(maskedBody, cfg.SensitiveFields)
			// #1214: the size cap previously applied ONLY to the
			// non-JSON branch — a JSON body was masked and logged with
			// no bound (observed ~7KB credential-bearing lines). Marshal
			// the masked copy and truncate the same way.
			if maskedJSON, err := json.Marshal(maskedBody); err == nil && len(maskedJSON) > cfg.MaxBodyLogSize {
				truncated := string(maskedJSON[:cfg.MaxBodyLogSize]) + "... (truncated)"
				fields = append(fields, "response_body", truncated)
			} else {
				fields = append(fields, "response_body", maskedBody)
			}
		} else {
			// If not JSON or too large, truncate it
			if len(responseBody) > cfg.MaxBodyLogSize {
				truncatedBody := responseBody[:cfg.MaxBodyLogSize] + "... (truncated)"
				fields = append(fields, "response_body", truncatedBody)
			} else {
				fields = append(fields, "response_body", responseBody)
			}
		}
	}

	log.Info("Request completed", fields...)
}

// requestBodyCaptureLimit bounds how much of a request body the
// logging and error-handler middlewares read before the handler runs.
// The bound is load-bearing, not cosmetic:
//
//   - The upload endpoint's design contract (0060, uploads.go) is that
//     the client's body is NEVER buffered — it streams through to
//     agentd, whose admission (Admit → stageStream) must observe the
//     client's pace. An unbounded pre-handler read swallows the whole
//     transfer first: a client-paced 1MiB trickle at 64k/s held
//     NOTHING at agentd for its entire 16s transfer because the
//     handler (and the agentd forward) only started after the last
//     byte arrived — the nightly SR-6B row failed 10/10 on exactly
//     this (runs 36135708380..36749521434; instrumented proof run
//     36749714653: holders "Request received" at the API only at
//     T+16.0s, the 5th full-speed upload admitted into the empty cap).
//   - The middlewares run BEFORE auth, on every route: an unbounded
//     ReadAll is an unauthenticated memory-exhaustion amplifier — any
//     body size is fully buffered before any handler-side cap applies.
//
// The capture replays through io.MultiReader, so bodies larger than
// the limit stream the remainder live to the handler.
const requestBodyCaptureLimit = int64(4 << 10)

// captureRequestBody reads at most limit bytes of the request body and
// replaces c.Request.Body with a stream that replays the captured
// prefix followed by the unread remainder. It returns the captured
// bytes and whether the WHOLE body was read (no remainder).
func captureRequestBody(c *gin.Context, limit int64) ([]byte, bool) {
	rest := c.Request.Body
	captured, err := io.ReadAll(io.LimitReader(rest, limit+1))
	c.Request.Body = newStreamedBody(captured, rest)
	if err != nil {
		return captured, false
	}
	return captured, int64(len(captured)) <= limit
}

// streamedBody is the capture-replay body: the captured prefix from
// memory, the remainder live from the original; Close propagates.
type streamedBody struct {
	io.Reader
	orig io.Closer
}

func newStreamedBody(captured []byte, rest io.ReadCloser) *streamedBody {
	return &streamedBody{
		Reader: io.MultiReader(bytes.NewReader(captured), rest),
		orig:   rest,
	}
}

func (b *streamedBody) Close() error { return b.orig.Close() }

func readAndReplaceBody(c *gin.Context) ([]byte, error) {
	body, complete := captureRequestBody(c, requestBodyCaptureLimit)
	if !complete {
		return nil, errBodyCaptureIncomplete
	}
	return body, nil
}

// errBodyCaptureIncomplete reports that the capture stopped at the
// limit (or a short read) — callers treat it as "no body to log", never
// as a request error: the handler's stream is unaffected.
var errBodyCaptureIncomplete = errors.New("request body capture incomplete")

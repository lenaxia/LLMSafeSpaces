// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	emailsvc "github.com/lenaxia/llmsafespaces/api/internal/services/email"
	"github.com/lenaxia/llmsafespaces/pkg/email"
)

// fakeEmailProvider captures the last message and returns the configured
// error so tests can exercise send-failure paths.
type fakeEmailProvider struct {
	last *email.Message
	err  error
}

func (f *fakeEmailProvider) Send(_ context.Context, msg email.Message) error {
	f.last = &msg
	return f.err
}

type testSendErr struct{ msg string }

func (e *testSendErr) Error() string { return e.msg }

// fakeRateCounter is a minimal rateCounter for the email handler. It
// increments an in-memory map keyed by the limiter key and returns the
// current count, emulating the Redis Increment semantics.
type fakeRateCounter struct {
	counts map[string]int64
}

func newFakeRateCounter() *fakeRateCounter {
	return &fakeRateCounter{counts: map[string]int64{}}
}

func (f *fakeRateCounter) Increment(_ context.Context, key string, _ int64, _ time.Duration) (int64, error) {
	f.counts[key]++
	return f.counts[key], nil
}

func (f *fakeRateCounter) set(key string, n int64) { f.counts[key] = n }

// captureLogger records Error calls so tests can assert the raw error is
// logged before mapping (PR review finding: no swallowed errors).
type captureLogger struct {
	errors []logEntry
}
type logEntry struct {
	msg string
	err error
}

func (l *captureLogger) Error(msg string, err error, _ ...any) {
	l.errors = append(l.errors, logEntry{msg: msg, err: err})
}

const testUserID = "admin-1"

func setupEmailRouter(h *EmailHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Simulate AuthMiddleware populating userID so the rate limiter can key
	// on it. In production this is set by AuthMiddleware before the handler.
	r.Use(func(c *gin.Context) { c.Set("userID", testUserID); c.Next() })
	r.POST("/admin/email/test", h.TestSend)
	return r
}

func TestEmailHandler_TestSend_SESSuccess(t *testing.T) {
	fp := &fakeEmailProvider{}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["sent"])
	assert.Equal(t, "ses", resp["provider"])
}

func TestEmailHandler_TestSend_NoopReportsNotSent(t *testing.T) {
	svc := emailsvc.NewService(&email.NoopProvider{}, "https://app.test", "")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["sent"])
	assert.Equal(t, "noop", resp["provider"])
}

func TestEmailHandler_TestSend_MissingTo_Rejected(t *testing.T) {
	svc := emailsvc.NewService(&fakeEmailProvider{}, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEmailHandler_TestSend_InvalidEmail_Rejected(t *testing.T) {
	svc := emailsvc.NewService(&fakeEmailProvider{}, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"not-an-email"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEmailHandler_TestSend_SendError_MappedNotLeaked(t *testing.T) {
	// US-49.4: errors must be mapped to generic categories, not leaked
	// verbatim. The raw SES message ("Email address is not verified. The PNG
	// JPEG ...") must NOT appear in the response.
	rawErr := &testSendErr{msg: "Email address is not verified. (AWS account 123456789012) in region us-east-1"}
	fp := &fakeEmailProvider{err: rawErr}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	lg := &captureLogger{}
	h := NewEmailHandler(svc, newFakeRateCounter(), lg)

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"unverified@test.com"}`)
	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errStr, _ := resp["error"].(string)
	assert.Contains(t, errStr, "not verified",
		"the mapped error must guide the admin toward the fix")
	// The raw error details (AWS account ID, internal region string) must
	// NOT leak — that's the whole point of mapping.
	assert.NotContains(t, errStr, "123456789012",
		"AWS account ID from the raw SES error must not leak into the response")

	// The raw error MUST be logged server-side before mapping (PR review
	// finding: no swallowed errors). The admin's "check API server logs"
	// guidance depends on this.
	require.Len(t, lg.errors, 1, "the raw send error must be logged exactly once")
	assert.Equal(t, rawErr, lg.errors[0].err, "the original error must be logged verbatim")
	assert.Contains(t, lg.errors[0].msg, "test-send")
}

func TestEmailHandler_TestSend_SendError_UnknownMapsGeneric(t *testing.T) {
	fp := &fakeEmailProvider{err: &testSendErr{msg: "unexpected internal glitch #42"}}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errStr, _ := resp["error"].(string)
	assert.Contains(t, errStr, "check API server logs",
		"unrecognized errors must fall back to a generic message that points to logs")
	assert.NotContains(t, errStr, "internal glitch #42",
		"raw internal error detail must not leak")
}

func TestEmailHandler_TestSend_NilProvider_ReportsNoop(t *testing.T) {
	// Defensive path: a Service constructed with a nil provider (email not
	// configured). The handler must report noop rather than panic.
	svc := emailsvc.NewService(nil, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["sent"])
	assert.Equal(t, "noop", resp["provider"])
}

func TestEmailHandler_TestSend_RateLimited_AfterFiveCalls(t *testing.T) {
	// US-49.4: per-admin rate limit of 5/hour. The 6th call must 429.
	fp := &fakeEmailProvider{}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	rl := newFakeRateCounter()
	// Pre-seed so the 6th request sees count=6 > 5 → 429.
	rl.set("email:test-send:"+testUserID, 5)
	h := NewEmailHandler(svc, rl, &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusTooManyRequests, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, float64(testSendRateLimit), resp["limit"])
	assert.Contains(t, resp["error"], "rate limit")
}

func TestEmailHandler_TestSend_RateLimiterNil_StillWorks(t *testing.T) {
	// When rl is nil (tests / deployments without a rate limiter), the
	// handler must still function — the global RateLimitMiddleware still
	// applies in production.
	fp := &fakeEmailProvider{}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	h := NewEmailHandler(svc, nil, &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestEmailHandler_TestSend_RateLimiterError_FailsOpen(t *testing.T) {
	// When Increment returns an error (Redis down), the handler must fail
	// open — i.e. allow the request through rather than blocking all
	// test-sends during a Redis hiccup. The global RateLimitMiddleware
	// remains the backstop. A fail-closed regression (e.g. flipping the
	// `err == nil` guard) would turn this test red.
	fp := &fakeEmailProvider{}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	h := NewEmailHandler(svc, &erroringRateCounter{}, &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"ops@test.com"}`)
	require.Equal(t, http.StatusOK, w.Code, "rate-limiter error must fail open, not block the request")
}

// erroringRateCounter always returns an error from Increment, simulating a
// Redis outage. Used to verify the handler fails open.
type erroringRateCounter struct{}

func (erroringRateCounter) Increment(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, errors.New("redis unavailable")
}

func TestEmailHandler_TestSend_DisplayNameParsed(t *testing.T) {
	// mail.ParseAddress accepts "Alice <alice@test.com>". The handler should
	// use the parsed .Address (alice@test.com), not the raw display-name
	// form, so SES receives a clean address.
	fp := &fakeEmailProvider{}
	svc := emailsvc.NewService(fp, "https://app.test", "ses")
	h := NewEmailHandler(svc, newFakeRateCounter(), &captureLogger{})

	router := setupEmailRouter(h)
	w := doRequest(router, http.MethodPost, "/admin/email/test", `{"to":"Alice <alice@test.com>"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, fp.last)
	assert.Equal(t, "alice@test.com", fp.last.To,
		"display-name form must be normalised to the bare address")
}

func TestMapSESError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect string
	}{
		{"nil", nil, "email send failed"},
		{"identity not verified", &testSendErr{msg: "Email address is not verified."}, "not verified"},
		{"domain not verified", &testSendErr{msg: "Domain is not verified for region"}, "not verified"},
		{"credentials/IRSA", &testSendErr{msg: "InvalidIdentityToken: No OpenIDConnect provider found"}, "credentials"},
		{"region", &testSendErr{msg: "InvalidRegion: region foo invalid"}, "region"},
		{"throttle", &testSendErr{msg: "Throttling: Rate exceeded"}, "throttled"},
		{"timeout", &testSendErr{msg: "context deadline exceeded"}, "timed out"},
		{"unknown", &testSendErr{msg: "something completely different"}, "check API server logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapSESError(tt.err)
			assert.Contains(t, got, tt.expect)
		})
	}
}

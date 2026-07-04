// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// mockCloudflareVerify returns an httptest.Server that pretends to be
// Cloudflare's siteverify endpoint. The response is controlled by the
// verifyResponse struct passed in.
func mockCloudflareVerify(t *testing.T, success bool, errorCodes []string, statusCode int) *httptest.Server {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		// Verify Cloudflare's expected form fields are present.
		_ = r.ParseForm()
		require.NotEmpty(t, r.FormValue("secret"), "secret must be sent")
		require.NotEmpty(t, r.FormValue("response"), "response token must be sent")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		body, _ := json.Marshal(turnstileVerifyResponse{
			Success:    success,
			ErrorCodes: errorCodes,
			Hostname:   "safespaces.thekao.cloud",
		})
		_, _ = w.Write(body)
	}
	return httptest.NewServer(http.HandlerFunc(handler))
}

// setupTestRouter mounts the Turnstile middleware in front of a
// simple 200-OK handler, returning both the router and the URL the
// test should POST to.
func setupTestRouter(t *testing.T, cfg TurnstileConfig) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.POST("/register", Turnstile(cfg), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestTurnstile_ValidTokenPassesThrough(t *testing.T) {
	verify := mockCloudflareVerify(t, true, nil, http.StatusOK)
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "valid-user-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "expected 200 OK when Turnstile approves")
	assert.Contains(t, rec.Body.String(), `"ok":true`)
}

func TestTurnstile_MissingTokenReturns401(t *testing.T) {
	// No mock server needed — we shouldn't reach verify.
	r := setupTestRouter(t, TurnstileConfig{
		SecretKey: "test-secret",
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":"turnstile_failed"`)
	assert.Contains(t, rec.Body.String(), `"reason":"missing_token"`)
}

func TestTurnstile_CloudflareRejectsTokenReturns401(t *testing.T) {
	verify := mockCloudflareVerify(t, false, []string{"invalid-input-response"}, http.StatusOK)
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "invalid-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":"turnstile_failed"`)
	assert.Contains(t, rec.Body.String(), `"reason":"rejected"`)
	// Detail should include Cloudflare's error code for debuggability.
	assert.Contains(t, rec.Body.String(), "invalid-input-response")
}

func TestTurnstile_CloudflareUnreachableFailsClosed(t *testing.T) {
	// Point verify URL at an unreachable localhost port.
	r := setupTestRouter(t, TurnstileConfig{
		SecretKey: "test-secret",
		VerifyURL: "http://127.0.0.1:1/nope",
		HTTPClient: &http.Client{
			Timeout: 100 * time.Millisecond, // fast fail
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "some-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "must fail closed when verify is unreachable")
	assert.Contains(t, rec.Body.String(), `"reason":"verify_unavailable"`)
}

func TestTurnstile_Cloudflare5xxFailsClosed(t *testing.T) {
	verify := mockCloudflareVerify(t, false, nil, http.StatusInternalServerError)
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "some-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"reason":"verify_unavailable"`)
}

func TestTurnstile_TokenFromFormFieldAlsoWorks(t *testing.T) {
	verify := mockCloudflareVerify(t, true, nil, http.StatusOK)
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	form := "cfTurnstileResponse=form-token&other=stuff"
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "form field fallback should work")
}

func TestTurnstile_HeaderTakesPrecedenceOverForm(t *testing.T) {
	// Server records what token it saw so we can verify precedence.
	var seenToken string
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// url-encoded body: extract response=...
		for _, p := range strings.Split(string(body), "&") {
			if strings.HasPrefix(p, "response=") {
				seenToken = strings.TrimPrefix(p, "response=")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	form := "cfTurnstileResponse=from-form"
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(turnstileHeader, "from-header")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "from-header", seenToken, "header should win over form")
}

func TestTurnstile_ClientIPForwardedToCloudflare(t *testing.T) {
	// Cloudflare siteverify accepts remoteip as a form field. Verify our
	// middleware extracts it from X-Forwarded-For (leftmost).
	var seenRemoteIP string
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seenRemoteIP = r.FormValue("remoteip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer verify.Close()

	r := setupTestRouter(t, TurnstileConfig{
		SecretKey:  "test-secret",
		VerifyURL:  verify.URL,
		HTTPClient: verify.Client(),
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "token")
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 172.16.0.1, 10.0.0.1") // leftmost = real client
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "203.0.113.5", seenRemoteIP)
}

func TestTurnstile_NoSecretConfiguredFailsClosed(t *testing.T) {
	// Middleware with empty SecretKey must reject every request rather
	// than pass through — the "no secret" state is a configuration bug
	// and should surface loudly.
	r := setupTestRouter(t, TurnstileConfig{
		SecretKey: "",
	})

	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(turnstileHeader, "some-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

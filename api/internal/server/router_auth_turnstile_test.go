// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- Turnstile integration tests ---
//
// The middleware unit tests (api/internal/middleware/turnstile_test.go)
// prove the middleware works in isolation. These tests prove the router
// actually WIRES the middleware onto /register when
// RouterConfig.Turnstile.Enabled=true (and that the plain handler is
// installed when Enabled=false). A regression that deletes or inverts
// the `if turnstile.Enabled { ... }` block in registerAuthRoutes would
// be caught here — no other test exercises that branch.
//
// Strategy: build a fixture identical to newAuthFixture but with:
//   1. A mock siteverify HTTP server (httptest.NewServer) whose
//      response is controllable per-test.
//   2. RouterConfig.Turnstile.{Enabled, SecretKey, VerifyURL} pointed
//      at that server.
// Then send POSTs to /api/v1/auth/register with / without a
// cf-turnstile-response header and assert:
//   - authSvc.Register is (or isn't) invoked.
//   - HTTP status code is 201 (allowed through) or 401 (blocked).

func newTurnstileFixture(t *testing.T, verifyServerURL string) (*gin.Engine, *authMockServices) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err, "logger init failed")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	db := &imocks.MockDatabaseService{}
	ca := &imocks.MockCacheService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()

	svc := &authMockServices{auth: auth, metrics: met, database: db, cache: ca}
	router := NewRouter(svc, log, nil, RouterConfig{
		Debug: false,
		Turnstile: TurnstileRouterConfig{
			Enabled:   true,
			SecretKey: "test-secret",
			VerifyURL: verifyServerURL,
		},
	})
	return router, svc
}

// mockTurnstileVerify returns an httptest.Server that emulates
// Cloudflare's siteverify endpoint. `success` controls the response;
// error codes are populated when success=false so the test can assert
// on the middleware's `reason` field passthrough.
func mockTurnstileVerify(t *testing.T, success bool, errorCodes []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"success":     success,
			"error-codes": errorCodes,
			"hostname":    "test",
		})
		_, _ = w.Write(body)
	}))
}

// TestRegister_Turnstile_ValidTokenReachesHandler proves the middleware
// passes valid tokens through to the register handler and the response
// looks like a real registration.
func TestRegister_Turnstile_ValidTokenReachesHandler(t *testing.T) {
	verify := mockTurnstileVerify(t, true, nil)
	defer verify.Close()

	router, svc := newTurnstileFixture(t, verify.URL)

	resp := &types.AuthResponse{
		Token: "jwt-token",
		User: types.User{
			ID: "user-1", Username: "testuser", Email: "test@example.com", Active: true, Role: "user",
		},
	}
	svc.auth.On("Register", mock.Anything, mock.MatchedBy(func(r types.RegisterRequest) bool {
		return r.Username == "testuser"
	})).Return(resp, nil)

	rec := doRequestWithHeaders(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "securepassword123",
	}, map[string]string{"cf-turnstile-response": "valid-user-token"})

	assert.Equal(t, http.StatusCreated, rec.Code)
	// The register handler was actually reached — Register mock was called.
	svc.auth.AssertCalled(t, "Register", mock.Anything, mock.Anything)
}

// TestRegister_Turnstile_MissingTokenBlocksHandler proves the middleware
// aborts before authSvc.Register is invoked. This is the critical test
// that catches regressions of the `if turnstile.Enabled { ... }` block:
// if the wrap is deleted, this test fails because Register would be
// called on the request.
func TestRegister_Turnstile_MissingTokenBlocksHandler(t *testing.T) {
	verify := mockTurnstileVerify(t, true, nil) // never reached
	defer verify.Close()

	router, svc := newTurnstileFixture(t, verify.URL)

	// No .On("Register", ...) stub — if the middleware fails to block,
	// the handler will call Register on a nil-mock-return and panic.
	// AssertNotCalled below is the belt-and-suspenders.

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":"turnstile_failed"`)
	assert.Contains(t, rec.Body.String(), `"reason":"missing_token"`)
	svc.auth.AssertNotCalled(t, "Register", mock.Anything, mock.Anything)
}

// TestRegister_Turnstile_InvalidTokenBlocksHandler exercises the
// remote-rejection branch: Cloudflare siteverify says success=false, so
// the middleware must abort with reason="rejected" and NOT call
// authSvc.Register.
func TestRegister_Turnstile_InvalidTokenBlocksHandler(t *testing.T) {
	verify := mockTurnstileVerify(t, false, []string{"invalid-input-response"})
	defer verify.Close()

	router, svc := newTurnstileFixture(t, verify.URL)

	rec := doRequestWithHeaders(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "securepassword123",
	}, map[string]string{"cf-turnstile-response": "invalid-user-token"})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"reason":"rejected"`)
	assert.Contains(t, rec.Body.String(), `invalid-input-response`)
	svc.auth.AssertNotCalled(t, "Register", mock.Anything, mock.Anything)
}

// TestRegister_Turnstile_VerifyServerDownFailsClosed proves the
// middleware treats an unreachable siteverify endpoint as failure (not
// a permissive fallthrough). Guards against a "verify unreachable →
// allow" regression which would be worse than useless.
func TestRegister_Turnstile_VerifyServerDownFailsClosed(t *testing.T) {
	// Point at an unreachable localhost port. The middleware's HTTP
	// client will time out or connection-refuse.
	router, svc := newTurnstileFixture(t, "http://127.0.0.1:1/nope")

	rec := doRequestWithHeaders(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "securepassword123",
	}, map[string]string{"cf-turnstile-response": "any-token"})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), `"reason":"verify_unavailable"`)
	svc.auth.AssertNotCalled(t, "Register", mock.Anything, mock.Anything)
}

// TestRegister_Turnstile_DisabledSkipsMiddleware proves the reverse
// wire: when RouterConfig.Turnstile.Enabled=false (the default), the
// middleware is NOT installed and /register works with no token.
// Guards against an "always-on install" regression.
func TestRegister_Turnstile_DisabledSkipsMiddleware(t *testing.T) {
	// Default fixture — Turnstile is zero-value, Enabled=false.
	router, svc := newAuthFixture(t)

	resp := &types.AuthResponse{
		Token: "jwt-token",
		User: types.User{
			ID: "user-1", Username: "testuser", Email: "test@example.com", Active: true, Role: "user",
		},
	}
	svc.auth.On("Register", mock.Anything, mock.Anything).Return(resp, nil)

	// No cf-turnstile-response header. With Turnstile disabled, this
	// must succeed — same as any pre-Turnstile deployment.
	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "testuser",
		Email:    "test@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	svc.auth.AssertCalled(t, "Register", mock.Anything, mock.Anything)
}

// doRequestWithHeaders mirrors doRequest (from router_auth_test.go)
// but allows adding arbitrary headers to the request — needed to
// pass cf-turnstile-response through.
func doRequestWithHeaders(t *testing.T, router *gin.Engine, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

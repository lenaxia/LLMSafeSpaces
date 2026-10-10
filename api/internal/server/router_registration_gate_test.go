// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier:AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// newRegistrationGateFixture builds a router with instance settings seeded
// from vals (may be nil = fresh install, no settings rows) and the passkey
// signup routes wired with a zero-value handler. The registration gate must
// abort BEFORE the handler runs, so a zero-value PasskeyHandler is safe in
// the disabled tests (it is never invoked).
func newRegistrationGateFixture(t *testing.T, vals map[string]any) (*gin.Engine, *authMockServices) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	data := make(map[string]json.RawMessage)
	for k, v := range vals {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		data[k] = raw
	}
	instanceSettings := settings.NewInstanceService(&settingsStore{data: data}, nil)
	instanceSettings.Start()

	apiLog, _ := apilogger.New(false, "error", "json")
	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()

	svc := &authMockServices{auth: auth, metrics: met, database: &imocks.MockDatabaseService{}, cache: &imocks.MockCacheService{}}
	router := NewRouter(svc, apiLog, nil, RouterConfig{
		Debug:            false,
		InstanceSettings: instanceSettings,
		PasskeyHandler:   &handlers.PasskeyHandler{},
	})
	return router, svc
}

// TestRegister_RegistrationDisabledBySetting_Returns403 pins the audit #1650
// fix: with auth.registrationEnabled=false in instance settings, POST
// /auth/register must 403 with a generic message and never reach the auth
// service. Pre-fix this test fails: the endpoint creates the account.
func TestRegister_RegistrationDisabledBySetting_Returns403(t *testing.T) {
	router, svc := newRegistrationGateFixture(t, map[string]any{
		settings.KeyAuthRegistrationEnabled.Name(): false,
	})

	// If the gate leaks, Register would be called and return a canned
	// AuthResponse; the assert below on mock expectations catches it.
	svc.auth.On("Register", mock.Anything, mock.Anything).
		Return(&types.AuthResponse{Token: "should-not-be-issued"}, nil).
		Maybe()

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "newuser",
		Email:    "new@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errMsg, _ := body["error"].(string)
	assert.Equal(t, "registration is disabled", errMsg)
	svc.auth.AssertNotCalled(t, "Register", mock.Anything, mock.Anything)
}

// TestRegister_RegistrationEnabledByDefault_BootstrapKept pins the
// fresh-install path: no settings row exists (registry default true), so
// registration must still work — the first-user-becomes-admin bootstrap
// (CreateUser CTE) must never be locked out by this gate.
func TestRegister_RegistrationEnabledByDefault_BootstrapKept(t *testing.T) {
	router, svc := newRegistrationGateFixture(t, nil)

	svc.auth.On("Register", mock.Anything, mock.Anything).
		Return(&types.AuthResponse{Token: "tok", TokenTTL: 86400e9}, nil)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "bootstrap",
		Email:    "first@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
	svc.auth.AssertNumberOfCalls(t, "Register", 1)
}

// TestRegister_RegistrationSettingReadError_FailsOpen pins the error-branch
// semantics: a settings read failure (here: wrong-typed value) must not lock
// the endpoint — matching /auth/config's identical fail-open fallback
// (router.go GetBool err → advertised default true).
func TestRegister_RegistrationSettingReadError_FailsOpen(t *testing.T) {
	router, svc := newRegistrationGateFixture(t, map[string]any{
		settings.KeyAuthRegistrationEnabled.Name(): "not-a-bool",
	})

	svc.auth.On("Register", mock.Anything, mock.Anything).
		Return(&types.AuthResponse{Token: "tok", TokenTTL: 86400e9}, nil)

	rec := doRequest(t, router, http.MethodPost, "/api/v1/auth/register", types.RegisterRequest{
		Username: "newuser",
		Email:    "new@example.com",
		Password: "securepassword123",
	})

	assert.Equal(t, http.StatusCreated, rec.Code)
}

// TestPasskeySignup_RegistrationDisabledBySetting_Returns403 pins the same
// gate on the passkey signup pair. Pre-fix both create accounts / begin
// ceremonies. The zero-value handler is never reached when the gate aborts.
func TestPasskeySignup_RegistrationDisabledBySetting_Returns403(t *testing.T) {
	router, _ := newRegistrationGateFixture(t, map[string]any{
		settings.KeyAuthRegistrationEnabled.Name(): false,
	})

	for _, path := range []string{
		"/api/v1/auth/passkey/register/begin",
		"/api/v1/auth/passkey/register/finish",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusForbidden, rec.Code)
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			errMsg, _ := body["error"].(string)
			assert.Equal(t, "registration is disabled", errMsg)
		})
	}
}

// TestPasskeySignup_EnabledByDefault_GatePassesThrough pins that the gate
// does not interfere on a fresh install: with no settings row, the request
// reaches the handler (which 400s on the empty body — proof the gate did
// not abort). Uses a real handler with a nil service on purpose: the
// binding error returns before any service call.
func TestPasskeySignup_EnabledByDefault_GatePassesThrough(t *testing.T) {
	router, _ := newRegistrationGateFixture(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/passkey/register/begin", nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// Zero-value handler: body bind fails → 400 "invalid request" — i.e. the
	// gate passed control to the handler. A gate bug would produce 403.
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestPasskeyLoginRecover_NotGatedByRegistrationToggle pins scope: the gate
// covers SIGNUP only. Existing users must still log in / recover while
// registration is disabled (an instance closing signup must not lock out
// its existing members). Zero-value handler: LoginBegin's BindJSON fails
// → 400 proves the request reached the handler.
func TestPasskeyLoginRecover_NotGatedByRegistrationToggle(t *testing.T) {
	router, _ := newRegistrationGateFixture(t, map[string]any{
		settings.KeyAuthRegistrationEnabled.Name(): false,
	})

	for _, path := range []string{
		"/api/v1/auth/passkey/login/begin",
		"/api/v1/auth/passkey/login/finish",
		"/api/v1/auth/passkey/recover",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assert.NotEqual(t, http.StatusForbidden, rec.Code, "login/recover must not be gated by the registration toggle")
		})
	}
}

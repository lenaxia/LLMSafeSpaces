// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil verifies the passkey
// routes are registered when PasskeyHandler is non-nil, using the real
// NewRouter() function — matching the pattern of all other router tests.
func TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{
		auth:     &imocks.MockAuthMiddlewareService{},
		metrics:  met,
		database: &imocks.MockDatabaseService{},
		cache:    &imocks.MockCacheService{},
	}
	svc.auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	svc.auth.On("GetUserID", mock.Anything).Return("").Maybe()

	// Zero-value PasskeyHandler — route registration test only checks routes
	// exist (non-404). The handler methods will 400 on empty body before
	// touching the nil service fields.
	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:          false,
		PasskeyHandler: &handlers.PasskeyHandler{},
	})

	for _, path := range []string{
		"/api/v1/auth/passkey/register/begin",
		"/api/v1/auth/passkey/register/finish",
		"/api/v1/auth/passkey/login/begin",
		"/api/v1/auth/passkey/login/finish",
		"/api/v1/auth/passkey/recover",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusNotFound, w.Code, "route %s must be registered", path)
	}
}

func TestRouter_PasskeyRoutes_NotRegisteredWhenHandlerNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{
		auth:     &imocks.MockAuthMiddlewareService{},
		metrics:  met,
		database: &imocks.MockDatabaseService{},
		cache:    &imocks.MockCacheService{},
	}
	svc.auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	svc.auth.On("GetUserID", mock.Anything).Return("").Maybe()

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:          false,
		PasskeyHandler: nil,
	})

	for _, path := range []string{
		"/api/v1/auth/passkey/register/begin",
		"/api/v1/auth/passkey/login/finish",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "route %s must NOT be registered when handler is nil", path)
	}
}

// TestRouter_AuthConfig_PasskeyEnabled verifies the /auth/config endpoint
// advertises passkeyEnabled=true when the handler is wired.
func TestRouter_AuthConfig_PasskeyEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{
		auth:     &imocks.MockAuthMiddlewareService{},
		metrics:  met,
		database: &imocks.MockDatabaseService{},
		cache:    &imocks.MockCacheService{},
	}
	svc.auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	svc.auth.On("GetUserID", mock.Anything).Return("").Maybe()

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:                false,
		PasskeyHandler:       &handlers.PasskeyHandler{},
		PasskeyDefaultSignup: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
	assert.True(t, cfg.PasskeyEnabled, "passkeyEnabled must be true when handler is wired")
	assert.True(t, cfg.PasskeyDefaultSignup, "passkeyDefaultSignup must reflect the config")
}

// TestRouter_AuthConfig_PasskeyDisabled verifies the /auth/config endpoint
// advertises passkeyEnabled=false when the handler is nil.
func TestRouter_AuthConfig_PasskeyDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{
		auth:     &imocks.MockAuthMiddlewareService{},
		metrics:  met,
		database: &imocks.MockDatabaseService{},
		cache:    &imocks.MockCacheService{},
	}
	svc.auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	svc.auth.On("GetUserID", mock.Anything).Return("").Maybe()

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:          false,
		PasskeyHandler: nil,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
	assert.False(t, cfg.PasskeyEnabled, "passkeyEnabled must be false when handler is nil")
}

// TestRouter_PasskeySettingsRoutes_Registered verifies the authenticated
// settings endpoints are registered when PasskeyHandler is wired.
func TestRouter_PasskeySettingsRoutes_Registered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	svc := &authMockServices{
		auth: &imocks.MockAuthMiddlewareService{}, metrics: met,
		database: &imocks.MockDatabaseService{}, cache: &imocks.MockCacheService{},
	}
	svc.auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Set("userID", "u1"); c.Next() })).Maybe()
	svc.auth.On("GetUserID", mock.Anything).Return("u1").Maybe()
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false, PasskeyHandler: &handlers.PasskeyHandler{}})
	// GET route
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/account/passkeys", nil)
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, req1)
	assert.NotEqual(t, http.StatusNotFound, w1.Code, "GET /account/passkeys must be registered")
	// POST route
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/account/passkeys/recovery-codes/regenerate", nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	assert.NotEqual(t, http.StatusNotFound, w2.Code, "POST /account/passkeys/recovery-codes/regenerate must be registered")

	// Enroll routes
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/account/passkeys/enroll/begin", nil)
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	assert.NotEqual(t, http.StatusNotFound, w3.Code, "enroll/begin must be registered")
}

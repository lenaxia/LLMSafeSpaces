// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

// TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil verifies the passkey
// routes are registered when PasskeyHandler is non-nil, using the real
// NewRouter() function — matching the pattern of all other router tests.
func TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	router := NewRouter(svc, testLogger(), nil, RouterConfig{
		Debug:          false,
		PasskeyHandler: &handlers.PasskeyHandler{},
	})

	for _, path := range []string{
		"/api/v1/auth/passkey/register/begin",
		"/api/v1/auth/passkey/register/finish",
		"/api/v1/auth/passkey/login/begin",
		"/api/v1/auth/passkey/login/finish",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusNotFound, w.Code, "route %s must be registered", path)
	}
}

// TestRouter_PasskeyRoutes_NotRegisteredWhenHandlerNil verifies passkey routes
// are absent (404) when PasskeyHandler is nil (feature not configured).
func TestRouter_PasskeyRoutes_NotRegisteredWhenHandlerNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

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

	router := NewRouter(svc, testLogger(), nil, RouterConfig{
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

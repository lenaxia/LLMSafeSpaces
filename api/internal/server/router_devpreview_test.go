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
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

func TestRouter_DevPreviewRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})).Maybe()
	auth.On("GetUserID", mock.Anything).Return("user-1").Maybe()

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{auth: auth, metrics: met}

	dpHandler := handlers.NewDevPreviewHandler(
		nil, nil, "llmsafespaces", nil, handlers.DevPreviewConfig{Enabled: true},
	)

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:             false,
		DevPreviewHandler: dpHandler,
	})

	// The route should be registered and reachable. Without a real workspace
	// getter the handler will nil-deref or 500, but the point is the route
	// resolves (not a gin 404 for the path itself).
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusNotFound, w.Code, "dev-preview route should be registered")
}

func TestRouter_DevPreviewRouteRequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	})).Maybe()

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{auth: auth, metrics: met}

	dpHandler := handlers.NewDevPreviewHandler(
		nil, nil, "llmsafespaces", nil, handlers.DevPreviewConfig{Enabled: true},
	)

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:             false,
		DevPreviewHandler: dpHandler,
	})

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "dev-preview must be behind auth")
}

func TestRouter_DevPreviewRouteOptional(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &authMockServices{auth: auth, metrics: met}

	// No DevPreviewHandler — the route should not be registered.
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "route should not exist when handler is nil")
}

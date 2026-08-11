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

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestRouter_DevPreviewRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Next()
	})).Maybe()
	auth.On("GetUserID", mock.Anything).Return("test-user").Maybe()

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}

	dpHandler := handlers.NewDevPreviewHandler(
		nil, nil, "llmsafespaces", nil, handlers.DevPreviewConfig{Enabled: true},
	)

	router := NewRouter(svc, log, nil, RouterConfig{
		Debug:             false,
		DevPreviewHandler: dpHandler,
	})

	// The route should be registered and reachable. The handler will
	// return 503 (kill-switch is on but no workspace getter), proving
	// the route resolved (not a gin 404 for the path itself).
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
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

// --- PUT /workspaces/:id/dev-preview toggle tests ---

// newDevPreviewToggleFixture builds a router with auth + workspace service
// mocks wired for the toggle endpoint. Returns the router and the workspace
// mock so tests can set expectations on SetDevPreview.
func newDevPreviewToggleFixture(t *testing.T) (*gin.Engine, *imocks.MockWorkspaceService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Next()
	})).Maybe()
	auth.On("GetUserID", mock.Anything).Return("test-user").Maybe()

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})
	return router, ws
}

func TestRouter_DevPreviewToggle_Success(t *testing.T) {
	router, ws := newDevPreviewToggleFixture(t)
	ws.On("SetDevPreview", mock.Anything, "test-user", "ws-1", true).Return(nil)

	body, _ := json.Marshal(map[string]bool{"enabled": true})
	req, _ := http.NewRequest("PUT", "/api/v1/workspaces/ws-1/dev-preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	ws.AssertExpectations(t)
}

func TestRouter_DevPreviewToggle_RequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")
	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
	})).Maybe()
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	svc := &authMockServices{auth: auth, metrics: met}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	body, _ := json.Marshal(map[string]bool{"enabled": true})
	req, _ := http.NewRequest("PUT", "/api/v1/workspaces/ws-1/dev-preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRouter_DevPreviewToggle_BadBody(t *testing.T) {
	router, ws := newDevPreviewToggleFixture(t)
	ws.On("SetDevPreview", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	req, _ := http.NewRequest("PUT", "/api/v1/workspaces/ws-1/dev-preview", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRouter_DevPreviewToggle_ServiceError(t *testing.T) {
	router, ws := newDevPreviewToggleFixture(t)
	ws.On("SetDevPreview", mock.Anything, "test-user", "ws-1", false).Return(assertError("k8s down"))

	body, _ := json.Marshal(map[string]bool{"enabled": false})
	req, _ := http.NewRequest("PUT", "/api/v1/workspaces/ws-1/dev-preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	ws.AssertExpectations(t)
}

type errAssert struct{ msg string }

func (e *errAssert) Error() string { return e.msg }

func assertError(msg string) error { return &errAssert{msg: msg} }

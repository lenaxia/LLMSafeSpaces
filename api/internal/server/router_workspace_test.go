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

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// mockServices is a minimal implementation of interfaces.Services for router tests.
type mockServices struct {
	auth      *imocks.MockAuthMiddlewareService
	metrics   *imocks.MockMetricsService
	workspace *imocks.MockWorkspaceService
}

func (s *mockServices) GetAuth() interfaces.AuthService         { return s.auth }
func (s *mockServices) GetDatabase() interfaces.DatabaseService { return nil }
func (s *mockServices) GetCache() interfaces.CacheService       { return nil }
func (s *mockServices) GetMetrics() interfaces.MetricsService   { return s.metrics }
func (s *mockServices) GetWorkspace() interfaces.WorkspaceService {
	return s.workspace
}
func (s *mockServices) GetRateLimiter() interfaces.RateLimiterService { return nil }
func (s *mockServices) GetMetering() interfaces.MeteringService       { return nil }

// newRouterFixture builds a Gin engine wired with mock services.
// The auth middleware rejects requests without an Authorization header (401)
// and passes all other requests through with userID = "test-user".
func newRouterFixture(t *testing.T) (*gin.Engine, *mockServices) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()

	// Design 0041: WorkspaceAccessMiddleware runs on every :id route and calls
	// ResolveWorkspace + CheckOwnership. Default to allow so the dozens of
	// existing router tests (which only stub the per-route handler method)
	// continue to drive outcomes via their handler-level mocks. Tests that
	// need to assert middleware behavior override these expectations.
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})
	return router, svc
}

// workspaceRoutes lists every registered workspace route as [method, path] pairs.
var workspaceRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/workspaces"},
	{http.MethodPost, "/api/v1/workspaces"},
	{http.MethodGet, "/api/v1/workspaces/ws-1"},
	{http.MethodDelete, "/api/v1/workspaces/ws-1"},
	{http.MethodPost, "/api/v1/workspaces/ws-1/suspend"},
	{http.MethodPost, "/api/v1/workspaces/ws-1/restart"},
	{http.MethodPost, "/api/v1/workspaces/ws-1/refresh-compute"},
	{http.MethodGet, "/api/v1/workspaces/ws-1/status"},
	{http.MethodPut, "/api/v1/workspaces/ws-1"},
}

// TestWorkspaceRoutes_Exist verifies that every workspace route returns a
// non-404 response (i.e. the route is registered), even when auth is satisfied.
func TestWorkspaceRoutes_Exist(t *testing.T) {
	for _, rt := range workspaceRoutes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			router, svc := newRouterFixture(t)

			// Provide workspace mock setup so handlers don't panic; we only
			// care that the route is registered (non-404).
			svc.workspace.On("ListWorkspaces", mock.Anything, mock.Anything, mock.Anything).
				Return(nil, assert.AnError).Maybe()
			svc.workspace.On("CreateWorkspace", mock.Anything, mock.Anything, mock.Anything).
				Return(nil, assert.AnError).Maybe()
			svc.workspace.On("GetWorkspace", mock.Anything, mock.Anything, mock.Anything).
				Return(nil, assert.AnError).Maybe()
			svc.workspace.On("DeleteWorkspace", mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("SuspendWorkspace", mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("GetWorkspaceStatus", mock.Anything, mock.Anything, mock.Anything).
				Return(nil, assert.AnError).Maybe()
			svc.workspace.On("SetCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("DeleteCredentials", mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("RenameWorkspace", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("RestartWorkspace", mock.Anything, mock.Anything, mock.Anything).
				Return(assert.AnError).Maybe()
			svc.workspace.On("RefreshWorkspaceCompute", mock.Anything, mock.Anything, mock.Anything).
				Return((*types.RefreshWorkspaceResult)(nil), assert.AnError).Maybe()

			req, _ := http.NewRequest(rt.method, rt.path, nil)
			req.Header.Set("Authorization", "Bearer testtoken")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.NotEqual(t, http.StatusNotFound, w.Code,
				"route %s %s should be registered (got %d)", rt.method, rt.path, w.Code)
		})
	}
}

// TestWorkspaceRoutes_RequireAuth verifies that every workspace route returns
// 401 when no Authorization header is provided.
func TestWorkspaceRoutes_RequireAuth(t *testing.T) {
	for _, rt := range workspaceRoutes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			router, _ := newRouterFixture(t)

			req, _ := http.NewRequest(rt.method, rt.path, nil)
			// No Authorization header — should be rejected by auth middleware.
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code,
				"route %s %s should return 401 without auth token", rt.method, rt.path)
		})
	}
}

// TestRefreshComputeRoute_Success verifies the refresh-compute endpoint is
// wired end-to-end: router → handler → service → 202 + JSON body with the
// bumped restartGeneration.
func TestRefreshComputeRoute_Success(t *testing.T) {
	router, svc := newRouterFixture(t)
	svc.workspace.On("RefreshWorkspaceCompute", mock.Anything, "test-user", "ws-1").
		Return(&types.RefreshWorkspaceResult{RestartGeneration: 7}, nil)

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/refresh-compute", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, w.Body.String(), `"restartGeneration":7`)
	svc.workspace.AssertCalled(t, "RefreshWorkspaceCompute", mock.Anything, "test-user", "ws-1")
}

// TestRefreshComputeRoute_ServiceError_Propagated verifies a service error
// (e.g. forbidden / conflict) surfaces as a non-2xx instead of 202.
func TestRefreshComputeRoute_ServiceError_Propagated(t *testing.T) {
	router, svc := newRouterFixture(t)
	svc.workspace.On("RefreshWorkspaceCompute", mock.Anything, mock.Anything, mock.Anything).
		Return((*types.RefreshWorkspaceResult)(nil), assert.AnError)

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/refresh-compute", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusAccepted, w.Code)
	assert.NotEqual(t, http.StatusNotFound, w.Code)
}

// #1505 rework wiring pin: the suspend endpoint routes to
// SuspendWorkspace — the single bounded-grace variant every suspend
// source shares since #1510 (#1507). No force variant exists; the only
// force path is refresh-compute's generation-keyed marker.
func TestSuspendRoute_UsesSuspendWorkspace(t *testing.T) {
	router, svc := newRouterFixture(t)
	svc.workspace.On("SuspendWorkspace", mock.Anything, "test-user", "ws-1").
		Return(nil).Once()

	req, _ := http.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/suspend", nil)
	req.Header.Set("Authorization", "Bearer testtoken")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	svc.workspace.AssertNumberOfCalls(t, "SuspendWorkspace", 1)
}

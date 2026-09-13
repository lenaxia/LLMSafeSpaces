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
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
)

// nullTestAdapter satisfies agent.Adapter for wiring-only tests: every
// method delegates to the nil embedded interface and would panic if
// called — these suites never drive adapter paths.
type nullTestAdapter struct{ agent.Adapter }

// adminSessionMockServices is a minimum-viable interfaces.Services that wires
// only what NewRouter's middleware stack touches (auth, metrics). Everything
// else is nil — the admin session route does not depend on workspace/cache/db
// services at the router level (the handler closes over its own deps).
type adminSessionMockServices struct {
	auth interfaces.AuthService
	met  interfaces.MetricsService
}

func (s *adminSessionMockServices) GetAuth() interfaces.AuthService { return s.auth }
func (s *adminSessionMockServices) GetDatabase() interfaces.DatabaseService {
	return nil
}
func (s *adminSessionMockServices) GetCache() interfaces.CacheService     { return nil }
func (s *adminSessionMockServices) GetMetrics() interfaces.MetricsService { return s.met }
func (s *adminSessionMockServices) GetWorkspace() interfaces.WorkspaceService {
	return nil
}
func (s *adminSessionMockServices) GetRateLimiter() interfaces.RateLimiterService {
	return nil
}
func (s *adminSessionMockServices) GetMetering() interfaces.MeteringService { return nil }

// newAdminSessionIntegrationRouter wires the REAL NewRouter with a real
// *handlers.ProxyHandler (so wsstate.Store.activeSess is live) and a real
// *handlers.AdminSessionHandler. The stub AuthMiddleware injects userRole
// so AdminGuard's branch can be exercised end-to-end through the registered
// route — proving the router → middleware → handler wiring is correct, not
// just that the handler works in isolation.
func newAdminSessionIntegrationRouter(t *testing.T, role string) (*gin.Engine, *handlers.ProxyHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "admin-int")
		c.Set("userRole", role)
		c.Next()
	})).Maybe()
	auth.On("GetUserID", mock.Anything).Return("admin-int")

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	proxy, err := handlers.NewProxyHandler(kmocks.NewMockKubernetesClient(), lmocks.NewMockLogger(), "default", nil, nullTestAdapter{})
	require.NoError(t, err)
	adminLogger := lmocks.NewMockLogger()
	adminLogger.On("Info", mock.Anything, mock.Anything).Maybe()
	adminLogger.On("Warn", mock.Anything, mock.Anything).Maybe()
	adminLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	adminLogger.On("Debug", mock.Anything, mock.Anything).Maybe()
	adminLogger.On("With", mock.Anything).Return(adminLogger).Maybe()
	adminH := handlers.NewAdminSessionHandler(proxy, nil, adminLogger)

	svc := &adminSessionMockServices{auth: auth, met: met}
	router := NewRouter(svc, log, proxy, RouterConfig{AdminSessionHandler: adminH})
	return router, proxy
}

// TestAdminSessionRoute_RegisteredAndGuarded proves the route is wired through
// the real NewRouter: AdminGuard returns 404 for non-admin callers (security
// gate enforced at the registered route, not just in a unit-test shim), and a
// stuck session is cleared for an admin caller. This is the Rule 0 e2e wiring
// test — without it the handler could pass unit tests while being unwired in
// the live request path (the exact failure mode the contract-test section of
// README-LLM.md calls out).
func TestAdminSessionRoute_RegisteredAndGuarded(t *testing.T) {
	t.Run("non-admin gets 404 through real router", func(t *testing.T) {
		router, proxy := newAdminSessionIntegrationRouter(t, "user")
		proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/workspaces/ws-1/sessions/sess-stuck/force-abort", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code, "AdminGuard must 404 non-admin")
		assert.True(t, proxy.HasActiveWorkspaceForTest("ws-1"),
			"non-admin must not mutate state through the real route")
	})

	t.Run("admin clears stuck session through real router", func(t *testing.T) {
		router, proxy := newAdminSessionIntegrationRouter(t, "admin")
		proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})
		require.True(t, proxy.HasActiveWorkspaceForTest("ws-1"))

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/workspaces/ws-1/sessions/sess-stuck/force-abort", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.False(t, proxy.HasActiveWorkspaceForTest("ws-1"),
			"real router path must clear the stuck session")
	})

	t.Run("404 for non-active session through real router", func(t *testing.T) {
		router, _ := newAdminSessionIntegrationRouter(t, "admin")

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/admin/workspaces/ws-1/sessions/sess-not-stuck/force-abort", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

// TestAdminSessionRoute_405OnWrongMethod proves the route is registered with
// the correct method (POST only). A GET to the same path must 404 (gin returns
// 404 for unregistered methods, not 405, by default).
func TestAdminSessionRoute_405OnWrongMethod(t *testing.T) {
	router, proxy := newAdminSessionIntegrationRouter(t, "admin")
	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workspaces/ws-1/sessions/sess-stuck/force-abort", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "GET must not match the POST-only route")
	assert.True(t, proxy.HasActiveWorkspaceForTest("ws-1"),
		"wrong-method request must not mutate state")
}

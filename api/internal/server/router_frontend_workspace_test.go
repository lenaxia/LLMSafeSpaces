// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- POST /api/v1/workspaces/:id/activate ---

func TestActivateWorkspace_Success(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ActivateWorkspace", mock.Anything, "test-user", "ws-1").Return(
		&types.ActivateWorkspaceResponse{Resumed: "ws-1", Suspended: "ws-old"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/activate", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp types.ActivateWorkspaceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ws-1", resp.Resumed)
	assert.Equal(t, "ws-old", resp.Suspended)
}

func TestActivateWorkspace_NotFound(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ActivateWorkspace", mock.Anything, "test-user", "ws-missing").Return(
		nil, apierrors.NewNotFoundError("workspace", "ws-missing", nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-missing/activate", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestActivateWorkspace_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/activate", nil)
	// No auth header
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestActivateWorkspace_ServiceError(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ActivateWorkspace", mock.Anything, "test-user", "ws-1").Return(
		nil, errors.New("internal error"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/activate", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestListWorkspaceSessions_EmptyList(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	assert.Empty(t, items)
}

func TestListWorkspaceSessions_NotFound(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-missing").Return(
		nil, apierrors.NewNotFoundError("workspace", "ws-missing", nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-missing/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestListWorkspaceSessions_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- PUT /api/v1/workspaces/:id/sessions/:sessionId/title ---

func TestRenameSession_Success(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("RenameSession", mock.Anything, "test-user", "ws-1", "sess-1", "New Title").Return(nil)

	body, _ := json.Marshal(map[string]string{"title": "New Title"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/title", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRenameSession_MissingTitle_Returns400(t *testing.T) {
	router, _ := newRouterFixture(t)

	body, _ := json.Marshal(map[string]string{})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/title", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRenameSession_EmptyBody_Returns400(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/title", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRenameSession_NotFound(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("RenameSession", mock.Anything, "test-user", "ws-missing", "sess-1", "Title").Return(
		apierrors.NewNotFoundError("workspace", "ws-missing", nil))

	body, _ := json.Marshal(map[string]string{"title": "Title"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-missing/sessions/sess-1/title", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRenameSession_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	body, _ := json.Marshal(map[string]string{"title": "Title"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/title", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- PUT /api/v1/workspaces/:id ---

func TestRenameWorkspace_Success(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("RenameWorkspace", mock.Anything, "test-user", "ws-1", "New Name").Return(nil)

	body, _ := json.Marshal(map[string]string{"name": "New Name"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRenameWorkspace_MissingName_Returns400(t *testing.T) {
	router, _ := newRouterFixture(t)

	body, _ := json.Marshal(map[string]string{})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRenameWorkspace_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	body, _ := json.Marshal(map[string]string{"name": "New Name"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestRenameWorkspace_NotFound(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("RenameWorkspace", mock.Anything, "test-user", "ws-missing", "Name").Return(
		apierrors.NewNotFoundError("workspace", "ws-missing", nil))

	body, _ := json.Marshal(map[string]string{"name": "Name"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-missing", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// --- POST /api/v1/workspaces/:id/sessions/new ---

func TestEnsureSession_Route_Success(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("EnsureSession", mock.Anything, "test-user", "ws-1").Return(
		&types.EnsureSessionResponse{
			WorkspaceID:    "sb-1",
			WorkspacePhase: "Active",
			SessionID:      "sess-abc",
			Resumed:        false,
		}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/new", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp types.EnsureSessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "sb-1", resp.WorkspaceID)
	assert.Equal(t, "sess-abc", resp.SessionID)
	assert.False(t, resp.Resumed)
}

func TestEnsureSession_Route_Resumed(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("EnsureSession", mock.Anything, "test-user", "ws-2").Return(
		&types.EnsureSessionResponse{
			WorkspaceID:    "sb-new",
			WorkspacePhase: "Active",
			SessionID:      "sess-xyz",
			Resumed:        true,
		}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-2/sessions/new", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp types.EnsureSessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Resumed)
}

func TestEnsureSession_Route_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/new", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestEnsureSession_Route_ServiceError(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("EnsureSession", mock.Anything, "test-user", "ws-bad").Return(
		nil, errors.New("internal_error: sandbox_timeout"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-bad/sessions/new", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- US-37.2: Active session status merge in GET /:id/sessions ---

func newRouterFixtureWithProxy(t *testing.T) (*gin.Engine, *mockServices, *handlers.ProxyHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()

	// Design 0041: WorkspaceAccessMiddleware on :id routes. Default-allow so
	// handler-level mocks drive outcomes; override per-test for middleware
	// behavior assertions.
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

	k8sMock := k8smocks.NewMockKubernetesClient()
	// GetAuthoritativeActiveSessions calls LlmsafespacesV1 — return nil
	// so it falls back to the in-memory activeSess map in tests.
	k8sMock.On("LlmsafespacesV1").Return(nil, nil).Maybe()
	proxyHandler, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, nil)
	require.NoError(t, err)

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, proxyHandler, RouterConfig{Debug: false})
	return router, svc, proxyHandler
}

func TestListWorkspaceSessions_MergesActiveStatus(t *testing.T) {
	router, svc, proxy := newRouterFixtureWithProxy(t)

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-2"})

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{
			{ID: "sess-1", Title: "Idle chat", MessageCount: 5, Status: "idle"},
			{ID: "sess-2", Title: "Active chat", MessageCount: 3, Status: "idle"},
			{ID: "sess-3", Title: "Another idle", MessageCount: 1, Status: "idle"},
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 3)
	assert.Equal(t, "idle", items[0].Status)
	assert.Equal(t, "active", items[1].Status, "sess-2 should be active")
	assert.Equal(t, "idle", items[2].Status)
}

func TestListWorkspaceSessions_AllIdleWhenNoProxyHandler(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{
			{ID: "sess-1", Title: "Chat", MessageCount: 5, Status: "idle"},
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 1)
	assert.Equal(t, "idle", items[0].Status)
}

func TestListWorkspaceSessions_EmptyWorkspace_NoCrash(t *testing.T) {
	router, svc, proxy := newRouterFixtureWithProxy(t)

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-x"})

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	assert.Empty(t, items)
}

// --- PUT /api/v1/workspaces/:id/sessions/:sessionId/seen ---

func TestMarkSessionSeen_Success(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", "sess-1").Return(nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/seen", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMarkSessionSeen_WrongUser_Returns403(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", "sess-1").Return(
		apierrors.NewForbiddenError("user does not own this workspace", nil))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/seen", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestMarkSessionSeen_Unauthorized(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/seen", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- GET /api/v1/workspaces/:id/status — context usage e2e ---

func TestGetWorkspaceStatus_ContextTotal_InJSON(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("GetWorkspaceStatus", mock.Anything, "test-user", "ws-1").Return(
		&types.WorkspaceStatusResult{
			Phase:        "Active",
			ContextUsed:  45000,
			ContextTotal: 200000,
			Sessions: []types.SessionStatusItem{
				{ID: "ses_1", Status: "idle", ContextUsed: 45000},
			},
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"contextUsed":45000`, "contextUsed must appear in JSON wire format")
	assert.Contains(t, body, `"contextTotal":200000`, "contextTotal must appear in JSON wire format")

	var result types.WorkspaceStatusResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, int64(45000), result.ContextUsed)
	assert.Equal(t, int64(200000), result.ContextTotal)
	require.Len(t, result.Sessions, 1)
	assert.Equal(t, int64(45000), result.Sessions[0].ContextUsed)
}

func TestGetWorkspaceStatus_ContextTotalZero_InJSON(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("GetWorkspaceStatus", mock.Anything, "test-user", "ws-1").Return(
		&types.WorkspaceStatusResult{
			Phase:        "Active",
			ContextUsed:  0,
			ContextTotal: 0,
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"contextUsed":0`, "zero contextUsed must NOT be dropped by omitempty")
	assert.Contains(t, body, `"contextTotal":0`, "zero contextTotal must NOT be dropped by omitempty")
}

func TestGetWorkspaceStatus_SessionsWithContextUsed_InJSON(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("GetWorkspaceStatus", mock.Anything, "test-user", "ws-1").Return(
		&types.WorkspaceStatusResult{
			Phase: "Active",
			Sessions: []types.SessionStatusItem{
				{ID: "ses_1", Status: "idle", ContextUsed: 42000},
				{ID: "ses_2", Status: "busy", ContextUsed: 0},
			},
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"contextUsed":42000`, "ses_1 contextUsed in JSON")
	assert.Contains(t, body, `"contextUsed":0`, "ses_2 contextUsed:0 in JSON (no omitempty)")

	var result types.WorkspaceStatusResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Len(t, result.Sessions, 2)
	assert.Equal(t, int64(42000), result.Sessions[0].ContextUsed)
	assert.Equal(t, int64(0), result.Sessions[1].ContextUsed)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

func newAdminSessionHandlerForTest(t *testing.T, proxy *ProxyHandler, db *sql.DB) *AdminSessionHandler {
	t.Helper()
	return NewAdminSessionHandler(proxy, db, &testLogger{})
}

func newProxyForAdminTest(t *testing.T) *ProxyHandler {
	t.Helper()
	proxy, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil)
	require.NoError(t, err)
	proxy.userBroker = eventbroker.NewUserEventBroker()
	return proxy
}

// setupAdminSessionRouter builds a gin engine mirroring the production route:
//
//	POST /api/v1/admin/workspaces/:workspaceId/sessions/:sessionId/force-abort
//
// guarded by AuthMiddleware shim + middleware.AdminGuard. role controls the
// injected userRole ("admin" or any non-admin value) so the AdminGuard path
// can be exercised without spinning up the full auth stack.
func setupAdminSessionRouter(t *testing.T, h *AdminSessionHandler, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "admin-1")
		c.Set("userRole", role)
		c.Next()
	})
	g := r.Group("/api/v1/admin/workspaces/:workspaceId/sessions")
	g.Use(middleware.AdminGuard())
	g.POST("/:sessionId/force-abort", h.ForceAbortSession)
	return r
}

func doAdminForceAbort(r *gin.Engine, workspaceID, sessionID string) *httptest.ResponseRecorder {
	path := "/api/v1/admin/workspaces/" + url.PathEscape(workspaceID) +
		"/sessions/" + url.PathEscape(sessionID) + "/force-abort"
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdminSession_ForceAbort_StuckSession_ClearedAnd200(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})
	require.True(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"))

	w := doAdminForceAbort(r, "ws-1", "sess-stuck")

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, true, body["aborted"])
	assert.Equal(t, "sess-stuck", body["sessionId"])
	assert.Equal(t, "ws-1", body["workspaceId"])

	assert.False(t, proxy.HasActiveWorkspaceForTest("ws-1"), "stuck session must be removed from activeSess")
	assert.False(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"))
}

func TestAdminSession_ForceAbort_NotActive_Returns404(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	w := doAdminForceAbort(r, "ws-1", "sess-not-stuck")

	assert.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "sess-not-stuck", body["sessionId"])
	assert.Equal(t, "ws-1", body["workspaceId"])
}

func TestAdminSession_ForceAbort_InvalidSessionID_Returns400(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	w := doAdminForceAbort(r, "ws-1", "bad session!")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "invalid sessionId")
}

func TestAdminSession_ForceAbort_PathTraversalSessionID_Returns400(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	// "a..b" is composed entirely of valid sessionId characters, so it slips
	// past the regex guard; it is rejected ONLY by the dedicated ".."
	// path-traversal guard inside validateSessionID.
	w := doAdminForceAbort(r, "ws-1", "a..b")

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body["error"], "path traversal")
}

func TestAdminSession_ForceAbort_NonAdmin_Gets404(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "user")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	w := doAdminForceAbort(r, "ws-1", "sess-stuck")

	// AdminGuard returns 404 (not 403) to avoid revealing route existence.
	assert.Equal(t, http.StatusNotFound, w.Code)
	// Stuck session must NOT be cleared when the caller is rejected.
	assert.True(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"),
		"non-admin must not mutate state")
}

func TestAdminSession_ForceAbort_PublishesSSEAborted(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	sub, err := proxy.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer proxy.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := doAdminForceAbort(r, "ws-1", "sess-stuck")
	require.Equal(t, http.StatusOK, w.Code)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "sess-stuck", evt.SessionID)
		assert.Equal(t, "aborted", evt.Status)
		// WorkspaceID is intentionally NOT set on workspace-scoped publishes,
		// matching the established onSessionIdle / DeleteSession convention:
		// the routing key (PublishToWorkspace's first arg) is the workspace
		// identity; the field is reserved for user-scoped fan-out.
		assert.Empty(t, evt.WorkspaceID)
	default:
		t.Fatal("expected session.status=aborted SSE event, got none")
	}
}

func TestAdminSession_ForceAbort_AuditLogWritten(t *testing.T) {
	proxy := newProxyForAdminTest(t)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO audit_log`).
		WithArgs("admin-1", "sess-stuck", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h := newAdminSessionHandlerForTest(t, proxy, db)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	w := doAdminForceAbort(r, "ws-1", "sess-stuck")
	require.Equal(t, http.StatusOK, w.Code)

	require.NoError(t, mock.ExpectationsWereMet(), "audit INSERT must be executed")
}

func TestAdminSession_ForceAbort_AuditLogFailure_DoesNotFailRequest(t *testing.T) {
	proxy := newProxyForAdminTest(t)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO audit_log`).
		WillReturnError(assert.AnError)

	h := newAdminSessionHandlerForTest(t, proxy, db)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	w := doAdminForceAbort(r, "ws-1", "sess-stuck")

	// Audit failure is logged but must not fail the recovery operation —
	// the stuck session is still cleared and the caller still gets 200.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAdminSession_ForceAbort_NilDB_DoesNotPanic(t *testing.T) {
	proxy := newProxyForAdminTest(t)
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	assert.NotPanics(t, func() {
		w := doAdminForceAbort(r, "ws-1", "sess-stuck")
		assert.Equal(t, http.StatusOK, w.Code)
	})
	assert.False(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"))
}

func TestAdminSession_ForceAbort_NoBroker_DoesNotPanic(t *testing.T) {
	proxy, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil)
	require.NoError(t, err)
	// Deliberately leave userBroker nil — exercises the same nil-guard that
	// publishWorkspaceEvent already applies, proving the handler is safe in
	// configurations where the broker has not been started.
	h := newAdminSessionHandlerForTest(t, proxy, nil)
	r := setupAdminSessionRouter(t, h, "admin")

	proxy.SetActiveSessionsForTest("ws-1", []string{"sess-stuck"})

	assert.NotPanics(t, func() {
		w := doAdminForceAbort(r, "ws-1", "sess-stuck")
		assert.Equal(t, http.StatusOK, w.Code)
	})
	assert.False(t, proxy.isSessionActive(context.Background(), "ws-1", "sess-stuck"))
}

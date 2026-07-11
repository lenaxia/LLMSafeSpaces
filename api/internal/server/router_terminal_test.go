// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Terminal test infrastructure ---

type terminalMockCache struct {
	store map[string]string
}

func newTerminalMockCache() *terminalMockCache {
	return &terminalMockCache{store: make(map[string]string)}
}

func (m *terminalMockCache) Get(_ context.Context, key string) (string, error) {
	v, ok := m.store[key]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return v, nil
}

func (m *terminalMockCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	m.store[key] = value
	return nil
}

func (m *terminalMockCache) Delete(_ context.Context, key string) error {
	delete(m.store, key)
	return nil
}

type terminalMockWSGetter struct {
	workspaces map[string]*v1.Workspace
}

func (m *terminalMockWSGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ws, nil
}

func newTerminalTestRouter(t *testing.T) (*gin.Engine, *terminalMockCache) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("user-1")
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	// Design 0041 D1: WorkspaceAccessMiddleware now runs before the terminal
	// ticket handler and resolves ownership via the workspace service (Postgres
	// metadata). Mirror the existing wsGetter ownership semantics so the E2E
	// terminal tests exercise the same allow/deny decisions through the new
	// gate. Known-but-not-owned workspaces now return 403 (design mandates
	// 403-for-known / 404-for-unknown), replacing the handler's prior 404.
	ws.On("ResolveWorkspace", mock.Anything, "ws-active").
		Return(&types.WorkspaceMetadata{ID: "ws-active", UserID: "user-1"}, nil)
	ws.On("ResolveWorkspace", mock.Anything, "ws-suspended").
		Return(&types.WorkspaceMetadata{ID: "ws-suspended", UserID: "user-1"}, nil)
	ws.On("ResolveWorkspace", mock.Anything, "ws-other").
		Return(&types.WorkspaceMetadata{ID: "ws-other", UserID: "other-user"}, nil)
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(nil, apierrors.NewNotFoundError("workspace", mock.Anything, fmt.Errorf("not found"))).Maybe()
	ws.On("CheckOwnership", mock.Anything, "user-1", mock.MatchedBy(func(m *types.WorkspaceMetadata) bool {
		return m != nil && m.UserID == "user-1"
	})).Return(nil)
	ws.On("CheckOwnership", mock.Anything, "user-1", mock.MatchedBy(func(m *types.WorkspaceMetadata) bool {
		return m != nil && m.UserID != "user-1"
	})).Return(apierrors.NewForbiddenError("workspace access denied", fmt.Errorf("not owner")))

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}

	cache := newTerminalMockCache()
	wsGetter := &terminalMockWSGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-active": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-active",
					Labels: map[string]string{"user-id": "user-1"},
				},
				Status: v1.WorkspaceStatus{
					Phase:   v1.WorkspacePhaseActive,
					PodName: "ws-active-pod",
				},
			},
			"ws-suspended": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-suspended",
					Labels: map[string]string{"user-id": "user-1"},
				},
				Status: v1.WorkspaceStatus{
					Phase: "Suspended",
				},
			},
			"ws-other": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-other",
					Labels: map[string]string{"user-id": "other-user"},
				},
				Status: v1.WorkspaceStatus{
					Phase:   v1.WorkspacePhaseActive,
					PodName: "ws-other-pod",
				},
			},
		},
	}

	terminalHandler := handlers.NewTerminalHandler(cache, wsGetter, "llmsafespaces", log, nil)

	router := NewRouter(svc, log, nil, RouterConfig{
		TerminalHandler: terminalHandler,
	})

	return router, cache
}

// --- Integration tests ---

func TestTerminalTicket_E2E_Success(t *testing.T) {
	router, cache := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-active/terminal/ticket", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Ticket    string `json:"ticket"`
		ExpiresAt string `json:"expiresAt"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp.Ticket, "tkt_")
	assert.NotEmpty(t, resp.ExpiresAt)

	// Verify ticket was stored in cache
	key := "terminal:ticket:" + resp.Ticket
	stored, err := cache.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Contains(t, stored, "ws-active")
	assert.Contains(t, stored, "user-1")
}

func TestTerminalTicket_E2E_WorkspaceNotActive(t *testing.T) {
	router, _ := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-suspended/terminal/ticket", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestTerminalTicket_E2E_NotOwner(t *testing.T) {
	router, _ := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-other/terminal/ticket", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Design 0041 D1: WorkspaceAccessMiddleware returns 403 for a known
	// workspace the caller does not own, and 404 only for unknown workspaces
	// (no existence leak via 404, but no false 404 either). Pre-middleware the
	// terminal handler returned 404 to mask existence; the middleware gate is
	// the design-mandated behavior now.
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTerminalTicket_E2E_WorkspaceNotFound(t *testing.T) {
	router, _ := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/nonexistent/terminal/ticket", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTerminalWebSocket_E2E_InvalidTicket(t *testing.T) {
	router, _ := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-active/terminal?ticket=invalid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestTerminalWebSocket_E2E_MissingTicket(t *testing.T) {
	router, _ := newTerminalTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-active/terminal", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestTerminalWebSocket_E2E_TicketConsumedOnce(t *testing.T) {
	router, cache := newTerminalTestRouter(t)

	// First: get a ticket
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-active/terminal/ticket", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct{ Ticket string }
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Use the ticket (will fail WebSocket upgrade but ticket should be consumed)
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-active/terminal?ticket="+resp.Ticket, nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	// The response won't be a proper WebSocket upgrade (no upgrade headers), but the ticket is consumed

	// Verify ticket is gone from cache
	key := "terminal:ticket:" + resp.Ticket
	_, err := cache.Get(context.Background(), key)
	assert.Error(t, err, "ticket should be consumed after first use")
}

func TestTerminalWebSocket_E2E_WorkspaceMismatch(t *testing.T) {
	router, cache := newTerminalTestRouter(t)

	// Store a ticket for ws-active
	ticketData := `{"userID":"user-1","workspaceID":"ws-active"}`
	cache.Set(context.Background(), "terminal:ticket:tkt_test123", ticketData, 30*time.Second)

	// Try to use it for a different workspace
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-suspended/terminal?ticket=tkt_test123", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

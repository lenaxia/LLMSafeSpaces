// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Mock cache for terminal tickets ---

type mockTerminalCache struct {
	store map[string]string
}

func newMockTerminalCache() *mockTerminalCache {
	return &mockTerminalCache{store: make(map[string]string)}
}

func (m *mockTerminalCache) Get(ctx context.Context, key string) (string, error) {
	v, ok := m.store[key]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return v, nil
}

func (m *mockTerminalCache) Set(ctx context.Context, key, value string, exp time.Duration) error {
	m.store[key] = value
	return nil
}

func (m *mockTerminalCache) SetNX(ctx context.Context, key, value string, exp time.Duration) (bool, error) {
	if _, exists := m.store[key]; exists {
		return false, nil
	}
	m.store[key] = value
	return true, nil
}

func (m *mockTerminalCache) Delete(ctx context.Context, key string) error {
	delete(m.store, key)
	return nil
}

// --- Mock workspace getter ---

type mockWorkspaceGetter struct {
	workspaces map[string]*v1.Workspace
}

func (m *mockWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ws, nil
}

// --- Tests ---

func setupTerminalRouter(h *TerminalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Simulate auth middleware setting userID
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.POST("/api/v1/workspaces/:id/terminal/ticket", h.HandleTicket)
	r.GET("/api/v1/workspaces/:id/terminal", h.HandleTerminal)
	return r
}

func TestHandleTicket_Success(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-1",
					Labels: map[string]string{"user-id": "user-1"},
				},
				Status: v1.WorkspaceStatus{
					Phase:   v1.WorkspacePhaseActive,
					PodName: "ws-1-pod",
				},
			},
		},
	}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/terminal/ticket", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp TicketResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, strings.HasPrefix(resp.Ticket, "tkt_"))
	assert.False(t, resp.ExpiresAt.IsZero())
}

func TestHandleTicket_WorkspaceNotFound(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/nonexistent/terminal/ticket", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleTicket_WorkspaceNotActive(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-1",
					Labels: map[string]string{"user-id": "user-1"},
				},
				Status: v1.WorkspaceStatus{
					Phase: "Suspended",
				},
			},
		},
	}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/terminal/ticket", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestHandleTicket_TrustsMiddlewareForOwnership asserts the handler no longer
// performs its own ownership comparison against ws.Labels["user-id"]: when the
// middleware has authorized the request, HandleTicket proceeds to issue a
// ticket regardless of the CRD label. The CRD-label check was removed in
// design 0041 Story 2 because it duplicated the middleware's gate and org
// attribution lives authoritatively in PostgreSQL (the `org-id` CRD label that
// used to back such checks was deleted in Story 3 as unread dead state).
// Router-level ownership coverage lives in TestTerminalTicket_E2E_NotOwner
// (api/internal/server).
func TestHandleTicket_TrustsMiddlewareForOwnership(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": {
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ws-1",
					Labels: map[string]string{"user-id": "other-user"},
				},
				Status: v1.WorkspaceStatus{
					Phase:   v1.WorkspacePhaseActive,
					PodName: "ws-1-pod",
				},
			},
		},
	}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/terminal/ticket", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Handler MUST issue a ticket — the middleware is the single ownership gate.
	assert.Equal(t, http.StatusOK, w.Code)
	var resp TicketResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, strings.HasPrefix(resp.Ticket, "tkt_"))
}

func TestHandleTerminal_InvalidTicket(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/terminal?ticket=invalid", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHandleTerminal_MissingTicket(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/terminal", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestHandleTerminal_TicketWorkspaceMismatch(t *testing.T) {
	cache := newMockTerminalCache()
	// Store a ticket for ws-2 but request terminal for ws-1
	ticketData := `{"userID":"user-1","workspaceID":"ws-2"}`
	cache.Set(context.Background(), "terminal:ticket:tkt_abc123", ticketData, 30*time.Second)

	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}

	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)
	r := setupTerminalRouter(h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/terminal?ticket=tkt_abc123", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestConnectionLimits_PerWorkspace(t *testing.T) {
	h := &TerminalHandler{
		wsConns:              make(map[string]int),
		maxPerWorkspaceConns: 5,
		maxGlobalConns:       500,
	}

	// Fill up to limit
	for i := 0; i < 5; i++ {
		assert.True(t, h.acquireConnection("ws-1"))
	}
	// Next should fail
	assert.False(t, h.acquireConnection("ws-1"))

	// Different workspace should still work
	assert.True(t, h.acquireConnection("ws-2"))

	// Release one from ws-1
	h.releaseConnection("ws-1")
	assert.True(t, h.acquireConnection("ws-1"))
}

func TestConnectionLimits_Global(t *testing.T) {
	h := &TerminalHandler{
		wsConns:              make(map[string]int),
		maxPerWorkspaceConns: 500,
		maxGlobalConns:       3, // low limit for testing
	}

	assert.True(t, h.acquireConnection("ws-1"))
	assert.True(t, h.acquireConnection("ws-2"))
	assert.True(t, h.acquireConnection("ws-3"))
	assert.False(t, h.acquireConnection("ws-4")) // global limit hit

	h.releaseConnection("ws-1")
	assert.True(t, h.acquireConnection("ws-4")) // now works
}

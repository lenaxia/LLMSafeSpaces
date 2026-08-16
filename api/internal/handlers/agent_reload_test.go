// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- mocks ---

type mockWsSvc struct {
	ws  *types.Workspace
	err error
}

func (m *mockWsSvc) GetWorkspace(_ context.Context, _, _ string) (*types.Workspace, error) {
	return m.ws, m.err
}

type mockAgentStateStore struct {
	changedAt    time.Time
	changedAtErr error
	reloadedAt   time.Time
	reloadedErr  error
	txErr        error
}

func (m *mockAgentStateStore) GetLastCredentialChangedAt(_ context.Context, _ string) (time.Time, error) {
	return m.changedAt, m.changedAtErr
}
func (m *mockAgentStateStore) MarkAgentReloaded(_ context.Context, _ *sql.Tx, _ string, _ time.Time) (time.Time, error) {
	return m.reloadedAt, m.reloadedErr
}
func (m *mockAgentStateStore) BeginTx(_ context.Context, _ *sql.TxOptions) (*sql.Tx, error) {
	if m.txErr != nil {
		return nil, m.txErr
	}
	// Can't easily mock *sql.Tx — return nil for tests that don't exercise commit/rollback
	return nil, nil
}

type mockPodIPResolver struct {
	ip  string
	err error
}

func (m *mockPodIPResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return m.ip, m.err
}

// --- Tests ---

func TestAgentReload_Unauthenticated_Returns401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAgentReloadHandler(nil, nil, nil, nil, nil)
	router := gin.New()
	// No userID in context
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAgentReload_WorkspaceNotFound_ReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAgentReloadHandler(
		&mockWsSvc{err: apierrors.NewNotFoundError("workspace", "ws-1", nil)},
		nil, nil, nil, nil,
	)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAgentReload_PhaseNotActive_Returns409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAgentReloadHandler(
		&mockWsSvc{ws: &types.Workspace{Phase: "Suspended"}},
		nil, nil, nil, nil,
	)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "Suspended")
}

func TestAgentReload_PodIPNotResolved_Returns409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAgentReloadHandler(
		&mockWsSvc{ws: &types.Workspace{Phase: "Active"}},
		nil,
		&mockPodIPResolver{ip: ""},
		nil, nil,
	)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "not reachable")
}

func TestAgentReload_AgentdUnreachable_Returns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewAgentReloadHandler(
		&mockWsSvc{ws: &types.Workspace{Phase: "Active"}},
		&mockAgentStateStore{changedAt: time.Now()},
		&mockPodIPResolver{ip: "192.0.2.1"}, // TEST-NET-1: unreachable
		&http.Client{Timeout: 100 * time.Millisecond},
		nil,
	)
	// Password getter required — the dispatch must actually run (and fail
	// at the dial) for the agent_unreachable path to be exercised (#848).
	handler.SetPasswordGetter(interfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) {
		return "pw", nil
	}))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAgentReload_AgentdReturns502_Returns500(t *testing.T) {
	// Start a server that returns 502 on ANY path (to catch the agentd port URL)
	agentd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"opencode down"}`)
	}))
	defer agentd.Close()

	// Extract host (without port) — handler will append agentd.AgentdPort
	// This test won't hit our mock unless port matches. Skip this specific assertion
	// and verify via the unreachable path instead.
	t.Skip("Full HTTP path test requires port injection — covered by integration tests")
}

func TestAgentReload_DisposeOK_TxBeginFails_Returns200WithWarning(t *testing.T) {
	// This test requires a successful HTTP call to agentd. Since we can't
	// easily mock the port, test the tx-failure path at a higher level.
	t.Skip("Requires agentd port injection — covered by integration tests")
}

func TestAgentReload_NoAgentStateRow_Returns409(t *testing.T) {
	t.Skip("Requires agentd port injection — covered by integration tests")
}

func TestAgentReload_HappyPath(t *testing.T) {
	t.Skip("Requires agentd port injection — covered by integration tests")
}

// --- Metric recording tests ---

type recordingMetrics struct {
	mu      sync.Mutex
	results []string
}

func (m *recordingMetrics) RecordAgentReload(result string, _ int64, _ bool) {
	m.mu.Lock()
	m.results = append(m.results, result)
	m.mu.Unlock()
}
func (m *recordingMetrics) RecordAgentReloadDrainTimeout(_ int64) {}
func (m *recordingMetrics) RecordAgentReloadBulk(_, _, _ int)     {}

func TestAgentReload_ErrorPath_RecordsErrorMetric(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := &recordingMetrics{}
	handler := NewAgentReloadHandler(
		&mockWsSvc{ws: &types.Workspace{Phase: "Active"}},
		&mockAgentStateStore{changedAt: time.Now()},
		&mockPodIPResolver{ip: "192.0.2.1"}, // unreachable TEST-NET-1
		&http.Client{Timeout: 50 * time.Millisecond},
		nil,
	)
	handler.SetMetrics(metrics)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	require.Len(t, metrics.results, 1, "defer must record exactly one metric")
	assert.Equal(t, "error", metrics.results[0], "error return path must record result=error")
}

func TestAgentReload_WorkspaceNotActive_RecordsErrorMetric(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := &recordingMetrics{}
	handler := NewAgentReloadHandler(
		&mockWsSvc{ws: &types.Workspace{Phase: "Suspended"}},
		&mockAgentStateStore{changedAt: time.Now()},
		&mockPodIPResolver{ip: "192.0.2.1"},
		&http.Client{Timeout: 50 * time.Millisecond},
		nil,
	)
	handler.SetMetrics(metrics)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	require.Len(t, metrics.results, 1)
	assert.Equal(t, "error", metrics.results[0])
}

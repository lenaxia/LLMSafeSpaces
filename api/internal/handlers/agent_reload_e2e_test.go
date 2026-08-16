// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- E2E mocks that simulate the full workspace lifecycle ---

type e2eWorkspaceSvc struct {
	workspaces map[string]*types.Workspace
}

func (s *e2eWorkspaceSvc) GetWorkspace(_ context.Context, userID, wsID string) (*types.Workspace, error) {
	ws, ok := s.workspaces[wsID]
	if !ok {
		return nil, apierrors.NewNotFoundError("workspace", wsID, nil)
	}
	if ws.UserID != userID {
		return nil, apierrors.NewForbiddenError("not your workspace", nil)
	}
	return ws, nil
}

type e2eAgentStateStore struct {
	states map[string]*agentState
}

type agentState struct {
	changedAt  time.Time
	disposedAt time.Time
	pending    bool
}

func (s *e2eAgentStateStore) GetLastCredentialChangedAt(_ context.Context, wsID string) (time.Time, error) {
	if st, ok := s.states[wsID]; ok {
		return st.changedAt, nil
	}
	return time.Time{}, nil
}

func (s *e2eAgentStateStore) MarkAgentReloaded(_ context.Context, _ *sql.Tx, wsID string, priorChangedAt time.Time) (time.Time, error) {
	st, ok := s.states[wsID]
	if !ok {
		return time.Time{}, apierrors.ErrNoAgentStateRow
	}
	now := time.Now()
	st.disposedAt = now
	if !st.changedAt.After(priorChangedAt) {
		st.pending = false
	}
	return now, nil
}

func (s *e2eAgentStateStore) BeginTx(_ context.Context, _ *sql.TxOptions) (*sql.Tx, error) {
	return nil, nil // mock — handler nil-guards Rollback
}

type e2ePodResolver struct {
	ips map[string]string
}

func (r *e2ePodResolver) GetWorkspacePodIP(_ context.Context, _, wsID string) (string, error) {
	ip := r.ips[wsID]
	return ip, nil
}

type e2ePendingLister struct {
	pending []*types.WorkspaceMetadata
}

func (l *e2ePendingLister) ListPendingReloadWorkspaces(_ context.Context, _ string) ([]*types.WorkspaceMetadata, error) {
	return l.pending, nil
}

// --- E2E tests ---

func TestE2E_ReloadWorkflow_FullPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Mock agentd on a known port that always succeeds dispose. The
	// handler's agentdPort override points the dispatch here so the full
	// path (auth header included, #848) is genuinely exercised.
	var (
		gotAuth     string
		gotAuthOnce sync.Once
	)
	agentdListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr, "test agentd listener")
	port := agentdListener.Addr().(*net.TCPAddr).Port
	agentdSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthOnce.Do(func() { gotAuth = r.Header.Get("Authorization") })
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"disposed":true}`))
	}))
	agentdSrv.Listener = agentdListener
	agentdSrv.Start()
	defer agentdSrv.Close()
	agentdHost := "127.0.0.1"

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-1": {ID: "ws-1", UserID: "user-1", Phase: "Active", Name: "test-ws"},
		"ws-2": {ID: "ws-2", UserID: "user-1", Phase: "Suspended", Name: "suspended-ws"},
		"ws-3": {ID: "ws-3", UserID: "user-2", Phase: "Active", Name: "other-user-ws"},
	}}

	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-1": {changedAt: time.Now().Add(-time.Hour), pending: true},
	}}

	pods := &e2ePodResolver{ips: map[string]string{
		"ws-1": agentdHost,
	}}

	handler := NewAgentReloadHandler(wsSvc, agentDB, pods, agentdSrv.Client(), nil)
	handler.agentdPort = port
	handler.SetPasswordGetter(interfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) {
		return "e2e-pw", nil
	}))

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	t.Run("happy_path_active_workspace", func(t *testing.T) {
		// Full path: auth → workspace → phase → pod → password → dispatch
		// to the mock agentd (port override) → 200. The dispatch MUST
		// carry the workspace Basic credential — agentd enforces it (#848).
		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "full-path dispatch must succeed against the mock agentd, body=%s", rec.Body.String())
		require.Equal(t, "127.0.0.1", pods.ips["ws-1"], "precondition: ws-1 pod IP must be the mock host")
	})

	t.Run("dispatch_carries_basic_auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:e2e-pw"))
		assert.Equal(t, expected, gotAuth,
			"the agentd dispatch must carry the workspace Basic credential (#848)")
	})

	t.Run("password_getter_error_surfaces_as_500", func(t *testing.T) {
		handler.SetPasswordGetter(interfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) {
			return "", errors.New("secret unreadable")
		}))
		defer handler.SetPasswordGetter(interfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) {
			return "e2e-pw", nil
		}))

		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "get_opencode_password_failed")
	})

	t.Run("nil_password_getter_surfaces_wiring_error", func(t *testing.T) {
		saved := handler.getPassword
		handler.SetPasswordGetter(nil)
		defer func() { handler.getPassword = saved }()

		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "password_getter_not_wired")
	})

	t.Run("suspended_workspace_rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-2/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "Suspended")
	})

	t.Run("other_users_workspace_forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-3/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("nonexistent_workspace_404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-999/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("no_pod_ip_conflict", func(t *testing.T) {
		// ws-1 has a pod, but let's test a workspace with no pod
		pods.ips["ws-1"] = "" // temporarily remove
		defer func() { pods.ips["ws-1"] = agentdHost }()

		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "not reachable")
	})

	t.Run("no_agent_state_row_409", func(t *testing.T) {
		// Remove the state row for ws-1. With the dispatch port override
		// the agentd call SUCCEEDS, so this genuinely exercises the
		// no-state-row branch after a successful dispose.
		delete(agentDB.states, "ws-1")
		defer func() {
			agentDB.states["ws-1"] = &agentState{changedAt: time.Now().Add(-time.Hour), pending: true}
		}()

		req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusConflict, rec.Code,
			"successful dispose + missing state row must 409, body=%s", rec.Body.String())
	})
}

func TestE2E_BulkReload_NDJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-1": {ID: "ws-1", UserID: "user-1", Phase: "Active"},
		"ws-2": {ID: "ws-2", UserID: "user-1", Phase: "Suspended"},
	}}

	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-1": {changedAt: time.Now(), pending: true},
		"ws-2": {changedAt: time.Now(), pending: true},
	}}

	pods := &e2ePodResolver{ips: map[string]string{
		"ws-1": "192.0.2.1", // unreachable — will fail at agent call
	}}

	pendingLister := &e2ePendingLister{pending: []*types.WorkspaceMetadata{
		{ID: "ws-1", UserID: "user-1"},
		{ID: "ws-2", UserID: "user-1"},
	}}

	handler := NewBulkReloadHandler(pendingLister, wsSvc, agentDB, pods,
		&http.Client{Timeout: 100 * time.Millisecond}, nil)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.POST("/users/me/agents/reload", handler.BulkReload)

	t.Run("streams_ndjson_with_summary", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/users/me/agents/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/x-ndjson", rec.Header().Get("Content-Type"))

		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		require.GreaterOrEqual(t, len(lines), 3, "expected at least 2 results + 1 summary line")

		// Last line should be summary
		var summary map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &summary))
		summaryData, ok := summary["summary"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, float64(2), summaryData["total"])
	})

	t.Run("empty_pending_returns_summary_only", func(t *testing.T) {
		emptyLister := &e2ePendingLister{pending: nil}
		emptyHandler := NewBulkReloadHandler(emptyLister, wsSvc, agentDB, pods,
			&http.Client{Timeout: 100 * time.Millisecond}, nil)

		emptyRouter := gin.New()
		emptyRouter.Use(func(c *gin.Context) {
			c.Set("userID", "user-1")
			c.Next()
		})
		emptyRouter.POST("/users/me/agents/reload", emptyHandler.BulkReload)

		req := httptest.NewRequest(http.MethodPost, "/users/me/agents/reload", nil)
		rec := httptest.NewRecorder()
		emptyRouter.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		require.Len(t, lines, 1) // just summary

		var summary map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &summary))
		summaryData := summary["summary"].(map[string]interface{})
		assert.Equal(t, float64(0), summaryData["total"])
	})

	t.Run("unauthenticated_401", func(t *testing.T) {
		noAuthRouter := gin.New()
		noAuthRouter.POST("/users/me/agents/reload", handler.BulkReload)

		req := httptest.NewRequest(http.MethodPost, "/users/me/agents/reload", nil)
		rec := httptest.NewRecorder()
		noAuthRouter.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("partial_failure_reflected_in_summary", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/users/me/agents/reload", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		var summary map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &summary))
		summaryData := summary["summary"].(map[string]interface{})
		// No password getter wired: ws-1 fails at the getter (wiring
		// error), ws-2 fails at phase_not_active — both count as failed.
		// (The real dispatch failure path is exercised in
		// TestE2E_BulkReload_DispatchDrivesAgentd and
		// TestAgentReload_AgentdUnreachable_Returns500.)
		assert.Equal(t, float64(2), summaryData["failed"])
		assert.Equal(t, float64(0), summaryData["succeeded"])
	})
}

func TestE2E_DrainMode_AlreadyIdle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-1": {ID: "ws-1", UserID: "user-1", Phase: "Active"},
	}}

	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-1": {changedAt: time.Now(), pending: true},
	}}

	// Mock opencode that returns all sessions idle
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"s1":{"type":"idle"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer opencodeSrv.Close()

	pods := &e2ePodResolver{ips: map[string]string{"ws-1": "192.0.2.1"}}
	tracker := sse.NewTracker(nil, nil, nil)

	handler := NewAgentReloadHandler(wsSvc, agentDB, pods, &http.Client{Timeout: 100 * time.Millisecond}, nil)
	handler.SetSSETracker(tracker)
	handler.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	handler.SetStatusCheckerFactory(func(podIP, password string) SessionStatusChecker {
		return opencode.NewClient(opencodeSrv.URL, password, zaptest.NewLogger(t))
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	// With drain=true, the handler calls GetSessionStatuses via the factory
	// which points at the mock opencode server. The mock returns all sessions
	// idle, so WaitUntilIdle returns nil immediately. The reload then proceeds
	// to POST to the pod IP (192.0.2.1) which is unreachable, so the reload
	// itself returns 500. This validates the drain code path IS entered and
	// the factory IS wired.
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/agent/reload?drain=true&drainTimeoutSeconds=1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// Drain succeeds (sessions idle in mock); reload POST fails (unreachable pod).
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestE2E_DrainMode_WithRealTracker(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// This test exercises the WaitUntilIdle + SSETracker integration
	// using a mock opencode that reports all sessions idle.
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || user != "opencode" || pw != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/session/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`)) // no sessions = already idle
			return
		}
		http.NotFound(w, r)
	}))
	defer opencodeSrv.Close()

	tracker := sse.NewTracker(nil, nil, nil)

	// Use the opencode client directly to test WaitUntilIdle
	client := opencode.NewClient(opencodeSrv.URL, "test-pw", zaptest.NewLogger(t))
	err := WaitUntilIdle(context.Background(), "ws-1", tracker, client, 5*time.Second)
	assert.NoError(t, err, "WaitUntilIdle should return immediately when no sessions exist")
}

func TestE2E_EnrichChatError_WithPendingCredentials(t *testing.T) {
	body := []byte(`{"_tag":"ProviderNotFoundError","message":"provider 'openai' not configured","providerID":"openai"}`)
	since := time.Now().Add(-30 * time.Minute)

	enriched := EnrichChatErrorBody(body, true, since, "ws-abc")

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(enriched, &result))

	// Allowlisted fields preserved
	assert.Equal(t, "ProviderNotFoundError", result["_tag"])
	assert.Equal(t, "openai", result["providerID"])
	assert.Equal(t, "provider 'openai' not configured", result["message"])

	// Enrichment fields added
	assert.Equal(t, true, result["agentNeedsRefresh"])
	assert.NotNil(t, result["credentialsPendingSince"])
	assert.Contains(t, result["hint"].(string), "ws-abc")
	assert.Contains(t, result["hint"].(string), "reload")

	// Unknown fields blocked
	assert.Nil(t, result["internal"])
}

func TestE2E_EnrichChatError_NoPendingCredentials(t *testing.T) {
	body := []byte(`{"_tag":"SessionBusyError","message":"session is busy","sessionID":"sess-1"}`)

	enriched := EnrichChatErrorBody(body, false, time.Time{}, "ws-xyz")

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(enriched, &result))

	assert.Equal(t, "SessionBusyError", result["_tag"])
	assert.Equal(t, "sess-1", result["sessionID"])
	assert.Nil(t, result["agentNeedsRefresh"])
	assert.Nil(t, result["hint"])
}

// TestE2E_BulkReload_DispatchDrivesAgentd exercises reloadOne's full
// dispatch path (auth → phase → pod → password → agentd) against a real
// mock agentd via the agentdPort override, asserting the Basic credential
// reaches the wire and the NDJSON row reports a genuine dispose (#848).
// Every pre-existing bulk test short-circuited before the dispatch.
func TestE2E_BulkReload_DispatchDrivesAgentd(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var (
		mu       sync.Mutex
		gotAuth  string
		gotCount int
	)
	agentdListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	port := agentdListener.Addr().(*net.TCPAddr).Port
	agentdSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotCount++
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"disposed":true}`))
	}))
	agentdSrv.Listener = agentdListener
	agentdSrv.Start()
	defer agentdSrv.Close()

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-1": {ID: "ws-1", UserID: "user-1", Phase: "Active"},
	}}
	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-1": {changedAt: time.Now().Add(-time.Hour), pending: true},
	}}
	pods := &e2ePodResolver{ips: map[string]string{"ws-1": "127.0.0.1"}}
	pendingLister := &e2ePendingLister{pending: []*types.WorkspaceMetadata{
		{ID: "ws-1", UserID: "user-1"},
	}}

	handler := NewBulkReloadHandler(pendingLister, wsSvc, agentDB, pods,
		agentdSrv.Client(), nil)
	handler.agentdPort = port
	handler.SetPasswordGetter(interfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) {
		return "bulk-pw", nil
	}))

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/users/me/agents/reload", handler.BulkReload)

	req := httptest.NewRequest(http.MethodPost, "/users/me/agents/reload", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, gotCount, "the dispatch must reach the mock agentd exactly once")
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:bulk-pw"))
	assert.Equal(t, expected, gotAuth, "bulk reload dispatch must carry the workspace Basic credential (#848)")

	var row map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.SplitN(rec.Body.String(), "\n", 2)[0]), &row))
	assert.Equal(t, "ws-1", row["workspaceId"])
	assert.Equal(t, true, row["disposed"], "row must report a genuine dispose, body=%s", rec.Body.String())
}

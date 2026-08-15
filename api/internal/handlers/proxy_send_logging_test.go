// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestSendMessage_AdapterErrorLogged verifies that when adapter.Send fails,
// the underlying error is logged (not just returned as a bare 502 body).
//
// Regression test for #817: two production 502s ("failed to send message")
// after exactly 2m5s where the root cause was invisible because SendMessage
// didn't log the adapter error. CreateSession/ListSessions/GetHistory all
// logged; SendMessage/SendPromptAsync/DeleteSession were the outliers.
func TestSendMessage_AdapterErrorLogged(t *testing.T) {
	log, logs := logger.NewObserved()

	env := newTestEnvWithBackendAndLogger(t, func(w http.ResponseWriter, r *http.Request) {
		// Every request fails with a 502 — the adapter error path.
		http.Error(w, `{"error":"upstream exploded"}`, http.StatusBadGateway)
	}, log)

	env.setupWorkspacePodWithT(t, "ws-log", "10.0.0.1", "Active", "ws-log")
	env.setupPasswordWithT(t, "ws-log", "test-password")
	env.setupWorkspaceWithT(t, "ws-log", 5)

	// Wire the real opencode adapter pointed at the test backend, so the
	// request takes the adapter path (h.adapter != nil) — the path whose
	// error logging this test guards.
	adapter := agentoc.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		nil,
		agentoc.WithAdapterHTTPClient(env.handler.httpClient),
	)
	env.handler.SetAdapter(adapter)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-log/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code, "handler must return 502 on adapter error")

	errLogs := logs.FilterMessage("SendMessage: adapter failed")
	require.NotEmpty(t, errLogs.All(), "adapter error must be logged with the underlying error")
	entry := errLogs.All()[0]
	assert.NotNil(t, entry.ContextMap()["error"], "log entry must carry the error context")
}

// TestSendPromptAsync_AdapterErrorLogged guards the /prompt path from #817 —
// the exact endpoint that failed in production.
func TestSendPromptAsync_AdapterErrorLogged(t *testing.T) {
	log, logs := logger.NewObserved()

	env := newTestEnvWithBackendAndLogger(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream exploded"}`, http.StatusBadGateway)
	}, log)

	env.setupWorkspacePodWithT(t, "ws-log2", "10.0.0.1", "Active", "ws-log2")
	env.setupPasswordWithT(t, "ws-log2", "test-password")
	env.setupWorkspaceWithT(t, "ws-log2", 5)

	adapter := agentoc.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		nil,
		agentoc.WithAdapterHTTPClient(env.handler.httpClient),
	)
	env.handler.SetAdapter(adapter)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-log2/sessions/ses_1/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	errLogs := logs.FilterMessage("SendPromptAsync: adapter failed")
	require.NotEmpty(t, errLogs.All(),
		"/prompt adapter error must be logged — this is the exact path that failed in #817")
}

// TestDeleteSession_AdapterErrorLogged completes the parity with
// CreateSession/ListSessions/GetHistory error logging.
func TestDeleteSession_AdapterErrorLogged(t *testing.T) {
	log, logs := logger.NewObserved()

	env := newTestEnvWithBackendAndLogger(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream exploded"}`, http.StatusBadGateway)
	}, log)

	env.setupWorkspacePodWithT(t, "ws-log3", "10.0.0.1", "Active", "ws-log3")
	env.setupPasswordWithT(t, "ws-log3", "test-password")
	env.setupWorkspaceWithT(t, "ws-log3", 5)

	adapter := agentoc.NewAdapter(
		env.handler.AdapterPasswordResolver(),
		env.handler.AdapterPodIPResolver(),
		nil,
		agentoc.WithAdapterHTTPClient(env.handler.httpClient),
	)
	env.handler.SetAdapter(adapter)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/ws-log3/sessions/ses_1", nil)
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	errLogs := logs.FilterMessage("DeleteSession: adapter failed")
	require.NotEmpty(t, errLogs.All(), "DeleteSession adapter error must be logged")
}

// TestReload_MalformedPodIP_NoPanic_InvalidURL pins the agent_reload_url_invalid
// branch (PR: Go 1.26 toolchain bump): a pod IP that produces an unparseable
// URL must return a clean 500, not Do(nil) — the SIGSEGV 1.25's lenient
// net/url parsing masked.
func TestReload_MalformedPodIP_NoPanic_InvalidURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srvAddr := srv.Listener.Addr().String() // host:port — malformed once Sprintf appends :4097

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-bad": {ID: "ws-bad", UserID: "user-1", Phase: "Active", Name: "bad-ip-ws"},
	}}
	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-bad": {changedAt: time.Now().Add(-time.Hour), pending: true},
	}}
	pods := &e2ePodResolver{ips: map[string]string{"ws-bad": srvAddr}}
	handler := NewAgentReloadHandler(wsSvc, agentDB, pods, srv.Client(), nil)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/agent/reload", handler.Reload)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-bad/agent/reload", nil)
	require.NotPanics(t, func() { router.ServeHTTP(w, req) })
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "agent_reload_url_invalid",
		"malformed pod IP must hit the url_invalid branch, not panic")
}

// TestBulkReloadOne_MalformedPodIP_NDJSONErrorRow pins the bulk reloadOne
// branch — same malformed-URL class; must yield an NDJSON error row.
func TestBulkReloadOne_MalformedPodIP_NDJSONErrorRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	srvAddr := srv.Listener.Addr().String()

	wsSvc := &e2eWorkspaceSvc{workspaces: map[string]*types.Workspace{
		"ws-bad2": {ID: "ws-bad2", UserID: "user-1", Phase: "Active", Name: "bad-ip-ws"},
	}}
	agentDB := &e2eAgentStateStore{states: map[string]*agentState{
		"ws-bad2": {changedAt: time.Now().Add(-time.Hour), pending: true},
	}}
	pods := &e2ePodResolver{ips: map[string]string{"ws-bad2": srvAddr}}
	handler := NewBulkReloadHandler(nil, wsSvc, agentDB, pods, srv.Client(), nil)

	var row map[string]any
	require.NotPanics(t, func() {
		row = handler.reloadOne(context.Background(), "user-1", "ws-bad2", false, 0)
	})
	require.NotNil(t, row["error"], "bulk reload must return an error row, not panic")
	errObj, ok := row["error"].(map[string]any)
	require.True(t, ok, "error row must be an object, got %T", row["error"])
	assert.Equal(t, "agent_reload_url_invalid", errObj["code"])
}

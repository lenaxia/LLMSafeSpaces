// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 2: SendPromptAsync, EnqueueMessage, GetHistory, GetSession,
// AbortSession, DeleteSession, and RenameSessionInAgent are adapter-only.
// Every guard row wires a WORKING legacy tail (V2 test server or proxy
// backend) so a regression to the deleted fallback surfaces as a
// 2xx/legacy response, not an incidental 5xx that passes the assertion.

func TestSendPromptAsync_NilAdapter_Returns503TypedError(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	require.Nil(t, h.adapter, "precondition: no adapter")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/prompt_async",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestEnqueueMessage_NilAdapter_Returns503TypedError(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	require.Nil(t, h.adapter, "precondition: no adapter")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestAbortSession_NilAdapter_Returns503TypedError(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	require.Nil(t, h.adapter, "precondition: no adapter")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestGetHistory_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/s1/message", nil)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestGetSession_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/s1", nil)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

// DeleteSession runs post-delete side effects (tombstone, index cleanup,
// SSE publish) AFTER a successful delete. The nil-adapter guard must
// fire BEFORE any of them: a wiring failure must not tombstone a session
// that was never deleted upstream.
func TestDeleteSession_NilAdapter_Returns503_AndWritesNoTombstone(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
	assert.False(t, env.handler.isSessionDeleted("ws-1", "s1"),
		"the nil-adapter guard must not tombstone the session")
}

// RenameSessionInAgent is a service-layer helper (no gin context): with no
// adapter it must return a typed error naming the wiring failure instead of
// silently PATCHing the pod over raw HTTP.
func TestRenameSessionInAgent_NilAdapter_ReturnsTypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	err := env.handler.RenameSessionInAgent(context.Background(), "ws-1", "s1", "new title")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent adapter not configured")
}

// DeleteSession with the adapter wired runs the same post-delete side
// effects the legacy path ran — the tombstone pin that the guard row
// above references.
func TestDeleteSession_AdapterPath_StillTombstones(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, env.handler.isSessionDeleted("ws-1", "s1"),
		"successful adapter delete must still tombstone")
}

// Ports of the two semantic rows from the deleted proxy_v2_test.go:
// shared validation precedes the guard (400 beats 503), and the prompt
// route takes no busy/409 guard.
func TestSendPromptAsync_NilAdapter_ValidationPrecedesGuard(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	require.Nil(t, h.adapter, "precondition: no adapter")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/prompt_async",
		strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendPromptAsync_AdapterPath_No409Guard(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/prompt_async",
		strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusConflict, w.Code, "prompt must never take a 409 busy guard")
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- EnqueueMessage -> syncSend wiring rows (review r1 N-1/N-2) ---

// The queue route's non-outbox contract (#828 batch 2): with the adapter
// wired and the outbox unset, EnqueueMessage performs the full
// synchronous send (session-limit/quota/policy pairing + contract
// response). Dev/test-only in production wiring (app.go sets the outbox
// on the production cache-service path) — this row pins the changed
// line directly.
func TestEnqueueMessage_AdapterPath_OutboxUnset_SyncSends(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, sid, text string, _ session.SendOpts) (*session.Message, error) {
			assert.Equal(t, "ses-1", sid)
			assert.Equal(t, "hello", text)
			return &session.Message{ID: "msg_sync_1", Type: session.MessageAssistant}, nil
		},
	}
	h.outbox = nil

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "msg_sync_1")
}

// Port of the deleted TestEnqueueV2_EmptyText: shared validation precedes
// the guard — an empty text is a 400 regardless of adapter wiring.
func TestEnqueueMessage_NilAdapter_ValidationPrecedesGuard(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	router, h := newV2TestHandler(t, srv)
	require.Nil(t, h.adapter, "precondition: no adapter")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":""}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "text must not be empty")
}

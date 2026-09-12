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

	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 2: SendPromptAsync, EnqueueMessage, GetHistory, GetSession,
// AbortSession, DeleteSession, and RenameSessionInAgent are adapter-only.
// Red-state derivation (empirically verified per row): the env-harness
// HTTP rows (history/get/delete) regress to a 2xx legacy response under
// fallback restoration — their backends serve 2xx; the rename row is a
// direct method call (no HTTP), and against the pre-PR code its legacy
// PATCH hits the fake backend's 200 and returns nil — an
// error-presence mismatch at require.Error. Of the V2-harness rows
// (bare gin.New(), as is the env-harness router too): the three guard
// rows and the sync-send row were captured red-first against the real
// tails — with the pre-deletion factory wiring the tails answered 2xx,
// and restoring them without the (deleted) factory yields 500; either
// way the asserted 503 fails (and bare guard deletion yields a
// nil-adapter deref panic instead of any response). The two
// validation-precedes-guard rows and the no-409 row are
// ordering/semantic pins — green against the pre-deletion code by
// design, red only under their named reintroductions (guard hoisted
// above validation; a re-added busy guard).

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
// adapter it must return its own error naming the wiring failure instead of
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

// --- Write-route activity parity pins (review r7) ---

// AbortSession's 2xx records workspace activity; the adapter-error path
// does not. Pins the r6 parity line — deleting it turns this row red.
func TestAbortSession_2xx_RecordsActivity(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Fresh tracker per phase: each assertion is exactly its phase's
	// count (the tracker exposes only a global PendingCount).
	okTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = okTracker
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/abort", nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, 1, okTracker.PendingCount(), "successful abort records activity")

	errTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = errTracker
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error {
			return assert.AnError
		},
	}
	w = env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s2/abort", nil)
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Zero(t, errTracker.PendingCount(), "failed abort records nothing")
}

// DeleteSession's 2xx records workspace activity; the guard and
// adapter-error paths do not. Pins the r6 parity line.
func TestDeleteSession_2xx_RecordsActivity_GuardAndErrorDoNot(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Fresh tracker per phase (global PendingCount only).
	guardTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = guardTracker
	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Zero(t, guardTracker.PendingCount(), "the guard records nothing")

	errTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = errTracker
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return assert.AnError
		},
	}
	w = env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s2", nil)
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.Zero(t, errTracker.PendingCount(), "a failed delete records nothing")

	okTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = okTracker
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	w = env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s3", nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, 1, okTracker.PendingCount(), "successful delete records activity")
}

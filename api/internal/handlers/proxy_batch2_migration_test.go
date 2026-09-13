// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
// way each row's asserted response fails (the guards' typed 503; the
// sync-send row's 200 + contract body), and bare guard deletion —
// guards only; the sync-send row wires a mock adapter — yields a
// nil-adapter deref panic instead of any response. The two
// validation-precedes-guard rows and the no-409 row are
// ordering/semantic pins — green against the pre-deletion code by
// design, red only under their named reintroductions (guard hoisted
// above validation; a re-added busy guard).

// TestSendPromptAsync_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestEnqueueMessage_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestAbortSession_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestGetHistory_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestGetSession_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestDeleteSession_NilAdapter_Returns503_AndWritesNoTombstone was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the no-tombstone-on-guard half is structurally impossible; the still-tombstones-on-success half survives in this row's sibling TestDeleteSession_AdapterPath_StillTombstones.

// TestRenameSessionInAgent_NilAdapter_ReturnsTypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the typed error cannot fire.

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

// TestSendPromptAsync_NilAdapter_ValidationPrecedesGuard was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): validation-first is inherent now.

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

// TestEnqueueMessage_NilAdapter_ValidationPrecedesGuard was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): validation-first is inherent now; the empty-text 400 is pinned by TestQuestionReply_AdapterPath_EmptyAnswersRejected's sibling semantics.

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
func TestDeleteSession_2xx_RecordsActivity_ErrorDoesNot(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Fresh tracker per phase (global PendingCount only). The nil-adapter
	// guard phase died with the ctor-required adapter (#828 final batch).
	errTracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = errTracker
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return assert.AnError
		},
	}
	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s2", nil)
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

// --- r1 (final batch): AbortSession's readiness contract rows ---

func TestAbortSession_WorkspaceNotActive_Returns503(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Suspended", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error {
			t.Fatal("adapter must not be called for a suspended workspace")
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/abort", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

func TestAbortSession_WorkspaceNotFound_Returns404(t *testing.T) {
	env := newTestEnv(t)
	env.wsMock.On("Get", mock.Anything, "ws-missing", metav1.GetOptions{}).
		Return(nil, fmt.Errorf("not found")).Once()
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error {
			t.Fatal("adapter must not be called for a missing workspace")
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-missing/sessions/s1/abort", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAbortSession_ConnectionCeiling_Returns429(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error {
			t.Fatal("adapter must not be called when the ceiling rejects")
			return nil
		},
	}
	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = maxConnectionsPerWorkspace
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/abort", nil)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

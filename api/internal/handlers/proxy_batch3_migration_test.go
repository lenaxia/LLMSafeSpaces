// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 3: the question/permission routes are adapter-only.
// Red-state derivation (empirically verified per row): the env-harness
// rows wire a WORKING proxy backend with the dialect set, so a
// regression to the deleted fallback surfaces as a 2xx legacy response
// (or, for the emitPending/autoApprove rows, as a backend hit the
// adapter path cannot produce). The validation rows are ordering pins —
// green against the pre-deletion code by design, red only under a guard
// hoisted above validation.

func TestListQuestions_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/question", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestQuestionReply_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"]]}`))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestQuestionReject_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestListPermissions_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/permission", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestPermissionReply_NilAdapter_Returns503TypedError(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always"}`))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "agent adapter not configured")
}

func TestQuestionReply_NilAdapter_ValidationPrecedesGuard(t *testing.T) {
	env := newInputTestEnv(t)
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/notaque/reply",
		strings.NewReader(`{}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid question request ID format")
}

func TestPermissionReply_NilAdapter_ValidationPrecedesGuard(t *testing.T) {
	env := newInputTestEnv(t)
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/per_abc/reply",
		strings.NewReader(`{}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestListQuestions_AdapterPath_ReturnsNormalizedEnvelope(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{
				{ID: "que_1", SessionID: "ses_1", Kind: session.InputQuestion, Question: "Proceed?"},
				{ID: "per_1", SessionID: "ses_1", Kind: session.InputPermission, Permission: "bash"},
			}, nil
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/question", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 1, "questions only — permissions filtered out")
	assert.Equal(t, "que_1", got[0]["id"])
	assert.Equal(t, "ses_1", got[0]["session_id"])
	assert.Equal(t, "ses_1", got[0]["root_session_id"], "no parents resolver → root == session")
	questions, ok := got[0]["questions"].([]any)
	require.True(t, ok, "envelope carries the questions block")
	require.Len(t, questions, 1)
	q := questions[0].(map[string]any)
	assert.Equal(t, "Proceed?", q["question"])
}

func TestListPermissions_AdapterPath_ReturnsNormalizedEnvelope(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{
				{ID: "que_1", SessionID: "ses_1", Kind: session.InputQuestion},
				{ID: "per_1", SessionID: "ses_1", Kind: session.InputPermission, Permission: "bash", Patterns: []string{"rm -rf*"}},
			}, nil
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/permission", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var got []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got, 1, "permissions only — questions filtered out")
	assert.Equal(t, "per_1", got[0]["id"])
	assert.Equal(t, "bash", got[0]["permission"])
}

func TestQuestionReply_AdapterPath_AnswerQuestionReceivesAnswers(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	var gotID string
	var gotAnswers [][]string
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, rid string, answers [][]string) error {
			gotID, gotAnswers = rid, answers
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"],["Yes","No"]]}`))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, "que_abc123", gotID)
	require.Len(t, gotAnswers, 2)
	assert.Equal(t, []string{"Go"}, gotAnswers[0])
	assert.Equal(t, []string{"Yes", "No"}, gotAnswers[1])
}

func TestQuestionReply_AdapterPath_EmptyAnswersRejected(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
			t.Fatal("adapter must not be called for an empty answer body")
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "answers")
}

func TestQuestionReject_AdapterPath_RejectInputCalled(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	var gotID string
	env.handler.adapter = &mockAdapter{
		rejectInputFn: func(_ context.Context, _, _, rid string) error {
			gotID = rid
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "que_abc123", gotID)
}

func TestPermissionReply_AdapterPath_ReplyPermissionReceivesDecision(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	var gotReply, gotMessage string
	env.handler.adapter = &mockAdapter{
		replyPermissionFn: func(_ context.Context, _, _, _ string, reply, message string) error {
			gotReply, gotMessage = reply, message
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always","message":"trusted tool"}`))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, "always", gotReply)
	assert.Equal(t, "trusted tool", gotMessage, "the message field survives — the legacy passthrough carried it")
}

// emitPendingInputRequests with a nil adapter: no legacy fetch — the
// deferred snapshot-complete marker still fires with snapshot_ok=false
// (D10: clients keep their existing pending state).
func TestEmitPendingInputRequests_NilAdapter_MarkerFiresNotOK(t *testing.T) {
	env := newInputTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	userSub, err := env.handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	env.handler.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.False(t, *marker.SnapshotOK, "nil adapter → non-authoritative marker")
}

// autoApprovePermission with a nil adapter: no raw HTTP to the pod.
func TestAutoApprovePermission_NilAdapter_NoPodHTTP(t *testing.T) {
	var hits int
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	env.handler.autoApprovePermission("ws-1", "per_abc123")

	assert.Zero(t, hits, "the legacy raw-HTTP tail is gone; a nil adapter must not reach the pod")
}

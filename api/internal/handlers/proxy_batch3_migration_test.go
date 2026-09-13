// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #828 batch 3: the question/permission routes are adapter-only.
// Red-state derivation (empirically verified per row): the env-harness
// rows wire a WORKING proxy backend with the dialect set, so a
// regression to the deleted fallback surfaces as a 2xx legacy response
// (or, for the emitPending/autoApprove rows, as a backend hit the
// adapter path cannot produce). The validation rows are ordering pins —
// green against the pre-deletion code by design, red only under a guard
// hoisted above validation.

// TestListQuestions_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestQuestionReply_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestQuestionReject_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestListPermissions_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestPermissionReply_NilAdapter_Returns503TypedError was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the 503 cannot fire.

// TestQuestionReply_NilAdapter_ValidationPrecedesGuard was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): validation-first is inherent; the bad-ID 400 is pinned by the original TestProxyInput_Invalid* rows.

// TestPermissionReply_NilAdapter_ValidationPrecedesGuard was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): validation-first is inherent; the bad-ID 400 is pinned by TestProxyInput_InvalidPermissionID.

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

// TestEmitPendingInputRequests_NilAdapter_MarkerFiresNotOK was deleted with the nil-adapter guards (#828 final
// batch — the adapter is a required ctor parameter; the guard is
// structurally impossible): the D10 marker-on-failure contract is pinned by MarkerOKFalseOnBackendError (adapter error) and the k8s-failure rows.

// TestAutoApprovePermission_NilAdapter_NoPodHTTP was deleted (#828 final batch — the adapter is a
// ctor-required parameter): no raw-HTTP tail exists and no nil-adapter state exists; the adapter path is pinned by the permission suites.

// --- r1 remediation rows ---

// Transport-guard parity (review r1): the routes keep the workspace
// 404 / not-Active 503 / connection-ceiling guards the deleted transport
// enforced — re-homed via resolveWorkspaceForAdapter. The write routes
// are quota-gated + llm_request-metered (r2; the transport metered every
// 2xx on these routes); the list polls stay unmetered (the transport's
// poll metering was an over-count).
func TestListQuestions_WorkspaceNotActive_Returns503(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Suspended", "ws-1")
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			t.Fatal("adapter must not be called for a suspended workspace")
			return nil, nil
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/question", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

func TestListQuestions_WorkspaceNotFound_Returns404(t *testing.T) {
	env := newInputTestEnv(t)
	env.wsMock.On("Get", mock.Anything, "ws-missing", metav1.GetOptions{}).
		Return(nil, fmt.Errorf("not found")).Once()
	env.handler.adapter = &mockAdapter{}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-missing/question", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestQuestionReply_ConnectionCeiling_Returns429(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
			t.Fatal("adapter must not be called when the ceiling rejects")
			return nil
		},
	}
	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = maxConnectionsPerWorkspace
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"]]}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// #1302 mandate: a ListPending failure is non-authoritative — 503,
// never an authoritative empty. Both list routes.
func TestListPermissions_AdapterError_Returns503NonAuthoritative(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/permission", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "#1302: non-authoritative failure, not 502")
	assert.NotEmpty(t, w.Header().Get("Retry-After"), "the 503 carries retry semantics like the guard's")
}

func TestListQuestions_AdapterError_Returns503NonAuthoritative(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			return nil, assert.AnError
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/question", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "#1302: non-authoritative failure, not 502")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

// The remaining write-route 502 branches (r2): every route's adapter-error
// mapping is pinned.
func TestQuestionReject_AdapterError_Returns502(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			return assert.AnError
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestPermissionReply_AdapterError_Returns502(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		replyPermissionFn: func(_ context.Context, _, _, _ string, _, _ string) error {
			return assert.AnError
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always"}`))
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// Guard wiring on the remaining routes (r2): a dropped
// resolveWorkspaceForAdapter must be test-visible on every route.
func TestQuestionReject_WorkspaceNotActive_Returns503(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Suspended", "ws-1")
	env.handler.adapter = &mockAdapter{
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			t.Fatal("adapter must not be called for a suspended workspace")
			return nil
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListPermissions_WorkspaceNotFound_Returns404(t *testing.T) {
	env := newInputTestEnv(t)
	env.wsMock.On("Get", mock.Anything, "ws-missing", metav1.GetOptions{}).
		Return(nil, fmt.Errorf("not found")).Once()
	env.handler.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _ string, _ string) ([]session.InputRequest, error) {
			t.Fatal("adapter must not be called for a missing workspace")
			return nil, nil
		},
	}

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-missing/permission", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPermissionReply_ConnectionCeiling_Returns429(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		replyPermissionFn: func(_ context.Context, _, _, _ string, _, _ string) error {
			t.Fatal("adapter must not be called when the ceiling rejects")
			return nil
		},
	}
	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = maxConnectionsPerWorkspace
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always"}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// Quota parity (r2): the reply writes are quota-gated like SendMessage.
func TestQuestionReply_QuotaExceeded_Returns429(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
			t.Fatal("adapter must not be called when over quota")
			return nil
		},
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	env.handler.SetMeteringService(ms)

	// The quota gate keys on the authenticated owner (extractAuth); the
	// env router has no auth middleware, so set the context value the
	// middleware would.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"]]}`))
	req.Header.Set("Content-Type", "application/json")
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/question/:requestID/reply", func(c *gin.Context) {
		c.Set("userID", "user-1")
		env.handler.QuestionReply(c)
	})
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// Write-route adapter failures stay 502 (the agent's definitive
// rejection of the write).
func TestQuestionReply_AdapterError_Returns502(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error {
			return assert.AnError
		},
	}

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"]]}`))
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// --- r3: metering + quota pins for the r2 wiring ---

func TestQuestionReply_2xx_MetersLLMRequest(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		answerQuestionFn: func(_ context.Context, _, _, _ string, _ [][]string) error { return nil },
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	ms.On("Record", mock.Anything).Return()
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{"answers":[["Go"]]}`))
	require.Equal(t, http.StatusOK, w.Code)
	ms.AssertCalled(t, "Record", mock.Anything)
}

func TestQuestionReject_2xx_MetersLLMRequest(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		rejectInputFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	ms.On("Record", mock.Anything).Return()
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ms.AssertCalled(t, "Record", mock.Anything)
}

func TestPermissionReply_2xx_MetersLLMRequest(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		replyPermissionFn: func(_ context.Context, _, _, _ string, _, _ string) error { return nil },
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	ms.On("Record", mock.Anything).Return()
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always"}`))
	require.Equal(t, http.StatusOK, w.Code)
	ms.AssertCalled(t, "Record", mock.Anything)
}

// r3/r4 ordering pin: a malformed reply body must NOT burn a quota
// reservation — validation precedes the gate (SendMessage's order).
// Authenticated (doReplyAsUser) + mock configured: with the gate hoisted
// above validation, ReserveQuota fires before the 400 and this row goes
// red — the quota-burn regression is test-visible.
func TestQuestionReply_MalformedBody_DoesNotReserveQuota(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply",
		strings.NewReader(`{}`))
	require.Equal(t, http.StatusBadRequest, w.Code)
	ms.AssertNotCalled(t, "ReserveQuota", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestPermissionReply_MalformedBody_DoesNotReserveQuota(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{}`))
	require.Equal(t, http.StatusBadRequest, w.Code)
	ms.AssertNotCalled(t, "ReserveQuota", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestQuestionReject_QuotaExceeded_Returns429(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		rejectInputFn: func(_ context.Context, _, _, _ string) error {
			t.Fatal("adapter must not be called when over quota")
			return nil
		},
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, http.MethodPost, "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestPermissionReply_QuotaExceeded_Returns429(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		replyPermissionFn: func(_ context.Context, _, _, _ string, _, _ string) error {
			t.Fatal("adapter must not be called when over quota")
			return nil
		},
	}
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	env.handler.SetMeteringService(ms)

	w := doReplyAsUser(t, env, http.MethodPost, "/api/v1/workspaces/ws-1/permission/per_xyz789/reply",
		strings.NewReader(`{"reply":"always"}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// doReplyAsUser routes a request with the authenticated-owner context the
// quota gate and metering key on (extractAuth; the env router has no
// auth middleware): a fresh router re-registering the input routes with
// a userID-injecting wrapper.
func doReplyAsUser(t *testing.T, env *testEnv, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	wrap := func(h gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set("userID", "user-1")
			h(c)
		}
	}
	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	proxy.POST("/question/:requestID/reply", wrap(env.handler.QuestionReply))
	proxy.POST("/question/:requestID/reject", wrap(env.handler.QuestionReject))
	proxy.POST("/permission/:requestID/reply", wrap(env.handler.PermissionReply))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

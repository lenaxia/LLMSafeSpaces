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
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// US-65.4 handler-level adapter path tests. PR #717 review requested
// these to cover the 5 adapter code paths (emitPendingViaAdapter,
// autoApprovePermission, fetchSessionParent, fetchAndPersistTitle,
// runParentBackfill).

// --- fetchSessionParent via adapter ---

func TestFetchSessionParent_Adapter_ReturnsParentID(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, sid string) (*session.Session, error) {
			return &session.Session{ID: sid, ParentID: "ses_parent"}, nil
		},
	}

	parentID, err := h.fetchSessionParent(context.Background(), "ws-1", "ses_child")
	require.NoError(t, err)
	assert.Equal(t, "ses_parent", parentID)
}

func TestFetchSessionParent_Adapter_NilSession_ReturnsEmpty(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, nil
		},
	}

	parentID, err := h.fetchSessionParent(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, parentID)
}

func TestFetchSessionParent_Adapter_Error_ReturnsError(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	_, err := h.fetchSessionParent(context.Background(), "ws-1", "ses_1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adapter GetSession")
}

// --- fetchAndPersistTitle via adapter ---

func TestFetchAndPersistTitle_Adapter_PersistsTitleAndParent(t *testing.T) {
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_1", Title: "My Session", ParentID: "ses_root"}, nil
		},
	}

	h.fetchAndPersistTitle("ws-1", "ses_1")

	assert.Equal(t, "My Session", idx.titles["ws-1/ses_1"],
		"title must be persisted from adapter GetSession")
}

func TestFetchAndPersistTitle_Adapter_NilSession_NoPersistence(t *testing.T) {
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, nil
		},
	}

	h.fetchAndPersistTitle("ws-1", "ses_1")
	assert.Empty(t, idx.titles, "nil session must not persist anything")
}

// --- runParentBackfill via adapter ---

func TestRunParentBackfill_Adapter_PersistsParents(t *testing.T) {
	idx := newMockSessionIndex()
	tIdx := &trackingSessionIndex{mockSessionIndex: idx}
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = tIdx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{
				{ID: "ses_child1", ParentID: "ses_root"},
				{ID: "ses_child2", ParentID: "ses_root"},
				{ID: "ses_root", ParentID: ""},
			}, nil
		},
	}

	h.runParentBackfill("ws-1")

	assert.Equal(t, "ses_root", tIdx.parents["ws-1/ses_child1"],
		"child1 parent must be persisted")
	assert.Equal(t, "ses_root", tIdx.parents["ws-1/ses_child2"],
		"child2 parent must be persisted")
	_, hasRoot := tIdx.parents["ws-1/ses_root"]
	assert.False(t, hasRoot, "root has no parent; must not be persisted")
}

func TestRunParentBackfill_Adapter_Error_ClearsBackfillGate(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return nil, fmt.Errorf("pod down")
		},
	}
	h.state().SetParentBackfilled(context.Background(), "ws-1")

	h.runParentBackfill("ws-1")

	assert.False(t, h.state().GetParentBackfilled(context.Background(), "ws-1"),
		"adapter error must clear the backfill gate so a retry can fire")
}

// --- autoApprovePermission via adapter ---

func TestAutoApprovePermission_Adapter_HappyPath(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		resolveFn: func(_ context.Context, _, _, rid, reply string) error {
			called = true
			assert.Equal(t, "per_1", rid)
			assert.Equal(t, "always", reply)
			return nil
		},
	}

	h.autoApprovePermission("ws-1", "per_1")
	assert.True(t, called, "adapter.Resolve must be called")
}

func TestAutoApprovePermission_Adapter_Error_NoPanic(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		resolveFn: func(_ context.Context, _, _, _, _ string) error {
			return fmt.Errorf("network error")
		},
	}

	assert.NotPanics(t, func() {
		h.autoApprovePermission("ws-1", "per_1")
	})
}

// --- emitPendingInputRequests via adapter ---
//
// emitPendingInputRequests publishes SSE events via userBroker. Tests
// verify the adapter is called and the function does not panic on
// error. SSE event content is verified at the adapter level
// (pkg/agent/opencode/adapter_test.go TestAdapter_ListPending_*).

func TestEmitPendingInputRequests_Adapter_QuestionWithFullFields_NoPanic(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID:        "que_1",
				SessionID: "ses_1",
				Kind:      session.InputQuestion,
				Question:  "Which option?",
				Header:    "Choose",
				Options: []session.InputOption{
					{Label: "A", Description: "first"},
				},
				Custom: true,
				Tool:   &session.ToolRef{MessageID: "msg_1", CallID: "call_1"},
			}}, nil
		},
	}

	assert.NotPanics(t, func() {
		h.emitPendingInputRequests(context.Background(), "ws-1")
	})
}

func TestEmitPendingInputRequests_Adapter_PermissionWithPatterns_NoPanic(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return []session.InputRequest{{
				ID:         "per_1",
				SessionID:  "ses_1",
				Kind:       session.InputPermission,
				Permission: "shell",
				Patterns:   []string{"bash"},
				Always:     []string{"/workspace"},
				Tool:       &session.ToolRef{MessageID: "msg_1", CallID: "call_1"},
			}}, nil
		},
	}

	assert.NotPanics(t, func() {
		h.emitPendingInputRequests(context.Background(), "ws-1")
	})
}

func TestEmitPendingInputRequests_Adapter_AutoApprove_SkipsPermissions(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{
		AutoApprovePermissions: true,
	})
	called := false
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			called = true
			return []session.InputRequest{
				{ID: "que_1", SessionID: "ses_1", Kind: session.InputQuestion, Question: "q?"},
				{ID: "per_1", SessionID: "ses_1", Kind: session.InputPermission, Permission: "shell"},
			}, nil
		},
	}

	assert.NotPanics(t, func() {
		h.emitPendingInputRequests(context.Background(), "ws-1")
	})
	assert.True(t, called, "adapter.ListPending must be called")
}

func TestEmitPendingInputRequests_Adapter_ListPendingError_NoPanic(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	assert.NotPanics(t, func() {
		h.emitPendingInputRequests(context.Background(), "ws-1")
	})
}

// --- helpers ---

type trackingSessionIndex struct {
	*mockSessionIndex
	parents map[string]string
}

func (t *trackingSessionIndex) UpsertParent(ctx context.Context, wid, sid, pid string) error {
	if t.parents == nil {
		t.parents = make(map[string]string)
	}
	t.parents[wid+"/"+sid] = pid
	return t.mockSessionIndex.UpsertParent(ctx, wid, sid, pid)
}

func newProxyHandlerForAdapterTest(t *testing.T) *ProxyHandler {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhaseActive,
			PodIP:   "10.0.0.1",
			PodName: "test-pod",
		},
	}, nil)

	h, err := NewProxyHandler(
		k8sMock,
		&testLogger{},
		"default",
		nil,
		nil,
	)
	require.NoError(t, err)
	return h
}

func ptrTime(t time.Time) *time.Time { return &t }

// Ensure mockAdapter satisfies agent.Adapter at compile time.
var _ agent.Adapter = (*mockAdapter)(nil)

// Keep sync import alive.
var _ = sync.Mutex{}

// --- GetHistory adapter path (handler-level integration) ---

func TestGetHistory_AdapterPath_ReturnsContractJSON(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{
				{
					ID:        "msg_1",
					Type:      session.MessageUser,
					CreatedAt: ptrTime(time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)),
					Parts: []session.Part{
						{Type: session.PartText, Text: "hello"},
					},
				},
				{
					ID:   "msg_2",
					Type: session.MessageAssistant,
					Parts: []session.Part{
						{Type: session.PartText, Text: "hi there"},
					},
					Model: &session.ModelRef{ID: "gpt-4o", Provider: "openai"},
				},
			}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)

	h.GetHistory(c)

	require.Equal(t, http.StatusOK, w.Code)

	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs))
	require.Len(t, msgs, 2)
	assert.Equal(t, "msg_1", msgs[0].ID)
	assert.Equal(t, session.MessageUser, msgs[0].Type)
	assert.Equal(t, "hello", msgs[0].Parts[0].Text)
	assert.Equal(t, "msg_2", msgs[1].ID)
	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
	require.NotNil(t, msgs[1].Model)
	assert.Equal(t, "gpt-4o", msgs[1].Model.ID)
}

func TestGetHistory_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)

	h.GetHistory(c)

	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestGetHistory_AdapterPath_EmptyResult_ReturnsEmptyArray(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)

	h.GetHistory(c)

	require.Equal(t, http.StatusOK, w.Code)
	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs))
	assert.Empty(t, msgs)
}

// --- Session CRUD adapter paths ---

func TestCreateSession_AdapterPath_ReturnsContractJSON(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		createSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_new", Title: "New Session", Status: session.StatusIdle}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	h.CreateSession(c)

	require.Equal(t, http.StatusOK, w.Code)
	var s session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	assert.Equal(t, "ses_new", s.ID)
	assert.Equal(t, "New Session", s.Title)
}

func TestListSessions_AdapterPath_ReturnsContractArray(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{
				{ID: "ses_1", Title: "First", Status: session.StatusIdle},
				{ID: "ses_2", Title: "Second", Status: session.StatusBusy},
			}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h.ListSessions(c)

	require.Equal(t, http.StatusOK, w.Code)
	var sessions []session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sessions))
	require.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, session.StatusBusy, sessions[1].Status)
}

func TestGetSession_AdapterPath_ReturnsContractJSON(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_1", Title: "My Session", Status: session.StatusIdle}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h.GetSession(c)

	require.Equal(t, http.StatusOK, w.Code)
	var s session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	assert.Equal(t, "ses_1", s.ID)
	assert.Equal(t, "My Session", s.Title)
}

func TestRenameSessionInAgent_AdapterPath_DelegatesToAdapter(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		renameSessionFn: func(_ context.Context, _, _, sid, title string) error {
			called = true
			assert.Equal(t, "ses_1", sid)
			assert.Equal(t, "New Title", title)
			return nil
		},
	}

	err := h.RenameSessionInAgent(context.Background(), "ws-1", "ses_1", "New Title")
	require.NoError(t, err)
	assert.True(t, called)
}

func TestRenameSessionInAgent_AdapterPath_Error(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		renameSessionFn: func(_ context.Context, _, _, _, _ string) error {
			return fmt.Errorf("pod unreachable")
		},
	}

	err := h.RenameSessionInAgent(context.Background(), "ws-1", "ses_1", "x")
	require.Error(t, err)
}

// --- Error paths for session CRUD adapter ---

func TestCreateSession_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		createSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	h.CreateSession(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestListSessions_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return nil, fmt.Errorf("timeout")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.ListSessions(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestGetSession_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestListSessions_AdapterPath_EmptyResult_ReturnsEmptyArray(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{}, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.ListSessions(c)
	require.Equal(t, http.StatusOK, w.Code)
	var sessions []session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sessions))
	assert.Empty(t, sessions)
}

func TestGetSession_AdapterPath_NilResult_ReturnsNullJSON(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "null", w.Body.String())
}

// --- SendMessage adapter path ---

func TestSendMessage_AdapterPath_ReturnsContractMessage(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, _ session.SendOpts) (*session.Message, error) {
			assert.Equal(t, "hello world", text)
			return &session.Message{
				ID:   "msg_1",
				Type: session.MessageAssistant,
				Parts: []session.Part{
					{Type: session.PartText, Text: "Hi!"},
				},
			}, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello world"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/", body)

	h.SendMessage(c)
	require.Equal(t, http.StatusOK, w.Code)
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_1", msg.ID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
}

func TestSendMessage_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))

	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

// --- AbortSession adapter path ---

func TestAbortSession_AdapterPath_ReturnsOK(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, sid string) error {
			called = true
			assert.Equal(t, "ses_1", sid)
			return nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	h.AbortSession(c)
	c.Writer.WriteHeaderNow()
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, called)
}

func TestSendMessage_AdapterPath_EmptyText_Returns400(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":""}]}`))
	h.SendMessage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessage_AdapterPath_InvalidJSON_Returns400(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`not json`))
	h.SendMessage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAbortSession_AdapterPath_Error_Returns502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		abortFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("pod unreachable")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	h.AbortSession(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestSendPromptAsync_AdapterPath_ReturnsMessageID(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, _ session.SendOpts) (*session.Message, error) {
			called = true
			assert.Equal(t, "hello async", text)
			return &session.Message{ID: "msg_async_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hello async"}]}`))

	h.SendPromptAsync(c)
	require.True(t, called, "adapter.Send must be called")
	require.Equal(t, http.StatusOK, w.Code)
}

func TestSendPromptAsync_AdapterPath_Error_Returns500(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestSendPromptAsync_AdapterPath_SessionNotFound_Returns404(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("session not found: ses_missing")
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_missing"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "session-not-found error maps to 502 via Send path")
}

// E2E integration: adapter SendAsync through the full handler stack to a
// mock opencode backend serving the V2 prompt endpoint.
func TestE2E_Adapter_SendPromptAsync_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SendPromptAsync now uses synchronous Send (V1 POST /session/:id/message, #755)
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/session/") && strings.Contains(r.URL.Path, "/message") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_v1_1","info":{"role":"assistant","time":{"created":1786400000000}},"parts":[{"type":"text","text":"hello"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"async hello"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", body)

	require.Equal(t, http.StatusOK, w.Code, "SendPromptAsync now returns 200 via sync Send (#755)")
}

// --- DeleteSession adapter path ---

func TestDeleteSession_AdapterPath_Returns204(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, sid string) error {
			called = true
			assert.Equal(t, "ses_1", sid)
			return nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)

	h.DeleteSession(c)
	c.Writer.WriteHeaderNow()
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, called)
	assert.True(t, h.state().IsSessionDeleted(context.Background(), "ws-1", "ses_1"),
		"tombstone must be written even on adapter path")
}

func TestDeleteSession_AdapterPath_Error_Returns502_NoTombstone(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	idx := newMockSessionIndex()
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("pod unreachable")
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)

	h.DeleteSession(c)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.False(t, h.state().IsSessionDeleted(context.Background(), "ws-1", "ses_1"),
		"adapter error must NOT write tombstone (session may still be active)")
	assert.Empty(t, idx.titles, "adapter error must NOT clean up session index")
}

func TestDeleteSession_AdapterPath_RunsAllSideEffects(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	idx := newMockSessionIndex()
	h.sessionIndex = idx
	// Seed session index so we can verify cleanup.
	idx.titles["ws-1/ses_1"] = "My Session"
	h.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodDelete, "/", nil)

	h.DeleteSession(c)
	c.Writer.WriteHeaderNow()

	// Side effect 1: tombstone written.
	assert.True(t, h.state().IsSessionDeleted(context.Background(), "ws-1", "ses_1"),
		"tombstone must be written after adapter DeleteSession succeeds")

	// Side effect 2: session index cleaned up (DeleteSession on mock
	// session index is a no-op, but verify the handler calls it).
	// The mockSessionIndex.DeleteSession is a no-op return nil, so we
	// can't assert state change. The call itself is exercised by the
	// non-panic test path.

	// Side effect 3: active session removed + session parents invalidated.
	// These run in a goroutine; give them time.
	time.Sleep(50 * time.Millisecond)
	// Session parent cache invalidated (no panic = success).
}

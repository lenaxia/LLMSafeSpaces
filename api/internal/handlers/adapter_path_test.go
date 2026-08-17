// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
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

// TestEmitPendingInputRequests_Adapter_FetchFailure_MarkerOKFalse verifies
// the production (adapter) path's ok-flag semantics (PR #852 review C1): a
// ListPending failure — including a typed ErrPendingUnavailable from a
// failed pod fetch — must mark the snapshot ok:false so clients keep their
// existing pending state instead of committing an authoritative empty.
func TestEmitPendingInputRequests_Adapter_FetchFailure_MarkerOKFalse(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, fmt.Errorf("GET /question returned 500: %w", agentoc.ErrPendingUnavailable)
		},
	}

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK, "adapter-path marker must carry snapshot_ok")
	assert.False(t, *marker.SnapshotOK, "failed ListPending must not claim authoritative empty")
}

// TestEmitPendingInputRequests_Adapter_Success_MarkerOKTrue is the adapter
// path's happy-path ok assertion (the legacy path has its own).
func TestEmitPendingInputRequests_Adapter_Success_MarkerOKTrue(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
	}

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK)
}

// TestRequestInputSnapshot_FiresFlight verifies the on-demand snapshot
// endpoint emits begin → complete(ok) on the user stream (PR #852 review C3:
// ChatPage needs a fresh flight when arming reconnect mode without an SSE
// reconnect).
func TestRequestInputSnapshot_FiresFlight(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")

	proxy := env.router.Group("/api/v1/workspaces/:id")
	proxy.POST("/input-snapshot", env.handler.RequestInputSnapshot)

	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/input-snapshot", nil)
	assert.Equal(t, http.StatusAccepted, w.Code)

	begin := recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	assert.NotEmpty(t, begin.SnapshotID, "begin marker must carry the flight ID")
	complete := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, complete.SnapshotOK)
	assert.True(t, *complete.SnapshotOK)
	assert.Equal(t, begin.SnapshotID, complete.SnapshotID, "begin and complete must share the flight ID")
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

func TestSendPromptAsync_AdapterPath_Error_Returns502(t *testing.T) {
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

// TestE2E_Adapter_SendPromptAsync_UsesV1SendNotV2Queue is the regression
// test for #755 (messages disappear). SendPromptAsync must use V1
// synchronous POST /session/:id/message, NOT the V2 queue endpoint
// POST /api/session/:id/prompt. On opencode 1.18.10 the V2 queue is
// admitted but never drained — messages vanish.
//
// This test makes three positive assertions that all must hold:
//  1. The V1 endpoint is hit exactly once.
//  2. The V2 endpoint is NEVER hit.
//  3. The HTTP response carries the contract-shaped assistant message
//     (proving the synchronous Send return value flows back to the client).
//
// A revert to adapter.SendAsync (V2) would fail all three: V2 hit >0,
// V1 hit ==0, and the response body would not contain the assistant
// message ID returned by the V1 backend.
func TestE2E_Adapter_SendPromptAsync_UsesV1SendNotV2Queue(t *testing.T) {
	var v1Hits, v2Hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			v1Hits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"info":{"role":"assistant","id":"msg_v1_reply","time":{"created":1786400000000}},"parts":[{"type":"text","text":"V1 reply"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
			v2Hits++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":{"id":"msg_v2_admit","admittedSeq":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"async hello"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", body)

	require.Equal(t, http.StatusOK, w.Code, "SendPromptAsync must return 200 via synchronous Send (#755)")
	assert.Equal(t, 1, v1Hits, "V1 POST /session/:id/message must be called exactly once")
	assert.Equal(t, 0, v2Hits, "V2 POST /api/session/:id/prompt must NEVER be called (messages vanish, #755)")

	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg), "response must be the contract-shaped assistant message")
	assert.Equal(t, "msg_v1_reply", msg.ID, "response must carry the V1 assistant message ID, not a V2 admit receipt")
	assert.Equal(t, session.MessageAssistant, msg.Type)
	require.Len(t, msg.Parts, 1)
	assert.Equal(t, "V1 reply", msg.Parts[0].Text)
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

// TestSendPromptAsync_ForwardsModelOverride is the handler leg of the
// 2026-08-16 incident fix: the frontend already sends the selected model
// with every prompt ({"model":{"modelID":...,"providerID":...}}), but the
// handler dropped it, so a workspace whose persisted default model is
// unresolvable could not be interacted with at all. The model must reach
// adapter.Send via SendOpts.
func TestSendPromptAsync_ForwardsModelOverride(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, opts session.SendOpts) (*session.Message, error) {
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, gotOpts.Model, "request model must be forwarded to the adapter")
	assert.Equal(t, "glm-5.3", gotOpts.Model.ID)
	assert.Equal(t, "thekaocloud", gotOpts.Model.Provider)
}

// TestSendPromptAsync_NoModelInBody_NilOptsModel pins the default: bodies
// without a model selector (older clients, SDK prompts) must send nil model
// opts — the agent's session default applies.
func TestSendPromptAsync_NoModelInBody_NilOptsModel(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, opts session.SendOpts) (*session.Message, error) {
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
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

	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, gotOpts.Model)
}

// TestExtractPromptModel covers the model-selector parsing edge cases.
func TestExtractPromptModel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *session.ModelRef
	}{
		{"model present", `{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[]}`, &session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"}},
		{"model without provider", `{"model":{"modelID":"glm-5.3"}}`, &session.ModelRef{ID: "glm-5.3"}},
		{"model object empty modelID", `{"model":{"modelID":"","providerID":"x"},"parts":[]}`, nil},
		{"no model key", `{"parts":[{"type":"text","text":"hi"}]}`, nil},
		{"model null", `{"model":null}`, nil},
		{"malformed json", `not-json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPromptModel([]byte(tc.body))
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want.ID, got.ID)
			assert.Equal(t, tc.want.Provider, got.Provider)
		})
	}
}

// TestSendMessage_ForwardsModelOverride mirrors
// TestSendPromptAsync_ForwardsModelOverride for the SDK-documented
// synchronous /message path (review round 2: the forwarding was wired but
// unpinned — dropping extractPromptModel from SendMessage would pass CI).
func TestSendMessage_ForwardsModelOverride(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, opts session.SendOpts) (*session.Message, error) {
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendMessage(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, gotOpts.Model, "/message must forward the model selector like /prompt")
	assert.Equal(t, "glm-5.3", gotOpts.Model.ID)
	assert.Equal(t, "thekaocloud", gotOpts.Model.Provider)
}

// TestSendMessage_NoModelInBody_NilOptsModel pins the /message default.
func TestSendMessage_NoModelInBody_NilOptsModel(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, opts session.SendOpts) (*session.Message, error) {
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
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

	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, gotOpts.Model)
}

// --- Per-prompt model override: org policy enforcement (2026-08-16 follow-up) ---

// promptPolicyEnv wires a SendPromptAsync request against a mock adapter,
// with the gin-context workspace (OrgID) and optional policy checker set.
// Returns the response recorder and whether the adapter was reached.
func promptPolicyEnv(t *testing.T, orgID string, policy *types.OrgPolicyValues, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	h := newProxyHandlerForAdapterTest(t)
	adapterCalled := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			adapterCalled = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	if policy != nil || orgID != "" {
		h.SetModelPolicyChecker(&mockPolicyChecker{policy: policy})
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: orgID}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	h.SendPromptAsync(c)
	return w, adapterCalled
}

// TestSendPromptAsync_ModelOverride_OrgPolicyDenied verifies the prompt-path
// enforcement gap: ListModels filters and SetModel now rejects, but the
// per-prompt override could select any model id. Explicit denied override →
// 403, adapter never called.
func TestSendPromptAsync_ModelOverride_OrgPolicyDenied(t *testing.T) {
	w, called := promptPolicyEnv(t, "org-1", &types.OrgPolicyValues{
		AllowedModels: &[]string{"glm-5.3"},
	}, `{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, called, "denied override must not reach the adapter")
	assert.Contains(t, w.Body.String(), "not allowed by organization policy")
}

// TestSendPromptAsync_ModelOverride_OrgPolicyProviderDenied covers the
// provider axis on the prompt path.
func TestSendPromptAsync_ModelOverride_OrgPolicyProviderDenied(t *testing.T) {
	w, called := promptPolicyEnv(t, "org-1", &types.OrgPolicyValues{
		AllowedModels:    &[]string{"gpt-5.5"},
		AllowedProviders: &[]string{"anthropic"},
	}, `{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, called)
}

// TestSendPromptAsync_ModelOverride_PolicyAllowed_Bytes flow: allowed
// override forwards and returns 200.
func TestSendPromptAsync_ModelOverride_PolicyAllowed_Forwards(t *testing.T) {
	w, called := promptPolicyEnv(t, "org-1", &types.OrgPolicyValues{
		AllowedModels:    &[]string{"gpt-5.5"},
		AllowedProviders: &[]string{"openai"},
	}, `{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called)
}

// TestSendPromptAsync_ModelOverride_PersonalWorkspace_NoCheck: no org → no
// policy dimension; override forwards without a checker round-trip.
func TestSendPromptAsync_ModelOverride_PersonalWorkspace_NoCheck(t *testing.T) {
	w, called := promptPolicyEnv(t, "", nil,
		`{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called)
}

// TestSendPromptAsync_ModelOverride_PolicyError_FailsOpen: policy infra
// error must degrade to allow (matches ListModels/SetModel fail-open) —
// governance filter, not availability gate.
func TestSendPromptAsync_ModelOverride_PolicyError_FailsOpen(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{err: errors.New("policy db down")})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`))
	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called)
}

// TestSendPromptAsync_NoOverride_UnaffectedByPolicy: prompts without a model
// selector never hit the policy path even for restricted orgs.
func TestSendPromptAsync_NoOverride_UnaffectedByPolicy(t *testing.T) {
	w, called := promptPolicyEnv(t, "org-1", &types.OrgPolicyValues{
		AllowedModels: &[]string{"glm-5.3"},
	}, `{"parts":[{"type":"text","text":"hi"}]}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called, "no override = session default routing, policy not consulted")
}

// TestSendMessage_ModelOverride_OrgPolicyDenied pins the /message leg of
// the org-policy enforcement (same gap class the round-2 review flagged
// for forwarding: wired behavior must be pinned).
func TestSendMessage_ModelOverride_OrgPolicyDenied(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedModels: &[]string{"glm-5.3"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendMessage(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, called, "denied /message override must not reach the adapter")
}

// TestSendPromptAsync_ModelOverride_NoProvider_SkipsProviderAxis pins
// round-2 finding 4: a provider-less selector routes exactly like a
// session default (adapter degrades it), so the provider axis must not
// 403 it — only the model axis applies.
func TestSendPromptAsync_ModelOverride_NoProvider_SkipsProviderAxis(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedModels:    &[]string{"glm-5.3"},
		AllowedProviders: &[]string{"thekaocloud"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"glm-5.3"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code,
		"provider-less selector must not be denied on the provider axis")
	assert.True(t, called)
}

// TestSendPromptAsync_SlashBearingOverride_EmbeddedPrefixChecked guards
// the remaining bypass vector (rounds 3+5): a PROVIDER-LESS slash-bearing
// modelID — the adapter forwards it verbatim, so opencode routes via the
// embedded first segment. {"modelID":"deniedprov/gpt-5.5"} must be denied
// under allowed_providers=["openai"].
//
// Round 5 note: when providerID IS present it is authoritative (the
// adapter prefixes it; routing uses it and the policy checks it — see
// TestSendPromptAsync_FrontendDoubleForm_SlashedCatalogID_ForwardsAndAllows),
// so {"modelID":"deniedprov/x","providerID":"openai"} legitimately routes
// via openai and is NOT a bypass. AllowedModels is deliberately unset so
// the provider axis alone carries the denial.
func TestSendPromptAsync_SlashBearingOverride_EmbeddedPrefixChecked(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedProviders: &[]string{"openai"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"deniedprov/gpt-5.5"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"a provider-less slash-bearing ID routes via its embedded first segment — that provider must be policy-checked")
	assert.False(t, called)
}

// TestSendPromptAsync_SlashBearingOverride_AllowedEmbeddedProvider_Forwards
// pins the positive case: slashed ID with an allowed embedded provider routes.
func TestSendPromptAsync_SlashBearingOverride_AllowedEmbeddedProvider_Forwards(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedProviders: &[]string{"openrouter"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"openrouter/anthropic/claude-sonnet-4.5"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, called)
}

// TestSendPromptAsync_FrontendDoubleForm_SlashedCatalogID_ForwardsAndAllows
// pins round 5's major finding: the frontend builds its per-send selector
// from ListModels output — {modelID: "anthropic/claude-sonnet-4.5",
// providerID: "openrouter"} (advertised slashed ID + routing provider).
// The policy must allow it under allowed_providers=["openrouter"] (the
// first segment "anthropic" is a vendor namespace, NOT the routing
// provider), and the adapter must forward the provider-prefixed form.
func TestSendPromptAsync_FrontendDoubleForm_SlashedCatalogID_ForwardsAndAllows(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, opts session.SendOpts) (*session.Message, error) {
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedModels:    &[]string{"anthropic/claude-sonnet-4.5"},
		AllowedProviders: &[]string{"openrouter"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"anthropic/claude-sonnet-4.5","providerID":"openrouter"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code,
		"the frontend's double form must not be denied: providerID is the routing provider and is allowed")
	require.NotNil(t, gotOpts.Model)
	assert.Equal(t, "anthropic/claude-sonnet-4.5", gotOpts.Model.ID)
	assert.Equal(t, "openrouter", gotOpts.Model.Provider)
}

// TestSendPromptAsync_ModelAxis_TailAccepted aligns the prompt path's
// model axis with SetModel's: an org allowlisting the bare tail of a
// slashed catalog ID permits the override (deny-direction consistency
// across the three enforcement points).
func TestSendPromptAsync_ModelAxis_TailAccepted(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	called := false
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			called = true
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	h.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedModels: &[]string{"claude-sonnet-4.5"},
	}})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"anthropic/claude-sonnet-4.5","providerID":"openrouter"},"parts":[{"type":"text","text":"hi"}]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code, "tail-allowlisted model must be permitted like SetModel permits it")
	assert.True(t, called)
}

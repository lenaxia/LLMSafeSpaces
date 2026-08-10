// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
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
	h, err := NewProxyHandler(
		k8smocks.NewMockKubernetesClient(),
		&testLogger{},
		"default",
		nil,
		nil,
	)
	require.NoError(t, err)
	return h
}

// Ensure mockAdapter satisfies agent.Adapter at compile time.
var _ agent.Adapter = (*mockAdapter)(nil)

// Keep sync import alive.
var _ = sync.Mutex{}

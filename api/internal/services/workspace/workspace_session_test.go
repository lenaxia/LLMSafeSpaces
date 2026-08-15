// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockSessionIndex implements interfaces.SessionIndexService for testing.
type mockSessionIndex struct {
	mock.Mock
}

func (m *mockSessionIndex) RecordMessage(workspaceID, sessionID, title string, at time.Time) {
	m.Called(workspaceID, sessionID, title, at)
}
func (m *mockSessionIndex) ListByWorkspace(ctx context.Context, workspaceID string) ([]types.SessionListItem, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.SessionListItem), args.Error(1)
}
func (m *mockSessionIndex) DeleteByWorkspace(ctx context.Context, workspaceID string) error {
	return m.Called(ctx, workspaceID).Error(0)
}
func (m *mockSessionIndex) DeleteSession(ctx context.Context, workspaceID, sessionID string) error {
	return m.Called(ctx, workspaceID, sessionID).Error(0)
}
func (m *mockSessionIndex) UpsertTitle(ctx context.Context, workspaceID, sessionID, title string) error {
	return m.Called(ctx, workspaceID, sessionID, title).Error(0)
}
func (m *mockSessionIndex) UpsertParent(ctx context.Context, workspaceID, sessionID, parentID string) error {
	return m.Called(ctx, workspaceID, sessionID, parentID).Error(0)
}
func (m *mockSessionIndex) UpdateLastSeen(ctx context.Context, workspaceID, sessionID string) error {
	return m.Called(ctx, workspaceID, sessionID).Error(0)
}
func (m *mockSessionIndex) UpsertContextUsed(ctx context.Context, workspaceID, sessionID string, contextUsed int64) error {
	return m.Called(ctx, workspaceID, sessionID, contextUsed).Error(0)
}
func (m *mockSessionIndex) Start() error { return nil }
func (m *mockSessionIndex) Stop() error  { return nil }

var _ apiinterfaces.SessionIndexService = (*mockSessionIndex)(nil)

func TestListWorkspaceSessions_DelegatesToSessionIndex(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	expected := []types.SessionListItem{
		{ID: "s1", Title: "Chat", MessageCount: 5, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(expected, nil)

	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}, nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, expected, result)
	si.AssertCalled(t, "ListByWorkspace", mock.Anything, "ws-1")
}

func TestListWorkspaceSessions_NilSessionIndex_ReturnsEmpty(t *testing.T) {
	f := newFixture(t)
	// Don't set session index

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	assert.NoError(t, err)
	assert.Empty(t, result)
}

func TestListWorkspaceSessions_WrongOwner_Forbidden(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "other-user",
	}, nil)

	_, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "does not have access to workspace")
}

func TestRenameSession_DelegatesToSessionIndex(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)
	si.On("UpsertTitle", mock.Anything, "ws-1", "s1", "New Name").Return(nil)

	err := f.svc.RenameSession(context.Background(), "user-1", "ws-1", "s1", "New Name")
	assert.NoError(t, err)
	si.AssertCalled(t, "UpsertTitle", mock.Anything, "ws-1", "s1", "New Name")
}

func TestRenameSession_NilSessionIndex_NoError(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	err := f.svc.RenameSession(context.Background(), "user-1", "ws-1", "s1", "Title")
	assert.NoError(t, err)
}

func TestRenameWorkspace_Success(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)
	name := "new-name"
	f.db.On("UpdateWorkspace", mock.Anything, "ws-1", types.WorkspaceUpdates{Name: &name}).Return(nil)

	err := f.svc.RenameWorkspace(context.Background(), "user-1", "ws-1", "new-name")
	assert.NoError(t, err)
}

func TestRenameWorkspace_WrongOwner_Forbidden(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "other-user",
	}, nil)

	err := f.svc.RenameWorkspace(context.Background(), "user-1", "ws-1", "new-name")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "does not have access to workspace")
}

func TestMarkSessionSeen_DelegatesToSessionIndex(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)
	si.On("UpdateLastSeen", mock.Anything, "ws-1", "s1").Return(nil)

	err := f.svc.MarkSessionSeen(context.Background(), "user-1", "ws-1", "s1")
	assert.NoError(t, err)
	si.AssertCalled(t, "UpdateLastSeen", mock.Anything, "ws-1", "s1")
}

func TestMarkSessionSeen_WrongOwner_Forbidden(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "other-user",
	}, nil)

	err := f.svc.MarkSessionSeen(context.Background(), "user-1", "ws-1", "s1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "does not have access to workspace")
}

func TestMarkSessionSeen_NilSessionIndex_NoError(t *testing.T) {
	f := newFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	err := f.svc.MarkSessionSeen(context.Background(), "user-1", "ws-1", "s1")
	assert.NoError(t, err)
}

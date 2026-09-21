// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mocks

import (
	"context"

	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// MockWorkspaceService implements interfaces.WorkspaceService for testing.
type MockWorkspaceService struct {
	mock.Mock
}

var _ interfaces.WorkspaceService = (*MockWorkspaceService)(nil)

func (m *MockWorkspaceService) CreateWorkspace(ctx context.Context, userID string, req types.CreateWorkspaceRequest) (*types.Workspace, error) {
	args := m.Called(ctx, userID, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.Workspace), args.Error(1)
}

func (m *MockWorkspaceService) GetWorkspace(ctx context.Context, userID, workspaceID string) (*types.Workspace, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.Workspace), args.Error(1)
}

func (m *MockWorkspaceService) ResolveWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspaceMetadata), args.Error(1)
}

func (m *MockWorkspaceService) CheckOwnership(ctx context.Context, userID string, meta *types.WorkspaceMetadata) error {
	return m.Called(ctx, userID, meta).Error(0)
}

func (m *MockWorkspaceService) ListWorkspaces(ctx context.Context, userID string, opts types.ListOptions) (*types.WorkspaceListResult, error) {
	args := m.Called(ctx, userID, opts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspaceListResult), args.Error(1)
}

func (m *MockWorkspaceService) DeleteWorkspace(ctx context.Context, userID, workspaceID string) error {
	return m.Called(ctx, userID, workspaceID).Error(0)
}

func (m *MockWorkspaceService) SuspendWorkspace(ctx context.Context, userID, workspaceID string) error {
	return m.Called(ctx, userID, workspaceID).Error(0)
}

func (m *MockWorkspaceService) RestartWorkspace(ctx context.Context, userID, workspaceID string) error {
	return m.Called(ctx, userID, workspaceID).Error(0)
}

func (m *MockWorkspaceService) RefreshWorkspaceCompute(ctx context.Context, userID, workspaceID string) (*types.RefreshWorkspaceResult, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.RefreshWorkspaceResult), args.Error(1)
}

func (m *MockWorkspaceService) GetWorkspaceStatus(ctx context.Context, userID, workspaceID string) (*types.WorkspaceStatusResult, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspaceStatusResult), args.Error(1)
}

func (m *MockWorkspaceService) Start() error { return m.Called().Error(0) }
func (m *MockWorkspaceService) Stop() error  { return m.Called().Error(0) }

func (m *MockWorkspaceService) ActivateWorkspace(ctx context.Context, userID, workspaceID string) (*types.ActivateWorkspaceResponse, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.ActivateWorkspaceResponse), args.Error(1)
}

func (m *MockWorkspaceService) ListWorkspaceSessions(ctx context.Context, userID, workspaceID string) ([]types.SessionListItem, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.SessionListItem), args.Error(1)
}

func (m *MockWorkspaceService) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	return m.Called(ctx, userID, workspaceID, sessionID, title).Error(0)
}

func (m *MockWorkspaceService) MarkSessionSeen(ctx context.Context, userID, workspaceID, sessionID string) error {
	return m.Called(ctx, userID, workspaceID, sessionID).Error(0)
}

func (m *MockWorkspaceService) RenameWorkspace(ctx context.Context, userID, workspaceID, name string) error {
	return m.Called(ctx, userID, workspaceID, name).Error(0)
}

func (m *MockWorkspaceService) SetDevPreview(ctx context.Context, userID, workspaceID string, enabled bool) error {
	return m.Called(ctx, userID, workspaceID, enabled).Error(0)
}

func (m *MockWorkspaceService) EnsureSession(ctx context.Context, userID, workspaceID string) (*types.EnsureSessionResponse, error) {
	args := m.Called(ctx, userID, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.EnsureSessionResponse), args.Error(1)
}

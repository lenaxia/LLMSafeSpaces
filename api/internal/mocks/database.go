// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mocks

import (
	"context"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// MockDatabaseService implements the DatabaseService interface for testing.
type MockDatabaseService struct {
	mock.Mock
}

// Compile-time check against the real interface — not an anonymous copy.
var _ interfaces.DatabaseService = (*MockDatabaseService)(nil)

func (m *MockDatabaseService) GetUser(ctx context.Context, userID string) (*types.User, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.User), args.Error(1)
}

func (m *MockDatabaseService) GetUserByEmail(ctx context.Context, email string) (*types.User, error) {
	args := m.Called(ctx, email)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.User), args.Error(1)
}

func (m *MockDatabaseService) CreateUser(ctx context.Context, user *types.User) error {
	return m.Called(ctx, user).Error(0)
}

func (m *MockDatabaseService) CountUsers(ctx context.Context) (int, error) {
	args := m.Called(ctx)
	return args.Int(0), args.Error(1)
}

func (m *MockDatabaseService) UpdateUser(ctx context.Context, userID string, updates types.UserUpdates) error {
	return m.Called(ctx, userID, updates).Error(0)
}

func (m *MockDatabaseService) DeleteUser(ctx context.Context, userID string) error {
	return m.Called(ctx, userID).Error(0)
}

func (m *MockDatabaseService) SetUserStatus(ctx context.Context, userID string, status types.UserStatus) error {
	return m.Called(ctx, userID, status).Error(0)
}

func (m *MockDatabaseService) GetUserByAPIKey(ctx context.Context, apiKey string) (*types.User, error) {
	args := m.Called(ctx, apiKey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.User), args.Error(1)
}

func (m *MockDatabaseService) CreateAPIKey(ctx context.Context, apiKey *types.APIKey) error {
	return m.Called(ctx, apiKey).Error(0)
}

func (m *MockDatabaseService) ListAPIKeys(ctx context.Context, userID string) ([]*types.APIKey, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.APIKey), args.Error(1)
}

func (m *MockDatabaseService) GetAPIKey(ctx context.Context, userID, keyID string) (*types.APIKey, error) {
	args := m.Called(ctx, userID, keyID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.APIKey), args.Error(1)
}

func (m *MockDatabaseService) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	return m.Called(ctx, userID, keyID).Error(0)
}

func (m *MockDatabaseService) GetAPIKeyRecordByHash(ctx context.Context, keyHash string) (*types.APIKey, error) {
	args := m.Called(ctx, keyHash)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.APIKey), args.Error(1)
}

func (m *MockDatabaseService) UpdateAPIKeyDEK(ctx context.Context, keyID string, wrappedDEK, kekSalt []byte, synced bool) error {
	return m.Called(ctx, keyID, wrappedDEK, kekSalt, synced).Error(0)
}

func (m *MockDatabaseService) ListAPIKeysWithDecrypt(ctx context.Context, userID string) ([]*types.APIKey, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.APIKey), args.Error(1)
}

func (m *MockDatabaseService) GetWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspaceMetadata), args.Error(1)
}

func (m *MockDatabaseService) CreateWorkspace(ctx context.Context, workspace *types.WorkspaceMetadata) error {
	return m.Called(ctx, workspace).Error(0)
}

func (m *MockDatabaseService) UpdateWorkspace(ctx context.Context, workspaceID string, updates types.WorkspaceUpdates) error {
	return m.Called(ctx, workspaceID, updates).Error(0)
}

func (m *MockDatabaseService) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	return m.Called(ctx, workspaceID).Error(0)
}

func (m *MockDatabaseService) ListWorkspaces(ctx context.Context, userID string, limit, offset int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	args := m.Called(ctx, userID, limit, offset)
	var workspaces []*types.WorkspaceMetadata
	if args.Get(0) != nil {
		workspaces = args.Get(0).([]*types.WorkspaceMetadata)
	}
	var pagination *types.PaginationMetadata
	if args.Get(1) != nil {
		pagination = args.Get(1).(*types.PaginationMetadata)
	}
	return workspaces, pagination, args.Error(2)
}

func (m *MockDatabaseService) CountWorkspacesByUserAndOrg(ctx context.Context, userID, orgID string) (int, error) {
	args := m.Called(ctx, userID, orgID)
	return args.Int(0), args.Error(1)
}

func (m *MockDatabaseService) CountActiveWorkspacesByUserAndOrg(ctx context.Context, userID, orgID string) (int, error) {
	args := m.Called(ctx, userID, orgID)
	return args.Int(0), args.Error(1)
}

func (m *MockDatabaseService) SyncWorkspaceVersionInfo(ctx context.Context, workspaceID, imageTag, agentVersion string) {
	m.Called(ctx, workspaceID, imageTag, agentVersion)
}

func (m *MockDatabaseService) MarkWorkspaceDeleted(ctx context.Context, workspaceID string) {
	m.Called(ctx, workspaceID)
}

func (m *MockDatabaseService) CheckPermission(ctx context.Context, userID, resourceType, resourceID, action string) (bool, error) {
	args := m.Called(userID, resourceType, resourceID, action)
	return args.Bool(0), args.Error(1)
}

func (m *MockDatabaseService) CheckResourceOwnership(ctx context.Context, userID, resourceType, resourceID string) (bool, error) {
	args := m.Called(userID, resourceType, resourceID)
	return args.Bool(0), args.Error(1)
}

func (m *MockDatabaseService) Start() error { return m.Called().Error(0) }
func (m *MockDatabaseService) Stop() error  { return m.Called().Error(0) }

func (m *MockDatabaseService) Ping(ctx context.Context) error {
	return m.Called(ctx).Error(0)
}

func (m *MockDatabaseService) ListSessionIndex(ctx context.Context, workspaceID string) ([]types.SessionListItem, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.SessionListItem), args.Error(1)
}

func (m *MockDatabaseService) InsertSessionAlert(ctx context.Context, workspaceID, sessionID, alert string, oldestBusySeconds int) error {
	return m.Called(ctx, workspaceID, sessionID, alert, oldestBusySeconds).Error(0)
}

func (m *MockDatabaseService) ListSessionAlerts(ctx context.Context, workspaceID string, limit int) ([]types.SessionAlert, error) {
	args := m.Called(ctx, workspaceID, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.SessionAlert), args.Error(1)
}

func (m *MockDatabaseService) DeleteSessionIndex(ctx context.Context, workspaceID string) error {
	return m.Called(ctx, workspaceID).Error(0)
}

func (m *MockDatabaseService) DeleteSessionTree(ctx context.Context, workspaceID, sessionID string) error {
	return m.Called(ctx, workspaceID, sessionID).Error(0)
}

func (m *MockDatabaseService) UpsertSessionMessage(ctx context.Context, workspaceID, sessionID string, at time.Time) error {
	return m.Called(ctx, workspaceID, sessionID, at).Error(0)
}

func (m *MockDatabaseService) UpsertSessionTitle(ctx context.Context, workspaceID, sessionID, title string) error {
	return m.Called(ctx, workspaceID, sessionID, title).Error(0)
}

func (m *MockDatabaseService) UpsertSessionParent(ctx context.Context, workspaceID, sessionID, parentID string) error {
	return m.Called(ctx, workspaceID, sessionID, parentID).Error(0)
}

func (m *MockDatabaseService) UpsertSessionContextUsed(ctx context.Context, workspaceID, sessionID string, contextUsed int64) error {
	return m.Called(ctx, workspaceID, sessionID, contextUsed).Error(0)
}

func (m *MockDatabaseService) UpdateSessionLastSeen(ctx context.Context, workspaceID, sessionID string) error {
	return m.Called(ctx, workspaceID, sessionID).Error(0)
}

func (m *MockDatabaseService) ListAllWorkspaceOwners(ctx context.Context) (map[string]string, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(map[string]string), args.Error(1)
}

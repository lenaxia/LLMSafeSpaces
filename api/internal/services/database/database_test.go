// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helper function to create a test config
func createTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Database.Host = "localhost"
	cfg.Database.Port = 5432
	cfg.Database.User = "test"
	cfg.Database.Password = "test"
	cfg.Database.Database = "test"
	cfg.Database.SSLMode = "disable"
	cfg.Database.MaxOpenConns = 10
	cfg.Database.MaxIdleConns = 5
	cfg.Database.ConnMaxLifetime = time.Hour
	return cfg
}

func setupMockDB(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	// Create a mock database connection
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}

	// Create a mock logger
	mockLogger, err := logger.New(true, "debug", "console")
	if err != nil {
		t.Fatalf("Failed to create mock logger: %v", err)
	}

	// Create a mock config
	mockConfig := &config.Config{}
	mockConfig.Database.MaxOpenConns = 10
	mockConfig.Database.MaxIdleConns = 5
	mockConfig.Database.ConnMaxLifetime = 5 * time.Minute

	// Create the database service with the mock DB
	service := &Service{
		Logger: mockLogger,
		Config: mockConfig,
		DB:     db,
	}

	// Return the service, mock, and a cleanup function
	return service, mock, func() {
		db.Close()
	}
}

func TestNew(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")
	cfg := createTestConfig()

	// Mock the database
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock database: %v", err)
	}
	defer db.Close()

	// Create service with mocked DB
	service := &Service{
		Logger: log,
		Config: cfg,
		DB:     db,
	}

	// Test service creation
	assert.NotNil(t, service)
	assert.Equal(t, log, service.Logger)
	assert.Equal(t, cfg, service.Config)
	assert.Equal(t, db, service.DB)
}

func TestPing(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Set up expectations
	mock.ExpectPing()

	// Call the Ping method
	ctx := context.Background()
	err := service.Ping(ctx)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Verify all expectations were met
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestGetUserByAPIKey(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Test case: Valid API key
	ctx := context.Background()
	apiKey := "test_api_key"
	expectedUserID := "user123"
	expectedUsername := "testuser"
	expectedEmail := "test@example.com"
	expectedCreatedAt := time.Now()
	expectedUpdatedAt := time.Now()
	expectedActive := true
	expectedRole := "user"
	expectedStatus := types.UserStatusActive

	// Set up expectations for valid API key
	rows := sqlmock.NewRows([]string{"id", "username", "email", "created_at", "updated_at", "active", "role", "status", "email_verified"}).
		AddRow(expectedUserID, expectedUsername, expectedEmail, expectedCreatedAt, expectedUpdatedAt, expectedActive, expectedRole, expectedStatus, true)

	mock.ExpectQuery("SELECT u.id, u.username, u.email, u.created_at, u.updated_at, u.active, u.role, u.status, u.email_verified FROM users u JOIN api_keys k").
		WithArgs(apiKey).
		WillReturnRows(rows)

	// Call the method
	user, err := service.GetUserByAPIKey(ctx, apiKey)
	assert.NoError(t, err)
	assert.NotNil(t, user)
	assert.Equal(t, expectedUserID, user.ID)
	assert.Equal(t, expectedUsername, user.Username)
	assert.Equal(t, expectedEmail, user.Email)
	assert.Equal(t, expectedActive, user.Active)
	assert.Equal(t, expectedRole, user.Role)
	assert.Equal(t, expectedStatus, user.Status)

	// Test case: Invalid API key
	invalidKey := "invalid_key"
	mock.ExpectQuery("SELECT u.id, u.username, u.email, u.created_at, u.updated_at, u.active, u.role, u.status, u.email_verified FROM users u JOIN api_keys k").
		WithArgs(invalidKey).
		WillReturnError(sql.ErrNoRows)

	// Call the method
	user, err = service.GetUserByAPIKey(ctx, invalidKey)
	assert.NoError(t, err)
	assert.Nil(t, user)

	// Verify all expectations were met
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestCheckResourceOwnership_PropagatesContext proves ctx reaches the DB query
// (US-46.5 / #224 P1-c). database/sql's QueryRowContext fails fast on a
// pre-canceled ctx WITHOUT running the query — so an error return proves
// propagation (a context.Background() regression would run the query).
func TestCheckResourceOwnership_PropagatesContext(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT(.*) FROM workspaces").
		WithArgs("r", "u").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	owned, err := service.CheckResourceOwnership(context.Background(), "u", "workspace", "r")
	require.NoError(t, err)
	require.True(t, owned)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.CheckResourceOwnership(ctx, "u", "workspace", "r")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet(), "canceled ctx must not run the query")
}

// TestCheckPermission_PropagatesContext is the companion for CheckPermission
// (same migration; mirrored coverage).
func TestCheckPermission_PropagatesContext(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT(.*) FROM permissions").
		WithArgs("u", "workspace", "r", "read").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	ok, err := service.CheckPermission(context.Background(), "u", "workspace", "r", "read")
	require.NoError(t, err)
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.CheckPermission(ctx, "u", "workspace", "r", "read")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet(), "canceled ctx must not run the query")
}

func TestCheckResourceOwnership(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Test case: User owns the resource
	userID := "user123"
	resourceID := "resource456"
	resourceType := "workspace"

	// Set up expectations for owned resource
	rows := sqlmock.NewRows([]string{"count"}).AddRow(1)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspaces WHERE id = \\$1 AND user_id = \\$2").
		WithArgs(resourceID, userID).
		WillReturnRows(rows)

	// Call the method
	owned, err := service.CheckResourceOwnership(context.Background(), userID, resourceType, resourceID)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if !owned {
		t.Errorf("Expected resource to be owned by user")
	}

	// Test case: User does not own the resource
	otherUserID := "otheruser"
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspaces WHERE id = \\$1 AND user_id = \\$2").
		WithArgs(resourceID, otherUserID).
		WillReturnRows(rows)

	// Call the method
	owned, err = service.CheckResourceOwnership(context.Background(), otherUserID, resourceType, resourceID)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if owned {
		t.Errorf("Expected resource not to be owned by user")
	}

	// Test case: Unsupported resource type
	_, err = service.CheckResourceOwnership(context.Background(), userID, "unsupported", resourceID)
	if err == nil {
		t.Errorf("Expected error for unsupported resource type, got nil")
	}

	// Verify all expectations were met
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestCheckPermission(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Test case: User has permission
	userID := "user123"
	resourceType := "workspace"
	resourceID := "resource456"
	action := "read"

	// Set up expectations for permission check
	rows := sqlmock.NewRows([]string{"count"}).AddRow(1)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM permissions").
		WithArgs(userID, resourceType, resourceID, action).
		WillReturnRows(rows)

	// Call the method
	hasPermission, err := service.CheckPermission(context.Background(), userID, resourceType, resourceID, action)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if !hasPermission {
		t.Errorf("Expected user to have permission")
	}

	// Test case: User does not have permission
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM permissions").
		WithArgs(userID, resourceType, resourceID, "write").
		WillReturnRows(rows)

	// Call the method
	hasPermission, err = service.CheckPermission(context.Background(), userID, resourceType, resourceID, "write")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if hasPermission {
		t.Errorf("Expected user not to have permission")
	}

	// Verify all expectations were met
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestGetUser(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Test case: User exists
	ctx := context.Background()
	userID := "user123"
	username := "testuser"
	email := "test@example.com"
	createdAt := time.Now()
	updatedAt := time.Now()
	active := true
	role := "user"
	status := types.UserStatusActive

	// Set up expectations
	rows := sqlmock.NewRows([]string{"id", "username", "email", "password_hash", "created_at", "updated_at", "active", "role", "status", "email_verified"}).
		AddRow(userID, username, email, "$2a$10$hash", createdAt, updatedAt, active, role, status, true)
	mock.ExpectQuery("SELECT id, username, email, password_hash, created_at, updated_at, active, role, status, email_verified FROM users WHERE id = \\$1").
		WithArgs(userID).
		WillReturnRows(rows)

	// Call the method
	user, err := service.GetUser(ctx, userID)
	assert.NoError(t, err)
	assert.NotNil(t, user)
	assert.Equal(t, userID, user.ID)
	assert.Equal(t, username, user.Username)
	assert.Equal(t, email, user.Email)
	assert.Equal(t, active, user.Active)
	assert.Equal(t, role, user.Role)
	assert.Equal(t, status, user.Status)

	// Test case: User not found
	mock.ExpectQuery("SELECT id, username, email, password_hash, created_at, updated_at, active, role, status, email_verified FROM users WHERE id = \\$1").
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	// Call the method
	user, err = service.GetUser(ctx, "nonexistent")
	assert.NoError(t, err)
	assert.Nil(t, user)

	// Verify all expectations were met
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateWorkspace(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		ws := &types.WorkspaceMetadata{
			ID:          "ws-uuid-1",
			UserID:      "user-1",
			Name:        "My Workspace",
			Runtime:     "python:3.11",
			StorageSize: "10Gi",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}

		mock.ExpectExec("INSERT INTO workspaces").
			WithArgs(ws.ID, ws.UserID, ws.Name, ws.Runtime, ws.StorageSize, ws.OrgID, ws.CreatedAt, ws.UpdatedAt).
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := service.CreateWorkspace(ctx, ws)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("zero_timestamps_auto_filled", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		ws := &types.WorkspaceMetadata{
			ID:          "ws-uuid-2",
			UserID:      "user-2",
			Name:        "Auto TS Workspace",
			Runtime:     "nodejs:18",
			StorageSize: "5Gi",
		}

		mock.ExpectExec("INSERT INTO workspaces").
			WithArgs(ws.ID, ws.UserID, ws.Name, ws.Runtime, ws.StorageSize, ws.OrgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := service.CreateWorkspace(ctx, ws)
		assert.NoError(t, err)
		assert.False(t, ws.CreatedAt.IsZero())
		assert.False(t, ws.UpdatedAt.IsZero())
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		ws := &types.WorkspaceMetadata{
			ID:     "ws-err",
			UserID: "user-err",
			Name:   "Error Workspace",
		}

		mock.ExpectExec("INSERT INTO workspaces").
			WithArgs(ws.ID, ws.UserID, ws.Name, ws.Runtime, ws.StorageSize, ws.OrgID, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnError(sql.ErrConnDone)

		err := service.CreateWorkspace(ctx, ws)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create workspace")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestGetWorkspace(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		now := time.Now()
		wsID := "ws-uuid-found"

		rows := sqlmock.NewRows([]string{"id", "user_id", "name", "runtime", "storage_size", "image_tag", "agent_version", "created_at", "updated_at", "default_model", "agent_needs_refresh", "credentials_pending_since", "org_id"}).
			AddRow(wsID, "user-1", "My Workspace", "python:3.11", "10Gi", "sha-abc123", "1.15.12", now, now, "", false, nil, nil)

		mock.ExpectQuery("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at").
			WithArgs(wsID).
			WillReturnRows(rows)

		ws, err := service.GetWorkspace(ctx, wsID)
		assert.NoError(t, err)
		assert.NotNil(t, ws)
		assert.Equal(t, wsID, ws.ID)
		assert.Equal(t, "user-1", ws.UserID)
		assert.Equal(t, "My Workspace", ws.Name)
		assert.Equal(t, "python:3.11", ws.Runtime)
		assert.Equal(t, "10Gi", ws.StorageSize)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("not_found_returns_nil", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectQuery("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at").
			WithArgs("nonexistent").
			WillReturnError(sql.ErrNoRows)

		ws, err := service.GetWorkspace(ctx, "nonexistent")
		assert.NoError(t, err)
		assert.Nil(t, ws)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectQuery("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at").
			WithArgs("ws-err").
			WillReturnError(sql.ErrConnDone)

		ws, err := service.GetWorkspace(ctx, "ws-err")
		assert.Error(t, err)
		assert.Nil(t, ws)
		assert.Contains(t, err.Error(), "failed to get workspace")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestGetDefaultModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	assert.NoError(t, err)
	defer db.Close()
	service := &Service{DB: db}
	ctx := context.Background()

	t.Run("returns model when set", func(t *testing.T) {
		mock.ExpectQuery("SELECT default_model FROM workspaces WHERE id").
			WithArgs("ws-1").
			WillReturnRows(sqlmock.NewRows([]string{"default_model"}).AddRow("anthropic/claude-sonnet-4-5"))
		model, err := service.GetDefaultModel(ctx, "ws-1")
		assert.NoError(t, err)
		assert.Equal(t, "anthropic/claude-sonnet-4-5", model)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("returns empty string when null", func(t *testing.T) {
		mock.ExpectQuery("SELECT default_model FROM workspaces WHERE id").
			WithArgs("ws-2").
			WillReturnRows(sqlmock.NewRows([]string{"default_model"}).AddRow(nil))
		model, err := service.GetDefaultModel(ctx, "ws-2")
		assert.NoError(t, err)
		assert.Equal(t, "", model)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("returns empty string when workspace not found", func(t *testing.T) {
		mock.ExpectQuery("SELECT default_model FROM workspaces WHERE id").
			WithArgs("no-such").
			WillReturnError(sql.ErrNoRows)
		model, err := service.GetDefaultModel(ctx, "no-such")
		assert.NoError(t, err)
		assert.Equal(t, "", model)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("returns error on DB failure", func(t *testing.T) {
		mock.ExpectQuery("SELECT default_model FROM workspaces WHERE id").
			WithArgs("ws-err").
			WillReturnError(sql.ErrConnDone)
		model, err := service.GetDefaultModel(ctx, "ws-err")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "get default model")
		assert.Equal(t, "", model)
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestListWorkspaces(t *testing.T) {
	t.Run("multiple_rows", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-list"
		limit := 10
		offset := 0
		now := time.Now()

		mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspaces w").
			WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

		wsRows := sqlmock.NewRows([]string{"id", "user_id", "name", "runtime", "storage_size", "image_tag", "agent_version", "created_at", "updated_at", "default_model", "agent_needs_refresh", "credentials_pending_since", "org_id"}).
			AddRow("ws-1", userID, "Workspace One", "python:3.11", "5Gi", "sha-abc", "1.15.12", now, now, "", false, nil, nil).
			AddRow("ws-2", userID, "Workspace Two", "nodejs:18", "10Gi", "", "", now.Add(-time.Hour), now, "", true, &now, nil)

		mock.ExpectQuery("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at").
			WithArgs(userID, limit, offset).
			WillReturnRows(wsRows)

		wsList, pagination, err := service.ListWorkspaces(ctx, userID, limit, offset)
		assert.NoError(t, err)
		assert.Len(t, wsList, 2)
		assert.Equal(t, 2, pagination.Total)
		assert.Equal(t, "ws-1", wsList[0].ID)
		assert.Equal(t, "ws-2", wsList[1].ID)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("empty_result", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-empty"

		mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspaces w").
			WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

		wsList, pagination, err := service.ListWorkspaces(ctx, userID, 10, 0)
		assert.NoError(t, err)
		assert.Len(t, wsList, 0)
		assert.Equal(t, 0, pagination.Total)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("count_db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM workspaces w").
			WithArgs("user-err").
			WillReturnError(sql.ErrConnDone)

		_, _, err := service.ListWorkspaces(ctx, "user-err", 10, 0)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to count workspaces")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestUpdateWorkspace(t *testing.T) {
	t.Run("name_updated", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		wsID := "ws-update"
		name := "New Name"

		mock.ExpectExec("UPDATE workspaces SET updated_at = NOW\\(\\), name = \\$1 WHERE id = \\$2").
			WithArgs(name, wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpdateWorkspace(ctx, wsID, types.WorkspaceUpdates{Name: &name})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no_fields_is_noop", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		err := service.UpdateWorkspace(context.Background(), "ws-noop", types.WorkspaceUpdates{})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		wsID := "ws-err"
		name := "Error Update"

		mock.ExpectExec("UPDATE workspaces SET updated_at = NOW\\(\\), name = \\$1 WHERE id = \\$2").
			WithArgs(name, wsID).
			WillReturnError(sql.ErrConnDone)

		err := service.UpdateWorkspace(ctx, wsID, types.WorkspaceUpdates{Name: &name})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to update workspace")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestDeleteWorkspace(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		wsID := "ws-delete"

		mock.ExpectExec("DELETE FROM workspaces WHERE id = \\$1").
			WithArgs(wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.DeleteWorkspace(ctx, wsID)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("not_found_is_ok", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec("DELETE FROM workspaces WHERE id = \\$1").
			WithArgs("nonexistent").
			WillReturnResult(sqlmock.NewResult(0, 0))

		err := service.DeleteWorkspace(ctx, "nonexistent")
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec("DELETE FROM workspaces WHERE id = \\$1").
			WithArgs("ws-err").
			WillReturnError(sql.ErrConnDone)

		err := service.DeleteWorkspace(ctx, "ws-err")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to delete workspace")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestCreateUser(t *testing.T) {
	t.Run("success_with_explicit_timestamps", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		now := time.Now()
		user := &types.User{
			ID:           "user-abc",
			Username:     "alice",
			Email:        "alice@example.com",
			PasswordHash: "$2a$10$hash",
			CreatedAt:    now,
			UpdatedAt:    now,
			Active:       true,
			Role:         "user",
		}

		// G8: CreateUser is now QueryRowContext (RETURNING role).
		mock.ExpectQuery("WITH existing AS").
			WithArgs(user.ID, user.Username, user.Email, user.PasswordHash, user.CreatedAt, user.UpdatedAt, user.Active, user.Role).
			WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("user"))

		err := service.CreateUser(ctx, user)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("success_zero_timestamps_auto_filled", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		user := &types.User{
			ID:       "user-xyz",
			Username: "bob",
			Email:    "bob@example.com",
			Active:   false,
			Role:     "admin",
			// CreatedAt and UpdatedAt intentionally zero
		}

		mock.ExpectQuery("WITH existing AS").
			WithArgs(user.ID, user.Username, user.Email, user.PasswordHash, sqlmock.AnyArg(), sqlmock.AnyArg(), user.Active, user.Role).
			WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))

		err := service.CreateUser(ctx, user)
		assert.NoError(t, err)
		assert.False(t, user.CreatedAt.IsZero())
		assert.False(t, user.UpdatedAt.IsZero())
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		user := &types.User{
			ID:       "user-dup",
			Username: "dup",
			Email:    "dup@example.com",
		}

		mock.ExpectQuery("WITH existing AS").
			WithArgs(user.ID, user.Username, user.Email, user.PasswordHash, sqlmock.AnyArg(), sqlmock.AnyArg(), user.Active, user.Role).
			WillReturnError(sql.ErrConnDone)

		err := service.CreateUser(ctx, user)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create user")
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	// G8 (Epic 17): When the users table is empty, the SQL CTE
	// promotes the inserted row to admin regardless of the
	// caller-supplied role. The CreateUser call also reflects the
	// actual role into user.Role for the caller.
	t.Run("first_user_is_admin_atomically", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		user := &types.User{
			ID:       "user-first",
			Username: "founder",
			Email:    "founder@example.com",
			Active:   true,
			Role:     "user", // caller passes "user"; DB will return "admin"
		}

		mock.ExpectQuery("WITH existing AS").
			WithArgs(user.ID, user.Username, user.Email, user.PasswordHash, sqlmock.AnyArg(), sqlmock.AnyArg(), user.Active, user.Role).
			WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("admin"))

		err := service.CreateUser(ctx, user)
		assert.NoError(t, err)
		assert.Equal(t, "admin", user.Role,
			"DB-assigned role must be reflected back into user.Role (G8)")
	})
}

func TestDeleteUser(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-to-delete"

		mock.ExpectExec("DELETE FROM users WHERE id = \\$1").
			WithArgs(userID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.DeleteUser(ctx, userID)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("user_not_found_is_not_an_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		// DELETE affecting 0 rows is not an error in the current implementation
		mock.ExpectExec("DELETE FROM users WHERE id = \\$1").
			WithArgs("nonexistent").
			WillReturnResult(sqlmock.NewResult(0, 0))

		err := service.DeleteUser(ctx, "nonexistent")
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec("DELETE FROM users WHERE id = \\$1").
			WithArgs("user-err").
			WillReturnError(sql.ErrConnDone)

		err := service.DeleteUser(ctx, "user-err")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to delete user")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestUpdateUser(t *testing.T) {
	t.Run("username_and_email", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-1"
		username := "alice"
		email := "alice@example.com"

		// Correct query: $1=username, $2=email, $3=userID
		mock.ExpectExec("UPDATE users SET updated_at = NOW\\(\\), username = \\$1, email = \\$2 WHERE id = \\$3").
			WithArgs(username, email, userID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpdateUser(ctx, userID, types.UserUpdates{
			Username: &username,
			Email:    &email,
		})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("single_field_role", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-1"
		role := "admin"

		mock.ExpectExec("UPDATE users SET updated_at = NOW\\(\\), role = \\$1 WHERE id = \\$2").
			WithArgs(role, userID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpdateUser(ctx, userID, types.UserUpdates{Role: &role})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("all_fields", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		userID := "user-1"
		username, email, role := "bob", "bob@example.com", "user"
		active := true

		// $1=username, $2=email, $3=active, $4=role, $5=userID
		mock.ExpectExec("UPDATE users SET updated_at = NOW\\(\\), username = \\$1, email = \\$2, active = \\$3, role = \\$4 WHERE id = \\$5").
			WithArgs(username, email, active, role, userID).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpdateUser(ctx, userID, types.UserUpdates{
			Username: &username,
			Email:    &email,
			Active:   &active,
			Role:     &role,
		})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("no_fields_is_noop", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		// No SQL expected
		err := service.UpdateUser(context.Background(), "user-1", types.UserUpdates{})
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		role := "admin"
		mock.ExpectExec("UPDATE users SET updated_at = NOW\\(\\), role = \\$1 WHERE id = \\$2").
			WithArgs(role, "user-1").
			WillReturnError(sql.ErrConnDone)

		err := service.UpdateUser(ctx, "user-1", types.UserUpdates{Role: &role})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to update user")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestUpdateUser_Status(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	userID := "user-1"
	status := types.UserStatusSuspended

	mock.ExpectExec("UPDATE users SET updated_at = NOW\\(\\), status = \\$1 WHERE id = \\$2").
		WithArgs(string(status), userID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := service.UpdateUser(ctx, userID, types.UserUpdates{Status: &status})
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestSetUserStatus(t *testing.T) {
	// F6 (US-43.19): SetUserStatus now mirrors `active` from `status`
	// (active = (status=='active')) so the two columns cannot drift.
	t.Run("suspended", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		mock.ExpectExec("UPDATE users SET status = \\$1, active = \\$2, updated_at = NOW\\(\\) WHERE id = \\$3").
			WithArgs(string(types.UserStatusSuspended), false, "user-1").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.SetUserStatus(ctx, "user-1", types.UserStatusSuspended)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("active_restores_access", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		mock.ExpectExec("UPDATE users SET status = \\$1, active = \\$2, updated_at = NOW\\(\\) WHERE id = \\$3").
			WithArgs(string(types.UserStatusActive), true, "user-1").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.SetUserStatus(ctx, "user-1", types.UserStatusActive)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()
		mock.ExpectExec("UPDATE users SET status = \\$1, active = \\$2, updated_at = NOW\\(\\) WHERE id = \\$3").
			WithArgs(string(types.UserStatusSuspended), false, "user-1").
			WillReturnError(sql.ErrConnDone)

		err := service.SetUserStatus(ctx, "user-1", types.UserStatusSuspended)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to set user status")
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestMarkWorkspaceDeleted_PurgesBindings is the regression test for
// Bug 11 in worklog 0085: deleting a workspace must also purge its
// user_secret_bindings rows. The bindings table has no FK to
// workspaces.id (column types differ historically), so without an
// explicit DELETE we accumulate orphan rows over time. The two writes
// run inside a single transaction so an API-process crash between
// them cannot leave a soft-deleted workspace with orphan bindings.
func TestMarkWorkspaceDeleted_PurgesBindings(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE workspaces SET deleted_at = NOW.*").
		WithArgs("ws-to-delete").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM user_secret_bindings WHERE workspace_id = .*").
		WithArgs("ws-to-delete").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	service.MarkWorkspaceDeleted(context.Background(), "ws-to-delete")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("MarkWorkspaceDeleted must DELETE bindings inside a tx: %v", err)
	}
}

// TestMarkWorkspaceDeleted_BindingDeleteFailureRollsBack verifies that
// when the bindings DELETE fails the entire transaction rolls back —
// neither the soft-delete nor the bindings purge land. This is
// stronger than the original behavior (which committed the soft-delete
// even if bindings failed) but ensures a clean atomic semantic.
// Operators can retry MarkWorkspaceDeleted on the next reconcile.
func TestMarkWorkspaceDeleted_BindingDeleteFailureRollsBack(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE workspaces SET deleted_at = NOW.*").
		WithArgs("ws-id").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM user_secret_bindings WHERE workspace_id = .*").
		WithArgs("ws-id").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	// Must not panic.
	service.MarkWorkspaceDeleted(context.Background(), "ws-id")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCreateAPIKey_WithDEKWrappingColumns(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	kekSalt := make([]byte, 32)
	wrappedDEK := make([]byte, 48)
	keyCiphertext := make([]byte, 48)
	rand.Read(kekSalt)
	rand.Read(wrappedDEK)
	rand.Read(keyCiphertext)

	apiKey := &types.APIKey{
		ID:            "key-dek-1",
		UserID:        "user-1",
		Key:           "hash-of-key",
		Name:          "dek-key",
		Active:        true,
		CreatedAt:     time.Now(),
		Prefix:        "lsp_",
		Legacy:        false,
		DecryptAccess: true,
		KekSalt:       kekSalt,
		WrappedDEK:    wrappedDEK,
		DekSynced:     true,
		KeyCiphertext: keyCiphertext,
	}

	mock.ExpectExec("INSERT INTO api_keys").
		WithArgs(
			apiKey.ID,
			apiKey.UserID,
			apiKey.Key,
			apiKey.Name,
			apiKey.Active,
			apiKey.CreatedAt,
			nil,
			"lsp_",
			false,
			true,
			kekSalt,
			wrappedDEK,
			true,
			keyCiphertext,
			apiKey.KeyVersion,
			nil,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := service.CreateAPIKey(ctx, apiKey)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateAPIKey_WithoutDEKWrappingColumns(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	apiKey := &types.APIKey{
		ID:        "key-nodek-1",
		UserID:    "user-1",
		Key:       "hash-of-key",
		Name:      "plain-key",
		Active:    true,
		CreatedAt: time.Now(),
		Prefix:    "lsp_",
		Legacy:    false,
	}

	mock.ExpectExec("INSERT INTO api_keys").
		WithArgs(
			apiKey.ID,
			apiKey.UserID,
			apiKey.Key,
			apiKey.Name,
			apiKey.Active,
			apiKey.CreatedAt,
			nil,
			"lsp_",
			false,
			false,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			false,
			sqlmock.AnyArg(),
			apiKey.KeyVersion,
			nil,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := service.CreateAPIKey(ctx, apiKey)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetAPIKeyRecordByHash_WithDEKColumns(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	keyHash := "abc123"
	kekSalt := []byte("salt-32-bytes-long-salt-32-bytes!")
	wrappedDEK := []byte("wrapped-dek-data")
	keyCiphertext := []byte("key-ciphertext")
	createdAt := time.Now()

	rows := sqlmock.NewRows([]string{
		"id", "user_id", "key", "name", "active", "created_at", "expires_at",
		"decrypt_access", "kek_salt", "wrapped_dek", "dek_synced", "key_ciphertext",
		"allowed_cidrs",
	}).AddRow(
		"key-1", "user-1", keyHash, "dek-key", true, createdAt, nil,
		true, kekSalt, wrappedDEK, true, keyCiphertext,
		nil,
	)

	mock.ExpectQuery("SELECT id, user_id, key, name, active, created_at, expires_at").
		WithArgs(keyHash).
		WillReturnRows(rows)

	rec, err := service.GetAPIKeyRecordByHash(ctx, keyHash)
	assert.NoError(t, err)
	assert.NotNil(t, rec)
	assert.Equal(t, "key-1", rec.ID)
	assert.True(t, rec.DecryptAccess)
	assert.Equal(t, kekSalt, rec.KekSalt)
	assert.Equal(t, wrappedDEK, rec.WrappedDEK)
	assert.True(t, rec.DekSynced)
	assert.Equal(t, keyCiphertext, rec.KeyCiphertext)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetAPIKeyRecordByHash_NullDEKColumns(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	createdAt := time.Now()

	rows := sqlmock.NewRows([]string{
		"id", "user_id", "key", "name", "active", "created_at", "expires_at",
		"decrypt_access", "kek_salt", "wrapped_dek", "dek_synced", "key_ciphertext",
		"allowed_cidrs",
	}).AddRow(
		"key-2", "user-1", "hash2", "plain-key", true, createdAt, nil,
		false, nil, nil, false, nil,
		nil,
	)

	mock.ExpectQuery("SELECT id, user_id, key, name, active, created_at, expires_at").
		WithArgs("hash2").
		WillReturnRows(rows)

	rec, err := service.GetAPIKeyRecordByHash(ctx, "hash2")
	assert.NoError(t, err)
	assert.NotNil(t, rec)
	assert.False(t, rec.DecryptAccess)
	assert.Nil(t, rec.KekSalt)
	assert.Nil(t, rec.WrappedDEK)
	assert.False(t, rec.DekSynced)
	assert.Nil(t, rec.KeyCiphertext)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestGetAPIKeyRecordByHash_NotFound(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()

	mock.ExpectQuery("SELECT id, user_id, key, name, active, created_at, expires_at").
		WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	rec, err := service.GetAPIKeyRecordByHash(ctx, "nonexistent")
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestGetAPIKey_PopulatesHashFromKeyColumn is the DB-layer regression test
// for the API key cache eviction fix (PR #597 review finding #1). The
// auth.DeleteAPIKey path relies on GetAPIKey returning the stored hash in
// the .Key field so it can construct the cache key "apikey:<hash>" and
// evict it. Pre-fix, GetAPIKey scanned the key column into a discarded
// local variable; if someone reverts that, .Key is empty, the
// "if existing.Key != \"\"" guard in DeleteAPIKey skips eviction, and the
// bug silently returns — every auth-service test passes because they all
// mock the DatabaseService interface.
//
// This test uses sqlmock to verify the real SQL scan path populates .Key
// from the api_keys.key column, closing the gap the mock-based tests
// cannot cover.
func TestGetAPIKey_PopulatesHashFromKeyColumn(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	storedHash := "deadbeefcafebabe" // what api_keys.key holds (= sha256(plaintext))
	createdAt := time.Now()

	rows := sqlmock.NewRows([]string{
		"id", "key", "name", "active", "created_at", "expires_at",
	}).AddRow(
		"key-1", storedHash, "my-key", true, createdAt, nil,
	)

	mock.ExpectQuery("SELECT id, key, name, active, created_at, expires_at").
		WithArgs("key-1", "user-1").
		WillReturnRows(rows)

	rec, err := service.GetAPIKey(ctx, "user-1", "key-1")
	assert.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "key-1", rec.ID)
	assert.Equal(t, storedHash, rec.Key,
		"GetAPIKey must populate .Key with the stored hash from the api_keys.key "+
			"column — DeleteAPIKey uses it to construct the cache eviction key")
	assert.Equal(t, "my-key", rec.Name)
	assert.True(t, rec.Active)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestGetAPIKey_NotFound_ReturnsNilNil verifies the no-rows case returns
// (nil, nil) rather than an error, matching the contract DeleteAPIKey
// relies on (nil check at auth.go:1368 produces "api key not found").
func TestGetAPIKey_NotFound_ReturnsNilNil(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()

	mock.ExpectQuery("SELECT id, key, name, active, created_at, expires_at").
		WithArgs("nonexistent", "user-1").
		WillReturnError(sql.ErrNoRows)

	rec, err := service.GetAPIKey(ctx, "user-1", "nonexistent")
	assert.NoError(t, err)
	assert.Nil(t, rec)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateAPIKeyDEK(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	newWrapped := []byte("new-wrapped-dek")
	newSalt := []byte("new-salt-32-bytes-long-enough!!")

	mock.ExpectExec("UPDATE api_keys SET wrapped_dek").
		WithArgs(newWrapped, newSalt, true, "key-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := service.UpdateAPIKeyDEK(ctx, "key-1", newWrapped, newSalt, true)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateAPIKeyDEK_SyncFailure(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()

	mock.ExpectExec("UPDATE api_keys SET wrapped_dek").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), false, "key-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := service.UpdateAPIKeyDEK(ctx, "key-1", nil, nil, false)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestListAPIKeysWithDecrypt(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ctx := context.Background()
	createdAt := time.Now()
	salt := []byte("salt")
	wrapped := []byte("wrapped")
	ciphertext := []byte("ciphertext")

	rows := sqlmock.NewRows([]string{
		"id", "user_id", "key", "name", "active", "created_at", "expires_at",
		"decrypt_access", "kek_salt", "wrapped_dek", "dek_synced", "key_ciphertext", "key_version",
	}).AddRow(
		"key-1", "user-1", "hash1", "dek-key", true, createdAt, nil,
		true, salt, wrapped, true, ciphertext, 1,
	).AddRow(
		"key-2", "user-1", "hash2", "another-dek", true, createdAt, nil,
		true, salt, wrapped, false, ciphertext, 1,
	)

	mock.ExpectQuery("SELECT id, user_id, key, name, active, created_at, expires_at").
		WithArgs("user-1").
		WillReturnRows(rows)

	keys, err := service.ListAPIKeysWithDecrypt(ctx, "user-1")
	assert.NoError(t, err)
	assert.Len(t, keys, 2)
	assert.True(t, keys[0].DecryptAccess)
	assert.True(t, keys[0].DekSynced)
	assert.False(t, keys[1].DekSynced)
	assert.Equal(t, 1, keys[0].KeyVersion)
	assert.Equal(t, 1, keys[1].KeyVersion)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDeleteSessionTree(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec(`WITH RECURSIVE descendants`).
			WithArgs("ws-1", "sess-parent").
			WillReturnResult(sqlmock.NewResult(0, 3))

		err := service.DeleteSessionTree(ctx, "ws-1", "sess-parent")
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("not_found_is_ok", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec(`WITH RECURSIVE descendants`).
			WithArgs("ws-1", "nonexistent").
			WillReturnResult(sqlmock.NewResult(0, 0))

		err := service.DeleteSessionTree(ctx, "ws-1", "nonexistent")
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		ctx := context.Background()

		mock.ExpectExec(`WITH RECURSIVE descendants`).
			WithArgs("ws-1", "sess-err").
			WillReturnError(sql.ErrConnDone)

		err := service.DeleteSessionTree(ctx, "ws-1", "sess-err")
		assert.Error(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("workspace_id_in_anchor_and_recursive", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		mock.ExpectExec(`WITH RECURSIVE descendants`).
			WithArgs("ws-1", "sess-x").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.DeleteSessionTree(context.Background(), "ws-1", "sess-x")
		assert.NoError(t, err)
	})

	t.Run("args_are_workspace_first", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		mock.ExpectExec(`WITH RECURSIVE descendants`).
			WithArgs("ws-1", "sess-y").
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.DeleteSessionTree(context.Background(), "ws-1", "sess-y")
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestDeleteSessionTree_SQLStructure(t *testing.T) {
	sql := `WITH RECURSIVE descendants AS (
			SELECT session_id FROM session_index
			WHERE workspace_id = $1 AND session_id = $2
			UNION ALL
			SELECT si.session_id FROM session_index si
			INNER JOIN descendants d ON si.parent_session_id = d.session_id AND si.workspace_id = $1
		)
		DELETE FROM session_index
		WHERE workspace_id = $1 AND session_id IN (SELECT session_id FROM descendants)`

	t.Run("anchor_filters_by_workspace_and_session", func(t *testing.T) {
		assert.Contains(t, sql, "WHERE workspace_id = $1 AND session_id = $2",
			"CTE anchor must filter by both workspace_id and session_id to prevent cross-workspace deletion")
	})

	t.Run("recursive_member_scoped_by_workspace", func(t *testing.T) {
		assert.Contains(t, sql, "si.workspace_id = $1",
			"recursive member must scope traversal to same workspace to prevent cross-workspace tree walk")
	})

	t.Run("delete_scoped_by_workspace", func(t *testing.T) {
		assert.Contains(t, sql, "WHERE workspace_id = $1 AND session_id IN",
			"DELETE must include workspace_id filter as defense-in-depth against cross-workspace data corruption")
	})

	t.Run("uses_union_all_not_union", func(t *testing.T) {
		assert.Contains(t, sql, "UNION ALL",
			"CTE must use UNION ALL (not UNION) for performance — no dedup needed since session_id is PK-scoped")
	})

	t.Run("joins_on_parent_session_id", func(t *testing.T) {
		assert.Contains(t, sql, "si.parent_session_id = d.session_id",
			"recursive join must walk parent_session_id → session_id to traverse the tree downward")
	})
}

func TestUpsertSessionContextUsed(t *testing.T) {
	t.Run("happy_path_upserts_context_used", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		mock.ExpectExec(`INSERT INTO session_index`).
			WithArgs("ws-1", "ses_abc", int64(12500)).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpsertSessionContextUsed(context.Background(), "ws-1", "ses_abc", 12500)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("zero_value_upserts_zero", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		mock.ExpectExec(`INSERT INTO session_index`).
			WithArgs("ws-1", "ses_abc", int64(0)).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := service.UpsertSessionContextUsed(context.Background(), "ws-1", "ses_abc", 0)
		assert.NoError(t, err)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("db_error_is_returned", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		mock.ExpectExec(`INSERT INTO session_index`).
			WithArgs("ws-1", "ses_abc", int64(5000)).
			WillReturnError(fmt.Errorf("connection refused"))

		err := service.UpsertSessionContextUsed(context.Background(), "ws-1", "ses_abc", 5000)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "connection refused")
	})
}

func TestListSessionIndex_IncludesContextUsed(t *testing.T) {
	t.Run("non_null_context_used_is_populated", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		contextUsed := int64(42000)
		rows := sqlmock.NewRows([]string{
			"session_id", "title", "parent_session_id", "last_message_at",
			"message_count", "last_seen_at", "has_unread", "context_used",
		}).AddRow("ses_1", "My Session", nil, nil, 3, nil, false, contextUsed)

		mock.ExpectQuery(`SELECT session_id`).
			WithArgs("ws-1").
			WillReturnRows(rows)

		items, err := service.ListSessionIndex(context.Background(), "ws-1")
		assert.NoError(t, err)
		assert.Len(t, items, 1)
		assert.NotNil(t, items[0].ContextUsed)
		assert.Equal(t, contextUsed, *items[0].ContextUsed)
	})

	t.Run("null_context_used_maps_to_nil", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		rows := sqlmock.NewRows([]string{
			"session_id", "title", "parent_session_id", "last_message_at",
			"message_count", "last_seen_at", "has_unread", "context_used",
		}).AddRow("ses_2", "New Session", nil, nil, 0, nil, false, nil)

		mock.ExpectQuery(`SELECT session_id`).
			WithArgs("ws-1").
			WillReturnRows(rows)

		items, err := service.ListSessionIndex(context.Background(), "ws-1")
		assert.NoError(t, err)
		assert.Len(t, items, 1)
		assert.Nil(t, items[0].ContextUsed, "NULL context_used in DB must map to nil pointer")
	})

	t.Run("zero_context_used_maps_to_pointer_to_zero", func(t *testing.T) {
		service, mock, cleanup := setupMockDB(t)
		defer cleanup()

		rows := sqlmock.NewRows([]string{
			"session_id", "title", "parent_session_id", "last_message_at",
			"message_count", "last_seen_at", "has_unread", "context_used",
		}).AddRow("ses_3", "Empty Session", nil, nil, 1, nil, false, int64(0))

		mock.ExpectQuery(`SELECT session_id`).
			WithArgs("ws-1").
			WillReturnRows(rows)

		items, err := service.ListSessionIndex(context.Background(), "ws-1")
		assert.NoError(t, err)
		assert.Len(t, items, 1)
		assert.NotNil(t, items[0].ContextUsed)
		assert.Equal(t, int64(0), *items[0].ContextUsed)
	})
}

// --- US-43.18: ListAllUsers ---

func userListRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "email", "role", "status", "created_at", "org_id", "org_name"}).
		AddRow("user-1", "a@example.com", "admin", "active", time.Now(), "org-1", "Acme").
		AddRow("user-2", "b@example.com", "user", "suspended", time.Now(), "", "")
}

func TestListAllUsers_NoFilterReturnsAll(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT u.id, u.email, u.role, u.status`).
		WithArgs(50, 0).
		WillReturnRows(userListRows())

	users, page, err := service.ListAllUsers(context.Background(), 50, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, page)
	assert.Equal(t, 2, page.Total)
	require.Len(t, users, 2)
	assert.Equal(t, "user-1", users[0].ID)
	assert.Equal(t, "a@example.com", users[0].Email)
	assert.Equal(t, "org-1", users[0].OrgID)
	assert.Equal(t, "Acme", users[0].OrgName)
	assert.Equal(t, 1, users[0].OrgCount)
	assert.Equal(t, "", users[1].OrgID)
	assert.Equal(t, 0, users[1].OrgCount)
	assert.Equal(t, types.UserStatusSuspended, users[1].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListAllUsers_StatusFilterAppendsWhere(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users WHERE status = \$1`).
		WithArgs("suspended").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`FROM users u.*WHERE u.status = \$1`).
		WithArgs("suspended", 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "status", "created_at", "org_id", "org_name"}).
			AddRow("user-2", "b@example.com", "user", "suspended", time.Now(), "", ""))

	users, page, err := service.ListAllUsers(context.Background(), 50, 0, strPtr("suspended"))
	require.NoError(t, err)
	assert.Equal(t, 1, page.Total)
	require.Len(t, users, 1)
	assert.Equal(t, types.UserStatusSuspended, users[0].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListAllUsers_LimitClampedToDefaultAndMax(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// limit <= 0 ⇒ default 50.
	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT u.id, u.email, u.role, u.status`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "status", "created_at", "org_id", "org_name"}).
			AddRow("u", "a@example.com", "user", "active", time.Now(), "", ""))

	_, _, err := service.ListAllUsers(context.Background(), 0, 0, nil)
	require.NoError(t, err)

	// limit > 200 ⇒ clamped to 200.
	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT u.id, u.email, u.role, u.status`).
		WithArgs(200, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "status", "created_at", "org_id", "org_name"}).
			AddRow("u", "a@example.com", "user", "active", time.Now(), "", ""))

	_, _, err = service.ListAllUsers(context.Background(), 9999, 0, nil)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListAllUsers_EmptyResultSkipsSelect(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	// No SELECT expectation: total == 0 must short-circuit.

	users, page, err := service.ListAllUsers(context.Background(), 50, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, page)
	assert.Equal(t, 0, page.Total)
	assert.Len(t, users, 0)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListAllUsers_CountError(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnError(errors.New("connection refused"))

	users, page, err := service.ListAllUsers(context.Background(), 50, 0, nil)
	require.Error(t, err)
	assert.Nil(t, users)
	assert.Nil(t, page)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListAllUsers_SelectError(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT u.id, u.email, u.role, u.status`).
		WithArgs(50, 0).
		WillReturnError(errors.New("select failed"))

	users, page, err := service.ListAllUsers(context.Background(), 50, 0, nil)
	require.Error(t, err)
	assert.Nil(t, users)
	assert.Nil(t, page)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListAllUsers_QueryShape verifies the LEFT JOIN to org_memberships +
// organizations is present — a regression guard against an accidental refactor
// that drops the org-resolution join (the dashboard relies on it).
func TestListAllUsers_QueryShape(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`COUNT\(\*\) FROM users`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`LEFT JOIN org_memberships m ON m.user_id = u.id.*LEFT JOIN organizations o ON o.id = m.org_id`).
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email", "role", "status", "created_at", "org_id", "org_name"}).
			AddRow("u", "a@example.com", "user", "active", time.Now(), "", ""))

	_, _, err := service.ListAllUsers(context.Background(), 50, 0, nil)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet(),
		"query must LEFT JOIN org_memberships + organizations to resolve org fields")
}

func TestPurgeUserSecrets_DeletesBothTables(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// provider_credentials (user-owned) deleted first, then user_secrets.
	// Both scoped to the user; FK ON DELETE CASCADE clears the bindings.
	mock.ExpectExec(`DELETE FROM provider_credentials WHERE owner_type = 'user' AND owner_id = \$1`).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`DELETE FROM user_secrets WHERE user_id = \$1`).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 2))

	err := service.PurgeUserSecrets(context.Background(), "user-1")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPurgeUserSecrets_DBError(t *testing.T) {
	service, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(`DELETE FROM provider_credentials`).
		WithArgs("user-1").
		WillReturnError(errors.New("connection refused"))

	err := service.PurgeUserSecrets(context.Background(), "user-1")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

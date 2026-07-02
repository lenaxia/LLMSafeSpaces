// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPodIdentity_GetOnEmptyRowReturnsZeroValues proves the fresh-workspace
// contract: when no workspace_agent_state row exists (workspace never
// bound a credential, never observed a pod), GetLastSeenPodIdentity must
// return empty string + zero time + nil error — not sql.ErrNoRows. The
// caller (workspace.Service.GetWorkspaceStatus) relies on this to treat
// the current pod as the FIRST observation (no auto-push fires) rather
// than as a spurious transition.
func TestPodIdentity_GetOnEmptyRowReturnsZeroValues(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COALESCE\(last_seen_pod_name`).
		WithArgs("ws-fresh").
		WillReturnError(sql.ErrNoRows)

	name, startTime, err := svc.GetLastSeenPodIdentity(context.Background(), "ws-fresh")
	require.NoError(t, err, "sql.ErrNoRows must surface as empty identity + nil error")
	assert.Equal(t, "", name)
	assert.True(t, startTime.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPodIdentity_GetReturnsPersistedValues verifies the read path when
// a row is present with an identity previously written by
// UpsertLastSeenPodIdentity.
func TestPodIdentity_GetReturnsPersistedValues(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	stored := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT COALESCE\(last_seen_pod_name`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "start"}).AddRow("pod-abc", stored))

	name, startTime, err := svc.GetLastSeenPodIdentity(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "pod-abc", name)
	assert.True(t, stored.Equal(startTime), "got %v want %v", startTime, stored)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPodIdentity_UpsertInsertsAndUpdates verifies the write path handles
// both the fresh-row (INSERT) and existing-row (UPDATE) cases via
// INSERT ... ON CONFLICT.
func TestPodIdentity_UpsertInsertsAndUpdates(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	now := time.Date(2026, 7, 2, 12, 30, 0, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO workspace_agent_state[\s\S]+last_seen_pod_name[\s\S]+ON CONFLICT`).
		WithArgs("ws-1", "pod-abc", now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertLastSeenPodIdentity(context.Background(), "ws-1", "pod-abc", now)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPodIdentity_MarkTransitionSetsPendingRefresh proves the transition-
// side write: on pod-identity change, we must also flip pending_refresh
// to TRUE and stamp last_credential_changed_at so the AgentReloadBanner
// (fallback UX) appears if the auto-push fails and stays visible until
// a successful MarkAgentReloaded clears it. Batching this into one
// statement (rather than a separate MarkCredentialChanged call) keeps
// the state transition atomic.
func TestPodIdentity_MarkTransitionSetsPendingRefresh(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	now := time.Date(2026, 7, 2, 12, 30, 0, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO workspace_agent_state[\s\S]+last_seen_pod_name[\s\S]+last_credential_changed_at[\s\S]+pending_refresh[\s\S]+ON CONFLICT`).
		WithArgs("ws-1", "pod-new", now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.MarkPodIdentityTransition(context.Background(), "ws-1", "pod-new", now)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

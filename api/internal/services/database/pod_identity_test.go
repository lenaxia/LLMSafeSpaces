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

// TestPodIdentity_GetTreatsEpochStartTimeAsZeroTime handles the edge
// case where a row exists (e.g. one written by MarkCredentialChanged
// before this feature existed, or one where a partial write lost the
// start_time) with last_seen_pod_start_time IS NULL. The COALESCE in
// GetLastSeenPodIdentity substitutes '1970-01-01', and the Go code
// checks Unix()==0 to zero the return value.
//
// Without this normalization, the workspace service's initial-observation
// check (`priorName == "" && priorStart.IsZero()`) would silently be
// false for such rows — leading to a spurious "transition detected"
// on the first status poll after deploy. That would DOS the auto-push
// path against every workspace whose agent-state row was written before
// #494 landed.
func TestPodIdentity_GetTreatsEpochStartTimeAsZeroTime(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// The row exists (name is present) but start_time was NULL — COALESCE
	// returns the epoch. The DB accessor must normalize this to a
	// zero-time so callers can use IsZero() to detect it.
	epoch := time.Unix(0, 0).UTC()
	mock.ExpectQuery(`SELECT COALESCE\(last_seen_pod_name`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"name", "start"}).AddRow("pod-x", epoch))

	name, startTime, err := svc.GetLastSeenPodIdentity(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "pod-x", name,
		"pod name is returned verbatim — a name-only row is still a "+
			"partial observation")
	assert.True(t, startTime.IsZero(),
		"an epoch start_time (NULL post-COALESCE) MUST surface as "+
			"IsZero()=true so the workspace service's initial-observation "+
			"branch fires cleanly; without this, migration-inherited "+
			"rows would trigger spurious transition detections on "+
			"every workspace's first post-deploy status poll")
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
// the state transition atomic. The returned timestamp is what the
// caller feeds back into ClearPendingRefreshAfterAutoPush on success
// (used by MarkAgentReloaded's optimistic-concurrency check).
func TestPodIdentity_MarkTransitionSetsPendingRefresh(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	now := time.Date(2026, 7, 2, 12, 30, 0, 0, time.UTC)
	returnedTs := time.Date(2026, 7, 2, 12, 30, 5, 0, time.UTC)

	mock.ExpectQuery(`INSERT INTO workspace_agent_state[\s\S]+last_seen_pod_name[\s\S]+last_credential_changed_at[\s\S]+pending_refresh[\s\S]+ON CONFLICT[\s\S]+RETURNING last_credential_changed_at`).
		WithArgs("ws-1", "pod-new", now).
		WillReturnRows(sqlmock.NewRows([]string{"last_credential_changed_at"}).AddRow(returnedTs))

	got, err := svc.MarkPodIdentityTransition(context.Background(), "ws-1", "pod-new", now)
	require.NoError(t, err)
	assert.True(t, got.Equal(returnedTs),
		"MarkPodIdentityTransition must return the DB-clock timestamp so the "+
			"caller can round-trip it through ClearPendingRefreshAfterAutoPush")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPodIdentity_ClearPendingRefresh_Success proves the paired write:
// on successful auto-push, the workspace-side ClearPendingRefreshAfterAutoPush
// call wraps the existing MarkAgentReloaded state machine in a self-
// contained transaction. The SELECT FOR UPDATE returns priorChangedAt,
// so newPendingRefresh=false — the banner disappears on the next poll.
func TestPodIdentity_ClearPendingRefresh_Success(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	priorChangedAt := time.Date(2026, 7, 2, 12, 30, 0, 0, time.UTC)
	disposedAt := time.Date(2026, 7, 2, 12, 30, 5, 0, time.UTC)

	mock.ExpectBegin()
	// SELECT FOR UPDATE returns the SAME timestamp we captured — no new
	// credential arrived during the push window, so pending_refresh clears.
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at[\s\S]+FOR UPDATE`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(priorChangedAt))
	mock.ExpectQuery(`INSERT INTO workspace_agent_state[\s\S]+RETURNING last_agent_disposed_at`).
		WithArgs("ws-1", false). // newPendingRefresh=false → banner disappears
		WillReturnRows(sqlmock.NewRows([]string{"last_agent_disposed_at"}).AddRow(disposedAt))
	mock.ExpectCommit()

	got, err := svc.ClearPendingRefreshAfterAutoPush(context.Background(), "ws-1", priorChangedAt)
	require.NoError(t, err)
	assert.True(t, disposedAt.Equal(got))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPodIdentity_ClearPendingRefresh_KeepsFlagOnMidPushBind proves the
// race handled by MarkAgentReloaded's SELECT FOR UPDATE: a credential
// arrives during the push window. currentChangedAt > priorChangedAt →
// newPendingRefresh=true → banner correctly re-appears for the fresh
// change.
func TestPodIdentity_ClearPendingRefresh_KeepsFlagOnMidPushBind(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	priorChangedAt := time.Date(2026, 7, 2, 12, 30, 0, 0, time.UTC)
	midPushChangedAt := priorChangedAt.Add(3 * time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at[\s\S]+FOR UPDATE`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(midPushChangedAt))
	mock.ExpectQuery(`INSERT INTO workspace_agent_state[\s\S]+RETURNING last_agent_disposed_at`).
		WithArgs("ws-1", true). // newPendingRefresh=true → banner stays
		WillReturnRows(sqlmock.NewRows([]string{"last_agent_disposed_at"}).AddRow(time.Now()))
	mock.ExpectCommit()

	_, err := svc.ClearPendingRefreshAfterAutoPush(context.Background(), "ws-1", priorChangedAt)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

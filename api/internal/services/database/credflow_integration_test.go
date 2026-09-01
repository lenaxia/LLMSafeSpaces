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

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
)

// TestCredflowIntegration_NoRowGetReturnsZeroTime proves the read side of the
// credflow state machine: when no row exists (fresh workspace, never bound a
// credential), GetLastCredentialChangedAt must return the zero time and no
// error — not sql.ErrNoRows. The HTTP layer relies on this to render
// agentNeedsRefresh=false on first status poll.
func TestCredflowIntegration_NoRowGetReturnsZeroTime(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at`).
		WithArgs("ws-fresh").
		WillReturnError(sql.ErrNoRows)

	got, err := svc.GetLastCredentialChangedAt(context.Background(), "ws-fresh")
	require.NoError(t, err, "sql.ErrNoRows must surface as zero-time + nil error")
	assert.True(t, got.IsZero(), "fresh workspace should report zero credential-changed time")
}

// TestCredflowIntegration_BindThenReloadClearsPendingRefresh drives the happy
// path of the credflow state machine through the real SQL the production
// database.Service emits:
//
//  1. MarkCredentialChanged — INSERT … ON CONFLICT DO UPDATE with pending_refresh=TRUE
//  2. GetLastCredentialChangedAt — SELECT COALESCE(... '1970-01-01')
//  3. MarkAgentReloaded — SELECT FOR UPDATE + INSERT … ON CONFLICT DO UPDATE
//
// Step 3 must flip pending_refresh to FALSE because no new credential was
// staged during the dispose window (currentChangedAt == priorChangedAt).
//
// This test exists because Epic 30 rewrote the secret-injection path
// (later renamed in PR #407) and the existing
// agent_reload_e2e_test.go mocks the DB entirely; the SQL transitions
// had no regression protection (per worklog 170 item US-27a.9).
func TestCredflowIntegration_BindThenReloadClearsPendingRefresh(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()
	ctx := context.Background()

	// 1. MarkCredentialChanged.
	mock.ExpectExec(`INSERT INTO workspace_agent_state`).
		WithArgs("ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, svc.MarkCredentialChanged(ctx, "ws-1"))

	// 2. GetLastCredentialChangedAt — returns a timestamp.
	bindTime := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(bindTime))

	priorChangedAt, err := svc.GetLastCredentialChangedAt(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, bindTime, priorChangedAt)

	// 3. MarkAgentReloaded inside a transaction.
	// Production code opens the tx with sql.DB.BeginTx. Mock it.
	mock.ExpectBegin()

	// SELECT FOR UPDATE inside MarkAgentReloaded returns the SAME timestamp
	// (no new credential was staged during dispose). newPendingRefresh=false.
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at[\s\S]+FOR UPDATE`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(bindTime))

	disposedAt := time.Date(2026, 6, 15, 12, 0, 5, 0, time.UTC)
	mock.ExpectQuery(`INSERT INTO workspace_agent_state`).
		WithArgs("ws-1", false).
		WillReturnRows(sqlmock.NewRows([]string{"last_agent_disposed_at"}).AddRow(disposedAt))

	mock.ExpectCommit()

	// Open the tx the way agent_reload.go does.
	tx, err := svc.DB.BeginTx(ctx, nil)
	require.NoError(t, err)

	gotDisposedAt, err := svc.MarkAgentReloaded(ctx, tx, "ws-1", priorChangedAt)
	require.NoError(t, err)
	assert.Equal(t, disposedAt, gotDisposedAt)
	require.NoError(t, tx.Commit())
}

// TestCredflowIntegration_MidDisposeBindKeepsPendingRefresh proves the race
// the SELECT FOR UPDATE exists to handle: a credential arrives during the
// dispose window. currentChangedAt > priorChangedAt, so MarkAgentReloaded
// must keep pending_refresh=TRUE so the banner re-appears.
func TestCredflowIntegration_MidDisposeBindKeepsPendingRefresh(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()
	ctx := context.Background()

	priorChangedAt := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	midDisposeChangedAt := priorChangedAt.Add(2 * time.Second)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at[\s\S]+FOR UPDATE`).
		WithArgs("ws-2").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(midDisposeChangedAt))
	// Because midDisposeChangedAt > priorChangedAt, newPendingRefresh must be true.
	mock.ExpectQuery(`INSERT INTO workspace_agent_state`).
		WithArgs("ws-2", true).
		WillReturnRows(sqlmock.NewRows([]string{"last_agent_disposed_at"}).AddRow(time.Now()))
	mock.ExpectCommit()

	tx, err := svc.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = svc.MarkAgentReloaded(ctx, tx, "ws-2", priorChangedAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// TestCredflowIntegration_ReloadOnMissingRowReturnsErrNoAgentStateRow
// proves the documented error contract: MarkAgentReloaded on a workspace with
// no row (e.g. directly called reload on a workspace that never had a
// credential bound) returns apierrors.ErrNoAgentStateRow, which the HTTP layer
// translates into a 409 "first bind a credential".
func TestCredflowIntegration_ReloadOnMissingRowReturnsErrNoAgentStateRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()
	ctx := context.Background()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(last_credential_changed_at[\s\S]+FOR UPDATE`).
		WithArgs("ws-empty").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	tx, err := svc.DB.BeginTx(ctx, nil)
	require.NoError(t, err)

	_, err = svc.MarkAgentReloaded(ctx, tx, "ws-empty", time.Time{})
	assert.ErrorIs(t, err, apierrors.ErrNoAgentStateRow)
	require.NoError(t, tx.Rollback())
}

// TestCredflowIntegration_ListPendingReloadWorkspaces verifies the
// pending-refresh surfacing query that powers the bulk-reload banner:
// only workspaces where pending_refresh=TRUE are returned, with the
// credentials_pending_since timestamp populated.
func TestCredflowIntegration_ListPendingReloadWorkspaces(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	since := time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM workspaces w[\s\S]+JOIN workspace_agent_state s[\s\S]+WHERE w\.user_id = \$1[\s\S]+AND s\.pending_refresh = TRUE`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "user_id", "name", "runtime", "storage_size", "image_tag", "agent_version",
			"created_at", "updated_at", "default_model", "agent_needs_refresh", "credentials_pending_since",
		}).AddRow(
			"ws-1", "user-1", "Test", "python", "10Gi", "v1", "1.2.27",
			time.Now(), time.Now(), "anthropic/claude", true, since,
		))

	got, err := svc.ListPendingReloadWorkspaces(context.Background(), "user-1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "ws-1", got[0].ID)
	assert.True(t, got[0].AgentNeedsRefresh, "pending_refresh must surface as agentNeedsRefresh=true")
}

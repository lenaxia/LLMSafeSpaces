// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListSessionIndex_IncludesParentID verifies the SELECT statement
// returns parent_session_id alongside the other fields, populating the
// ParentID column on the result. This is the data path that the sidebar
// hierarchy depends on — without ParentID the tree builder collapses all
// sessions into roots.
func TestListSessionIndex_IncludesParentID(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used"}).
		AddRow("ses_root", "Root chat", nil, time.Now(), 5, nil, false, nil).
		AddRow("ses_child", "Subagent task", "ses_root", time.Now(), 3, nil, false, int64(8000))

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT session_id, title, parent_session_id, last_message_at, message_count`,
	)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 2)

	assert.Equal(t, "ses_root", items[0].ID)
	assert.Equal(t, "", items[0].ParentID, "top-level session has empty ParentID")
	assert.Nil(t, items[0].ContextUsed)

	assert.Equal(t, "ses_child", items[1].ID)
	assert.Equal(t, "ses_root", items[1].ParentID, "child carries its parent")
	require.NotNil(t, items[1].ContextUsed)
	assert.Equal(t, int64(8000), *items[1].ContextUsed)
}

// TestListSessionIndex_NullParentBecomesEmpty pins the conversion of a
// SQL NULL parent_session_id to an empty Go string. If we ever switch to a
// pointer/optional representation in the type, the sidebar tree builder
// must be updated to match — this test fails loudly in that case.
func TestListSessionIndex_NullParentBecomesEmpty(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used"}).
		AddRow("ses_solo", "Standalone", nil, time.Now(), 1, nil, false, nil)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id, title, parent_session_id`)).
		WithArgs("ws-1").
		WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "", items[0].ParentID)
}

// TestUpsertSessionParent_WritesExpectedColumns verifies that the upsert
// statement writes parent_session_id into the right column with the right
// args. Catches accidental column-rename / arg-order drift.
func TestUpsertSessionParent_WritesExpectedColumns(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, parent_session_id, updated_at)`,
	)).
		WithArgs("ws-1", "ses_child", "ses_parent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertSessionParent(context.Background(), "ws-1", "ses_child", "ses_parent")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertSessionParent_OnConflictUpdatesParent verifies the
// ON CONFLICT clause refreshes parent_session_id rather than inserting a
// duplicate row. The SQL is asserted by string match on the upsert clause.
func TestUpsertSessionParent_OnConflictUpdatesParent(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   parent_session_id = EXCLUDED.parent_session_id`,
	)).
		WithArgs("ws-1", "ses_child", "ses_parent_new").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertSessionParent(context.Background(), "ws-1", "ses_child", "ses_parent_new")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertSessionMessageCount_WritesExpectedColumns pins the #1481
// rebuild upsert: message_count is set ABSOLUTELY (the harness-walked
// ground truth replaces the incrementally-maintained approximation),
// and the DISTINCT guard skips the write when nothing changed — a
// converged workspace must not churn updated_at every 30s pass.
func TestUpsertSessionMessageCount_WritesExpectedColumns(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, message_count, updated_at)`,
	)).
		WithArgs("ws-1", "ses_c1", 507).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertSessionMessageCount(context.Background(), "ws-1", "ses_c1", 507)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertSessionMessageCount_DistinctGuardSkipsNoOpWrites asserts the
// conflict clause only fires on drift (the double-count repair writes
// once, then never again for the same count).
func TestUpsertSessionMessageCount_DistinctGuardSkipsNoOpWrites(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`WHERE session_index.message_count IS DISTINCT FROM EXCLUDED.message_count`,
	)).
		WithArgs("ws-1", "ses_c1", 507).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertSessionMessageCount(context.Background(), "ws-1", "ses_c1", 507)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

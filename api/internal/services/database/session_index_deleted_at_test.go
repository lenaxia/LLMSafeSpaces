// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

// #1882 read-path belt-and-braces: ListSessionIndex (the sidebar's
// only source) must not return rows for sessions the platform has
// deleted — even when the row itself lingers (a failed row-delete, a
// crashed effects tail, a replica that raced). The durable death
// signal is session_index.deleted_at, written by the delete-effects
// tail BEFORE the row delete; the list query filters on it.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListSessionIndex_ExcludesDeletedRows pins the existence filter:
// the query must carry `si.deleted_at IS NULL` so a lingering
// death-marked row stops rendering the moment fixed versions deploy.
// Simulated residue: the row still EXISTS (the delete failed) — the
// filter is the only thing keeping it out of the sidebar.
func TestListSessionIndex_ExcludesDeletedRows(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used", "archived", "short_id", "created_at", "parent_short_id", "origin"}).
		AddRow("ses_live", "Live chat", nil, time.Now(), 2, nil, false, nil, false, "amber-falcon-glides", nil, nil, nil)

	// The residue pin: the expected query carries the death filter.
	// The pre-#1882 query (no deleted_at predicate) fails to match
	// this expectation — the actual SQL text is matched against the
	// regexp, so the pin is RED until the filter exists.
	mock.ExpectQuery("si\\.deleted_at IS NULL").
		WithArgs("ws-1").
		WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "ses_live", items[0].InternalID)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkSessionIndexDeleted_MarksTree: the durable death mark covers
// the session AND its descendants (the DeleteSessionTree shape) — a
// soft-deleted parent must not leave orphan children rendering.
func TestMarkSessionIndexDeleted_MarksTree(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`WITH RECURSIVE descendants`)).
		WithArgs("ws-1", "ses_gone").
		WillReturnResult(sqlmock.NewResult(0, 2))

	err := svc.MarkSessionIndexDeleted(context.Background(), "ws-1", "ses_gone")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

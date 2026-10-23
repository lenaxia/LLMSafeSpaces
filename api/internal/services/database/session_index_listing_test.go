// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"fmt"

	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1886: the sidebar listing's row cap evicted LIVE unarchived
// sessions once a workspace crossed ~100 index rows. The SQL was
// `ORDER BY si.last_message_at DESC NULLS LAST LIMIT 100` — with
// #1880's NULL pool (dispatch-spawned sessions never get a
// last_message_at through the API's recording path), the LIMIT cut a
// 200+-row tie pool with NO secondary sort key, so which ~100 rows
// survived was the database's arbitrary choice: freshly spawned lane
// workers ranked identically to September rows and fell off
// server-side (refresh-proof). These sqlmock pins assert the query
// SHAPE the fix guarantees — a total deterministic order (message
// recency, then created_at recency, then the unique session_id) and
// NO truncation of the workspace's set: the end-anchored regex fails
// the moment any LIMIT trails the chain. The ordering/completeness
// SEMANTICS against real Postgres live in
// session_index_listing_integration_test.go.

// listSessionIndexOrderPin is the #1886 pin: the query must END with
// the full tiebreaker chain — nothing (a LIMIT) may trail it. Dots
// escaped so the column list matches literally; the trailing \s*$
// anchor is what makes the old `... ASC LIMIT 100` fail to match.
const listSessionIndexOrderPin = `ORDER BY si\.last_message_at DESC NULLS LAST, si\.created_at DESC, si\.session_id ASC\s*$`

// listSessionIndexRows builds n well-formed listing rows in the
// SELECT's 12-column shape, created_at descending (row 0 newest) so a
// caller asserting order has a deterministic fixture.
func listSessionIndexRows(n int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at",
		"message_count", "last_seen_at", "has_unread", "context_used",
		"archived", "short_id", "created_at", "parent_short_id",
	})
	base := time.Now()
	for i := 0; i < n; i++ {
		rows.AddRow(
			fmt.Sprintf("ses_row_%03d", i), fmt.Sprintf("Row %d", i), nil, nil,
			0, nil, false, nil, false, nil, base.Add(-time.Duration(i)*time.Minute), nil,
		)
	}
	return rows
}

// TestListSessionIndex_DeterministicOrder_NoTrailingLimit pins the
// full ORDER BY chain AND that the query ends there. The bare
// `LIMIT 100` made the cut through the #1880 NULL tie pool arbitrary
// — which live session got evicted was not decided by recency, which
// is the defect this anchor forbids regressing to.
func TestListSessionIndex_DeterministicOrder_NoTrailingLimit(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(listSessionIndexOrderPin).
		WithArgs("ws-1").
		WillReturnRows(listSessionIndexRows(1))

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListSessionIndex_ReturnsEveryRowPastTheHundredCap guards the
// Go side of the #1886 fix: 125 indexed rows must ALL come back — the
// scan loop keeps no row budget of its own, so a workspace's live
// unarchived sessions (the dispatch-spawned fleet) are never silently
// truncated below the workspace's real session count.
func TestListSessionIndex_ReturnsEveryRowPastTheHundredCap(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	const total = 125
	mock.ExpectQuery(listSessionIndexOrderPin).
		WithArgs("ws-1").
		WillReturnRows(listSessionIndexRows(total))

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, total, "no Go-side cap: every row the query yields is listed")
	for i, it := range items {
		assert.Equal(t, fmt.Sprintf("ses_row_%03d", i), it.InternalID,
			"result order is the query's order (row %d)", i)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListSessionIndex_QueryErrorPropagates: a failed listing must
// surface its error, never degrade to an empty slice — an empty 200
// would render as "no sessions" and hide the fleet a second way.
func TestListSessionIndex_QueryErrorPropagates(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(listSessionIndexOrderPin).
		WithArgs("ws-1").
		WillReturnError(assert.AnError)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.Error(t, err)
	assert.Nil(t, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListSessionIndex_ScanErrorPropagates: a malformed row aborts
// the listing with its error — the same no-silent-empty rule as the
// query-error path (a swallowed scan error would truncate the list
// mid-page with a 200).
func TestListSessionIndex_ScanErrorPropagates(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at",
		"message_count", "last_seen_at", "has_unread", "context_used",
		"archived", "short_id", "created_at", "parent_short_id",
	}).AddRow("ses_bad", "Bad row", nil, "not-a-timestamp", 0, nil, false, nil, false, nil, nil, nil)

	mock.ExpectQuery(listSessionIndexOrderPin).
		WithArgs("ws-1").
		WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.Error(t, err)
	assert.Nil(t, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

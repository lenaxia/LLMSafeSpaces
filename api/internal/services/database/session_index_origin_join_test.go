// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !race

package database

// session_index_origin_join_test.go — ruling 6 (2026-10-28 board
// declutter batch): the session LIST payload itself must expose each
// session's spawn provenance so the sidebar's Workers group is
// populated by board-worker PROVENANCE end-to-end. The spawner already
// stamps the mark at spawn (session_origins row, origin='worker',
// kind='session' — #1966); this test pins the read side: the listing
// query LEFT JOINs session_origins and scans the origin onto
// SessionListItem. Regression form: dropping the join (or the scan)
// fails the sqlmock expectation / returns Origin "" for the worker row.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestListSessionIndex_JoinsSessionOrigins(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// The expectation's pattern must match the issued SQL as a regex;
	// the join clause is the part this test exists to pin. The ::text
	// cast is load-bearing: session_origins.workspace_id is uuid,
	// session_index.workspace_id is text, and the bare compare is a
	// hard `uuid = text` operator error on real Postgres.
	mock.ExpectQuery("LEFT JOIN session_origins so\\s+ON so.session_id = si.session_id AND so.workspace_id::text = si.workspace_id").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"session_id", "title", "parent_session_id", "last_message_at",
			"message_count", "last_seen_at", "has_unread", "context_used",
			"archived", "short_id", "created_at", "parent_short_id", "origin",
		}).
			AddRow("ses-worker", "Board worker", nil, nil, 3, nil, false, nil, false, "amber-falcon-glides", nil, nil, "worker").
			AddRow("ses-user", "User session", nil, nil, 1, nil, false, nil, false, "bold-otter-reads", nil, nil, nil))

	svc := &Service{DB: db}
	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 2)

	require.Equal(t, "worker", items[0].Origin, "the worker-provenance row carries origin=worker on the listing payload")
	require.Equal(t, "", items[1].Origin, "a session with no origin mark reads origin-less (manual user session)")
	require.NoError(t, mock.ExpectationsWereMet())
}

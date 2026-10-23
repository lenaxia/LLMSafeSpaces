// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

// #1882: the durable death mark's SEMANTICS against real Postgres —
// the sqlmock pins assert the SQL text, this suite asserts the
// recursive-tree stamp (session + descendants), the write-free
// idempotent second call, and the ListSessionIndex death filter the
// sidebar's read path depends on (round-1 review's missing-test
// finding 1: no durable execution of the CTE existed).

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

func seedDeletedAtTree(t *testing.T, h *testharness.Harness) {
	t.Helper()
	pool := h.Pool()
	_, _ = pool.Exec(context.Background(), "DELETE FROM session_index WHERE workspace_id = $1", "int-ws-delmark")
	_, err := pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, title) VALUES
		('int-ws-delmark', 'ses_live', 'Live'),
		('int-ws-delmark', 'ses_ghost_parent', 'Ghost parent'),
		('int-ws-delmark', 'ses_ghost_child', 'Ghost child'),
		('int-ws-delmark', 'ses_ghost_grandchild', 'Ghost grandchild')
	`)
	require.NoError(t, err, "seed rows")
	_, err = pool.Exec(context.Background(), `
		UPDATE session_index SET parent_session_id = 'ses_ghost_parent'
		WHERE workspace_id = 'int-ws-delmark' AND session_id = 'ses_ghost_child'
	`)
	require.NoError(t, err, "link child")
	_, err = pool.Exec(context.Background(), `
		UPDATE session_index SET parent_session_id = 'ses_ghost_child'
		WHERE workspace_id = 'int-ws-delmark' AND session_id = 'ses_ghost_grandchild'
	`)
	require.NoError(t, err, "link grandchild")
}

// TestIntegration_MarkSessionIndexDeleted_StampsDescendantTree: the
// death mark covers the session AND its recursive descendants — a
// soft-deleted parent must not leave orphan children rendering.
func TestIntegration_MarkSessionIndexDeleted_StampsDescendantTree(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedDeletedAtTree(t, h)
	pool := h.Pool()
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.MarkSessionIndexDeleted(ctx, "int-ws-delmark", "ses_ghost_parent"))

	var marked, live int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND deleted_at IS NOT NULL`,
		"int-ws-delmark").Scan(&marked))
	assert.Equal(t, 3, marked, "parent + child + grandchild stamped (DeleteSessionTree shape)")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND session_id = 'ses_live' AND deleted_at IS NULL`,
		"int-ws-delmark").Scan(&live))
	assert.Equal(t, 1, live, "the unrelated live row is untouched")
}

// TestIntegration_MarkSessionIndexDeleted_IdempotentSecondCall: the
// `deleted_at IS NULL` guard makes re-marking write-free (rows already
// stamped keep their original mark; no churn on repeated effects).
func TestIntegration_MarkSessionIndexDeleted_IdempotentSecondCall(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedDeletedAtTree(t, h)
	pool := h.Pool()
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.MarkSessionIndexDeleted(ctx, "int-ws-delmark", "ses_ghost_parent"))

	var first sql.NullTime
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT deleted_at FROM session_index WHERE workspace_id = $1 AND session_id = 'ses_ghost_parent'`,
		"int-ws-delmark").Scan(&first))

	require.NoError(t, svc.MarkSessionIndexDeleted(ctx, "int-ws-delmark", "ses_ghost_parent"))

	var second sql.NullTime
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT deleted_at FROM session_index WHERE workspace_id = $1 AND session_id = 'ses_ghost_parent'`,
		"int-ws-delmark").Scan(&second))
	assert.Equal(t, first.Time, second.Time, "the second mark is a no-op (the original stamp survives)")
}

// TestIntegration_ListSessionIndex_FiltersDeathMarkedRows: the
// sidebar's read path returns nothing for a deleted session even when
// its row lingers — the simulated-residue pin against real Postgres.
func TestIntegration_ListSessionIndex_FiltersDeathMarkedRows(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedDeletedAtTree(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	require.NoError(t, svc.MarkSessionIndexDeleted(ctx, "int-ws-delmark", "ses_ghost_parent"))

	items, err := svc.ListSessionIndex(ctx, "int-ws-delmark")
	require.NoError(t, err)
	require.Len(t, items, 1, "only the live row renders — the lingering death-marked tree is filtered")
	assert.Equal(t, "ses_live", items[0].InternalID)
}

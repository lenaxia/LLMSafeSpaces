// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

// #1481: the count-rebuild upsert's persistence contract against real
// Postgres — the sqlmock pins assert the SQL text, this suite asserts
// the SEMANTICS: drift writes absolutely, convergence writes nothing
// (no rows affected, updated_at untouched). The DISTINCT guard is the
// write-free-steady-state promise the 30s reconcile pass depends on.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

func TestIntegration_UpsertSessionMessageCount_DriftWritesAbsolutely(t *testing.T) {
	h := testharness.New(t)
	pool := h.Pool()
	ctx := h.NewContext()

	wsID := "int-test-ws-count-rebuild"
	sesID := "int-test-ses-count-rebuild"
	_, _ = pool.Exec(ctx, "DELETE FROM session_index WHERE workspace_id = $1", wsID)

	_, err := pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at)
		VALUES ($1, $2, 'Drifted', 7, NOW())
	`, wsID, sesID)
	require.NoError(t, err, "seed drifted row")

	svc := &Service{DB: h.SQLDB()}
	err = svc.UpsertSessionMessageCount(ctx, wsID, sesID, 5)
	require.NoError(t, err, "rebuild write")

	var got int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT message_count FROM session_index WHERE workspace_id = $1 AND session_id = $2", wsID, sesID).Scan(&got))
	assert.Equal(t, 5, got, "the drifted count is replaced absolutely (the #754 double-count repair)")
}

func TestIntegration_UpsertSessionMessageCount_ConvergedIsWriteFree(t *testing.T) {
	h := testharness.New(t)
	pool := h.Pool()
	ctx := h.NewContext()

	wsID := "int-test-ws-count-converged"
	sesID := "int-test-ses-count-converged"
	_, _ = pool.Exec(ctx, "DELETE FROM session_index WHERE workspace_id = $1", wsID)

	_, err := pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at, updated_at)
		VALUES ($1, $2, 'Converged', 5, NOW(), NOW() - INTERVAL '1 hour')
	`, wsID, sesID)
	require.NoError(t, err, "seed converged row with an old updated_at")

	var before time.Time
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT updated_at FROM session_index WHERE workspace_id = $1 AND session_id = $2", wsID, sesID).Scan(&before))

	svc := &Service{DB: h.SQLDB()}
	err = svc.UpsertSessionMessageCount(ctx, wsID, sesID, 5)
	require.NoError(t, err)

	var after time.Time
	var rowsAffected int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT updated_at, message_count FROM session_index WHERE workspace_id = $1 AND session_id = $2", wsID, sesID).Scan(&after, &rowsAffected))
	assert.Equal(t, before, after,
		"the DISTINCT guard skips the no-op write — converged workspaces churn nothing every 30s pass")
}

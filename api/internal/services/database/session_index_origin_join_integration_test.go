// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

// TestIntegration_ListSessionIndex_OriginJoin — ruling 6: the session
// listing payload exposes spawn provenance end-to-end against the real
// schema. Seeds one session_index row with a worker origin mark (the
// shape the board spawner stamps at spawn), one without, and one with
// a routine mark; the listing must return the origins on the rows.
//
// session_origins.workspace_id is a uuid FK'd to workspaces(id) while
// session_index.workspace_id is text — so the seed must create a REAL
// workspace row and key every session_index row by its uuid string
// (the production shape), and the join under test must cast
// (so.workspace_id::text — pinned separately in the sqlmock unit
// test; the CI failure class this guards is the hard `operator does
// not exist: uuid = text` error that breaks EVERY session listing).
func TestIntegration_ListSessionIndex_OriginJoin(t *testing.T) {
	h := testharness.New(t)
	pool := h.Pool()
	ctx := h.NewContext()

	wsID := "8e65d000-0000-4000-8000-00000000ba5e"
	userID := "int-test-origin-join-user"
	defer func() {
		_, _ = pool.Exec(ctx, "DELETE FROM session_index WHERE workspace_id = $1", wsID)
		_, _ = pool.Exec(ctx, "DELETE FROM session_origins WHERE workspace_id = $1", wsID)
		_, _ = pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", wsID)
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)
	}()

	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, role)
		VALUES ($1::text, $1::text, $1::text || '@example.test', 'unused-by-api-key-auth', 'user')
		ON CONFLICT (id) DO NOTHING
	`, userID)
	require.NoError(t, err, "seed the workspace owner (FK target)")

	_, err = pool.Exec(ctx, `
		INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size)
		VALUES ($1, 'int-test-origin-join', $2, 'llmsafespaces', 'python:3.11', '1Gi')
		ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, deleted_at = NULL
	`, wsID, userID)
	require.NoError(t, err, "seed the workspace row (session_origins.workspace_id FK target)")

	seed := func(sessionID string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at)
			VALUES ($1, $2, $2, 0, NOW())
		`, wsID, sessionID)
		require.NoError(t, err, "insert seed session %s", sessionID)
	}
	seed("int-test-ses-worker")
	seed("int-test-ses-user")
	seed("int-test-ses-routine")

	_, err = pool.Exec(ctx, `
		INSERT INTO session_origins (session_id, workspace_id, origin, kind, title)
		VALUES ($1, $2, 'worker', 'session', 'worker mark')
	`, "int-test-ses-worker", wsID)
	require.NoError(t, err, "insert worker origin mark")
	_, err = pool.Exec(ctx, `
		INSERT INTO session_origins (session_id, workspace_id, origin, kind, title)
		VALUES ($1, $2, 'routine', 'session', 'routine mark')
	`, "int-test-ses-routine", wsID)
	require.NoError(t, err, "insert routine origin mark")

	svc := &Service{DB: h.SQLDB()}
	items, err := svc.ListSessionIndex(context.Background(), wsID)
	require.NoError(t, err)

	originByInternal := map[string]string{}
	for _, it := range items {
		originByInternal[it.InternalID] = it.Origin
	}
	assert.Equal(t, "worker", originByInternal["int-test-ses-worker"],
		"the board-spawner worker mark reaches the listing payload")
	assert.Equal(t, "routine", originByInternal["int-test-ses-routine"],
		"trigger-class marks reach the listing payload")
	assert.Equal(t, "", originByInternal["int-test-ses-user"],
		"an unmarked session reads origin-less (manual user session)")
}

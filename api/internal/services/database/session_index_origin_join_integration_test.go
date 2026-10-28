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
func TestIntegration_ListSessionIndex_OriginJoin(t *testing.T) {
	h := testharness.New(t)
	pool := h.Pool()
	ctx := h.NewContext()

	wsID := "int-test-ws-origin-join"
	_, _ = pool.Exec(ctx, "DELETE FROM session_index WHERE workspace_id = $1", wsID)
	_, _ = pool.Exec(ctx, "DELETE FROM session_origins WHERE workspace_id = $1", wsID)

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

	_, err := pool.Exec(ctx, `
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

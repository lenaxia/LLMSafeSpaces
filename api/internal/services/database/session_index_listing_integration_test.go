// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

// #1886's production repro shape against real Postgres: a workspace
// past 100 index rows whose NEWEST sessions are dispatch-spawned
// workers with NULL last_message_at (the #1880 class — their message
// streams never traverse the API's recording path). The old query
// (`ORDER BY last_message_at DESC NULLS LAST LIMIT 100`, no
// tiebreaker) cut the NULL tie pool arbitrarily, evicting live
// unarchived workers server-side — refresh-proof, because the hard
// refresh re-ran the same cut. Post-fix the listing is total: a
// deterministic order (recency head, then created_at DESC through the
// NULL pool, then the unique session_id), every row present, and the
// archived count the sidebar badge renders (`archived.length` of this
// response) equals the workspace's true archived total.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

const rowCapWS = "int-ws-rowcap"

// seedRowCapWorkspace builds the issue's live-evidence snapshot, at
// observation scale: 116 rows total — 103 archived NULL-key rows
// (old), 12 unarchived NULL-key worker rows (the NEWEST rows in the
// workspace — exactly the class the cap evicted), and 1 unarchived
// row WITH a recorded last_message_at (the relay-survivor class that
// escaped invisibility only because a relay landed through the
// recorded path).
func seedRowCapWorkspace(t *testing.T, h *testharness.Harness) {
	t.Helper()
	pool := h.Pool()
	_, _ = pool.Exec(context.Background(), "DELETE FROM session_index WHERE workspace_id = $1", rowCapWS)

	now := time.Now()

	// The relay survivor: the only row whose recency signal exists.
	// Spawned oldest of all, but its last_message_at outranks every
	// NULL key — the message-recency head.
	_, err := pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at, archived, created_at)
		VALUES ($1, 'ses-relay-survivor', 'Relay survivor', 395, $2, false, $3)
	`, rowCapWS, now.Add(-1*time.Hour), now.Add(-72*time.Hour))
	require.NoError(t, err, "seed relay survivor")

	// The fleet: 12 dispatch-spawned workers, NULL last_message_at,
	// the newest rows in the workspace (worker-01 = newest). These
	// are the live unarchived sessions the old cap evicted.
	for i := 1; i <= 12; i++ {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at, archived, created_at)
			VALUES ($1, $2, $3, 0, NULL, false, $4)
		`, rowCapWS, fmt.Sprintf("ses-worker-%02d", i), fmt.Sprintf("Worker %02d", i),
			now.Add(-time.Duration(i)*time.Minute))
		require.NoError(t, err, "seed worker row")
	}

	// The archive: 103 archived NULL-key rows, every one older than
	// the whole fleet (arch-030 = now-30min, newest of the block;
	// arch-132 = now-132min, oldest row in the workspace). The
	// $2::timestamptz cast pins the parameter's type — untyped, pgx
	// lets Postgres infer it from the subtraction context as
	// interval (SQLSTATE 42804).
	_, err = pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, title, message_count, last_message_at, archived, created_at)
		SELECT $1, 'ses-arch-' || g::text, 'Archived ' || g::text, 0, NULL, true, $2::timestamptz - make_interval(mins => g)
		FROM generate_series(30, 132) g
	`, rowCapWS, now)
	require.NoError(t, err, "seed archived block")
}

// TestIntegration_ListSessionIndex_LiveUnarchivedSurvivePastHundredRows:
// the #1886 regression — live unarchived sessions MUST survive past
// 100 index rows, newest-first; the archived badge count must be the
// true total.
func TestIntegration_ListSessionIndex_LiveUnarchivedSurvivePastHundredRows(t *testing.T) {
	h := testharness.New(t)
	defer h.Close()
	seedRowCapWorkspace(t, h)
	ctx := h.NewContext()

	svc := &Service{DB: h.SQLDB()}
	items, err := svc.ListSessionIndex(ctx, rowCapWS)
	require.NoError(t, err)

	// The cap pin: ALL 116 rows come back. Pre-fix against this same
	// fixture the listing returned 100 — ~16 rows silently evicted,
	// not the oldest, an arbitrary cut of the NULL tie pool.
	require.Len(t, items, 116, "the workspace's complete set is listed — no cap")

	// Membership: every seeded row is present, the fleet first among
	// equals (the evicted-at-observation class).
	got := make(map[string]bool, len(items))
	for _, it := range items {
		got[it.InternalID] = true
	}
	assert.True(t, got["ses-relay-survivor"], "the relay survivor is listed")
	for i := 1; i <= 12; i++ {
		assert.True(t, got[fmt.Sprintf("ses-worker-%02d", i)],
			"live unarchived worker %02d must be visible past the 100-row mark", i)
	}
	for g := 30; g <= 132; g++ {
		assert.True(t, got[fmt.Sprintf("ses-arch-%d", g)],
			"archived row %d must be listed (the badge count depends on it)", g)
	}

	// The badge-truth pins: the sidebar renders unarchived sessions
	// and counts archived.length over this same response — with the
	// full set both are the workspace's real numbers.
	unarchived, archived := 0, 0
	for _, it := range items {
		if it.Archived {
			archived++
		} else {
			unarchived++
		}
	}
	assert.Equal(t, 13, unarchived, "all 13 live unarchived sessions survive")
	assert.Equal(t, 103, archived, "the badge's archived count is the TRUE total, not a page-local one")

	// The order pin: the message-recency head first, then the NULL
	// pool by created_at DESC — newest-spawned workers first, never
	// an arbitrary member of the tie pool.
	assert.Equal(t, "ses-relay-survivor", items[0].InternalID,
		"the only row with a recency signal heads the list")
	for i := 1; i <= 12; i++ {
		assert.Equal(t, fmt.Sprintf("ses-worker-%02d", i), items[i].InternalID,
			"NULL-key pool orders by created_at DESC — worker %02d at position %d", i, i)
	}
	assert.Equal(t, "ses-arch-30", items[13].InternalID,
		"the archived block follows the fleet, newest-first")
	assert.Equal(t, "ses-arch-132", items[115].InternalID,
		"the oldest archived row closes the list")
}

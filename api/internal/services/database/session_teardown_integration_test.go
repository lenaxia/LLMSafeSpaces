// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1882 teardown-safety pin (the #1881 write-once guard's other side):
// the delete-propagation teardown removes ONLY the dead carrier's row —
// a LIVE row in the same workspace keeps its row AND its minted phrase
// untouched. The hazard class: a delete keyed by phrase instead of
// harness id (or any over-matching key) would strip live identity, the
// regression #1881's write-once trigger blocks from the UPDATE side;
// this pin blocks it from the DELETE side. No local Postgres in pods —
// this rides CI's secrets-integration database (inspection-grade
// locally, house disclosure contract).
func TestIntegration_DeleteSessionTree_RemovesOnlyDeadRow(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1882-teardown"

	dead := "ses_1882_dead_" + h.ID()
	live := "ses_1882_live_" + h.ID()

	deadPhrase, err := svc.EnsureSessionRegistered(ctx, wsID, dead, "opencode")
	require.NoError(t, err)
	livePhrase, err := svc.EnsureSessionRegistered(ctx, wsID, live, "opencode")
	require.NoError(t, err)
	require.True(t, len(deadPhrase) > 0 && len(livePhrase) > 0 && deadPhrase != livePhrase)

	// The carrier's brief life mints activity (message_count > 0 — the
	// kept_for_operator population the reap guard refuses).
	require.NoError(t, svc.UpsertSessionTitle(ctx, wsID, dead, "900-word lighthouse keeper story"))
	require.NoError(t, svc.UpsertSessionTitle(ctx, wsID, live, "a live session"))

	// The teardown under test: exactly the dead row.
	require.NoError(t, svc.DeleteSessionTree(ctx, wsID, dead))

	// The dead row is gone...
	var n int
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		wsID, dead).Scan(&n))
	assert.Equal(t, 0, n, "the dead carrier's row must be removed by the teardown")

	// ...and the live row is INTACT, phrase and all.
	var gotPhrase, gotTitle string
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT short_id, title FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		wsID, live).Scan(&gotPhrase, &gotTitle))
	assert.Equal(t, livePhrase, gotPhrase,
		"a live row's phrase must never be stripped by a dead row's delete (#1881 delete-side guard)")
	assert.Equal(t, "a live session", gotTitle)

	// The dead phrase no longer resolves (FindSessionByShortID is the
	// phrase-resolution surface): identity dies with its session.
	got, err := svc.FindSessionByShortID(ctx, wsID, deadPhrase)
	require.NoError(t, err)
	assert.Empty(t, got, "the deleted carrier's phrase must stop resolving")
	got, err = svc.FindSessionByShortID(ctx, wsID, livePhrase)
	require.NoError(t, err)
	assert.Equal(t, live, got, "the live phrase keeps resolving")
}

// TestIntegration_DeleteSessionTree_DeletesDescendants is the
// pre-existing semantic restated for the teardown's blast radius: rows
// whose parent chain reaches the deleted session die with it (the user
// route's long-standing behavior — a child of a deleted session is
// orphaned on the sidebar). Unrelated live rows survive.
func TestIntegration_DeleteSessionTree_DeletesDescendants(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1882-tree"

	root := "ses_1882_root_" + h.ID()
	child := "ses_1882_child_" + h.ID()
	bystander := "ses_1882_by_" + h.ID()

	_, err := svc.EnsureSessionRegistered(ctx, wsID, root, "opencode")
	require.NoError(t, err)
	_, err = svc.EnsureSessionRegistered(ctx, wsID, child, "opencode")
	require.NoError(t, err)
	_, err = svc.EnsureSessionRegistered(ctx, wsID, bystander, "opencode")
	require.NoError(t, err)
	require.NoError(t, svc.UpsertSessionParent(ctx, wsID, child, root))

	require.NoError(t, svc.DeleteSessionTree(ctx, wsID, root))

	for _, id := range []string{root, child} {
		var n int
		require.NoError(t, svc.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
			wsID, id).Scan(&n))
		assert.Equal(t, 0, n, "%s must be deleted with its parent", id)
	}
	var n int
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		wsID, bystander).Scan(&n))
	assert.Equal(t, 1, n, "the bystander keeps its row")
}

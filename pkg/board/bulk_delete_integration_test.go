// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// bulk_delete_integration_test.go — #1836's bulk DELETE variant at the
// engine seam (the issue's own "delete_task(s)" batch): per-item result
// arrays with partial failures, per-item malformed-id isolation, cap-50
// whole-batch refusal, and per-item single-task delegation to
// Service.DeleteTask — every landed item keeps its own transaction, its
// own delete_task audit event (with the full final-state snapshot),
// its own orphan events, its own post-commit worker abort.
//
// The delete-variant's idempotent-retry rendering (documented delta
// from the claim/move variants, pinned here): a destructive item's
// "already-done" state is row ABSENCE, which is indistinguishable from
// a miss and from a foreign row — all three render the SAME bare
// not_found item (absence ≡ miss ≡ foreign, the never-disclose
// triple-equality). Rendering absence as done would (a) fabricate an
// outcome for a foreign probe and (b) disclose foreign-row existence
// (an existing-but-foreign row returning not_found while an absent row
// returned done). Re-submitting a batch is SAFE — no error, no second
// write, no second audit row, no second abort — which is contract 2's
// substance for a destructive variant.

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bulkDeleteGone asserts the row is really absent (raw count — the
// read seam's NotFoundError is not a suite failure here).
func (s *BoardSuite) bulkDeleteGone(id string) bool {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE id=$1`, id).Scan(&n))
	return n == 0
}

// TestBulk_DeletePerItemResults is the core delete contract: a mixed
// batch deletes what it can, reports each ref verbatim in order, one
// delete_task event (with snapshot) per landed item, one transaction
// per item — and a ref that resolves to nothing is its own not_found
// item, never a batch failure.
func (s *BoardSuite) TestBulk_DeletePerItemResults() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	idleA := s.seedTask(e, "A", nil, true)
	idleB := s.seedTask(e, "B", nil, true)

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{idleA.ShortID, "tsk-nope-does-not-exist", idleB.ShortID, ""}, false, "bulk-del-1")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 4, "one result row per submitted ref, in order")

	assert.Equal(s.T(), []string{idleA.ShortID, "tsk-nope-does-not-exist", idleB.ShortID, ""},
		[]string{res[0].Ref, res[1].Ref, res[2].Ref, res[3].Ref}, "the caller's refs echo verbatim, order preserved")
	assert.Equal(s.T(), board.BulkDone, res[0].Status, "idle A deletes")
	assert.Equal(s.T(), board.BulkFailed, res[1].Status, "a miss is its own item failure")
	require.NotNil(s.T(), res[1].Error)
	assert.Equal(s.T(), board.BulkItemNotFound, res[1].Error.Code)
	assert.Equal(s.T(), board.BulkDone, res[2].Status, "idle B still deletes — per-item isolation")
	assert.Equal(s.T(), board.BulkFailed, res[3].Status, "an empty ref sinks only its own item")
	require.NotNil(s.T(), res[3].Error)
	assert.Equal(s.T(), board.BulkItemNotFound, res[3].Error.Code)

	// Rows really gone; one snapshot-bearing audit event per landed item.
	assert.True(s.T(), s.bulkDeleteGone(idleA.ID), "A's row is gone")
	assert.True(s.T(), s.bulkDeleteGone(idleB.ID), "B's row is gone")
	assert.Equal(s.T(), 2, s.countEvents("delete_task"), "one delete_task event per landed item — no batch blob")
}

// TestBulk_DeleteOrphansEchoedReconnect: the done item echoes the
// single form's OrphanedDependents (and Reconnected pairs under
// reconnect=true) — the affected-party facts the sweep caller needs
// without an events round-trip; the orphan's own unblocked_by_delete
// audit event + is_blocked rederive land per item exactly as the
// single form drives them.
func (s *BoardSuite) TestBulk_DeleteOrphansEchoedReconnect() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	upstream := s.seedTask(e, "up", nil, true)
	mid := s.seedTask(e, "mid", nil, true)
	down := s.seedTask(e, "down", nil, true)
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(), "", down.ShortID, mid.ShortID, ""))
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(), "", mid.ShortID, upstream.ShortID, ""))

	// No reconnect: down is orphaned (unblocked), surfaced on the item.
	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{mid.ShortID}, false, "bulk-del-orph")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 1)
	require.Equal(s.T(), board.BulkDone, res[0].Status)
	assert.Equal(s.T(), []string{down.ShortID}, res[0].Orphaned, "the orphaned dependent is echoed on the done item")
	assert.Empty(s.T(), res[0].Reconnected, "no reconnect pairs without reconnect=true")
	assert.False(s.T(), s.reloadTask(down.ID).IsBlocked, "the orphan's is_blocked rederives to ready")
	assert.Equal(s.T(), 1, s.countEvents("unblocked_by_delete"), "the orphan's own audit event")

	// Reconnect: a second A→B→C chain, delete mid with reconnect=true —
	// down inherits the upstream edge and stays blocked-by-upstream.
	up2 := s.seedTask(e, "up2", nil, true)
	mid2 := s.seedTask(e, "mid2", nil, true)
	down2 := s.seedTask(e, "down2", nil, true)
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(), "", down2.ShortID, mid2.ShortID, ""))
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(), "", mid2.ShortID, up2.ShortID, ""))

	res2, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{mid2.ShortID}, true, "bulk-del-reco")
	require.NoError(s.T(), err)
	require.Len(s.T(), res2, 1)
	require.Equal(s.T(), board.BulkDone, res2[0].Status)
	assert.Equal(s.T(), []string{down2.ShortID}, res2[0].Orphaned)
	assert.Equal(s.T(), []string{down2.ShortID + "→" + up2.ShortID}, res2[0].Reconnected,
		"the inherited pair is echoed on the done item")
	assert.True(s.T(), s.reloadTask(down2.ID).IsBlocked, "reconnect keeps the orphan blocked-by-upstream (the intent)")
}

// TestBulk_DeleteInFlightAbortsOnce: an in-flight item's live worker is
// aborted exactly once, post-commit (the single form's seam), and the
// re-submit of the already-deleted item neither errors nor aborts
// again.
func (s *BoardSuite) TestBulk_DeleteInFlightAbortsOnce() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	claimed := s.seedTask(e, "in-flight", nil, true)
	c, err := s.svc.TaskStart(context.Background(), s.owner(), "", claimed.ShortID, "")
	require.NoError(s.T(), err)
	require.NotEmpty(s.T(), c.Assignee)

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{claimed.ShortID}, false, "bulk-del-abort")
	require.NoError(s.T(), err)
	require.Equal(s.T(), board.BulkDone, res[0].Status)
	assert.Equal(s.T(), 1, s.aborter.count(), "the in-flight worker is aborted once (post-commit cleanup)")
	assert.Equal(s.T(), claimed.ID+"/"+c.Assignee, s.aborter.callsSlice()[0])

	// Re-submit: absence renders not_found; NO second abort.
	res2, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{claimed.ShortID}, false, "bulk-del-abort")
	require.NoError(s.T(), err, "the re-submit is safe — never a batch error")
	assert.Equal(s.T(), board.BulkFailed, res2[0].Status)
	assert.Equal(s.T(), board.BulkItemNotFound, res2[0].Error.Code)
	assert.Equal(s.T(), 1, s.aborter.count(), "no second abort for the already-deleted item")
}

// TestBulk_DeleteIdempotentResubmit is contract 2 for the destructive
// variant: re-submitting the SAME batch (same requestID) is safe — the
// landed items render the truthful absence (not_found, the error
// payload byte-identical to a miss's), no new audit rows appear,
// nothing errors; a
// partial batch (the failed subset alone) re-arbitrates cleanly.
func (s *BoardSuite) TestBulk_DeleteIdempotentResubmit() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID, "tsk-nope-does-not-exist"}, false, "bulk-del-retry")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[a.ShortID].Status)
	assert.Equal(s.T(), board.BulkDone, byRef[b.ShortID].Status)
	assert.Equal(s.T(), board.BulkFailed, byRef["tsk-nope-does-not-exist"].Status)
	require.Equal(s.T(), 2, s.countEvents("delete_task"))

	// Full re-submit under the SAME batch key.
	res2, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID, "tsk-nope-does-not-exist"}, false, "bulk-del-retry")
	require.NoError(s.T(), err, "re-submitting the batch is safe — never an error")
	byRef2 := bulkByID(s.T(), res2)

	// Absence ≡ miss ≡ foreign: the previously-deleted item's error
	// payload is BYTE-IDENTICAL to the never-existed ref's (same code,
	// same detail — no way to tell "deleted earlier" from "never was",
	// which is exactly the never-disclose property; the Ref field echoes
	// each caller's ref verbatim by design, so the ITEMS differ there
	// and only there).
	require.NotNil(s.T(), byRef2[a.ShortID].Error)
	require.NotNil(s.T(), byRef2["tsk-nope-does-not-exist"].Error)
	assert.Equal(s.T(), byRef2["tsk-nope-does-not-exist"].Error, byRef2[a.ShortID].Error,
		"absence renders the exact error a miss renders")
	assert.Equal(s.T(), board.BulkItemNotFound, byRef2[a.ShortID].Error.Code)
	assert.Equal(s.T(), 2, s.countEvents("delete_task"), "no duplicate delete writes on re-submit")

	// The failed subset alone, under a fresh key, is an ordinary batch.
	res3, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{"tsk-nope-does-not-exist"}, false, "bulk-del-retry-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res3[0].Status)
}

// TestBulk_DeleteBatchGuards: empty batch and over-cap are whole-batch
// invalid_input refusals with NOTHING processed (no row vanishes, no
// event lands).
func (s *BoardSuite) TestBulk_DeleteBatchGuards() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	survivor := s.seedTask(e, "survivor", nil, true)

	_, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), nil, false, "g1")
	require.Error(s.T(), err)
	g, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	assert.Equal(s.T(), board.GuardInput, g.Code)

	refs := make([]string, board.BulkMaxItems+1)
	for i := range refs {
		refs[i] = survivor.ShortID
	}
	_, err = s.svc.BulkDeleteTasks(context.Background(), s.owner(), refs, false, "g2")
	require.Error(s.T(), err)
	g2, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	assert.Equal(s.T(), board.GuardInput, g2.Code)
	assert.Contains(s.T(), g2.Detail, "split the batch")

	assert.False(s.T(), s.bulkDeleteGone(survivor.ID), "an over-cap batch deletes NOTHING")
	assert.Zero(s.T(), s.countEvents("delete_task"))
}

// TestBulk_DeleteDistinctItemsShareBatchRequestID: one batch key, two
// items — the derived per-item keys (batch#ref) never collide: two
// distinct audit events land.
func (s *BoardSuite) TestBulk_DeleteDistinctItemsShareBatchRequestID() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, false, "one-batch-key")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res[0].Status)
	assert.Equal(s.T(), board.BulkDone, res[1].Status)
	assert.Equal(s.T(), 2, s.countEvents("delete_task"),
		"two items under one batch key are two audit rows — the derived keys never collide")
}

// TestBulk_DeleteTripleEqualityNeverDisclose: the never-disclose
// triple-equality at the item tier — a foreign row, a miss, and an
// absent (previously-deleted) row render the IDENTICAL item on the bulk
// delete envelope; the foreign row is untouched.
func (s *BoardSuite) TestBulk_DeleteTripleEqualityNeverDisclose() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	foreign := s.foreignUserProject()
	doomed := s.seedTask(e, "doomed", nil, true)

	_, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{doomed.ShortID}, false, "tri-1")
	require.NoError(s.T(), err)

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(),
		[]string{foreign.ShortID, "tsk-nope-does-not-exist", doomed.ShortID}, false, "tri-2")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 3)
	for _, item := range res {
		assert.Equal(s.T(), board.BulkFailed, item.Status)
		require.NotNil(s.T(), item.Error)
		assert.Equal(s.T(), board.BulkItemNotFound, item.Error.Code)
		assert.NotContains(s.T(), item.Error.Detail, "did you mean", "bare not_found — no suggestions on the bulk envelope")
	}
	// Refs echo verbatim (differ by construction); the never-disclose
	// property lives in the error payload: byte-identical code + detail
	// across absence, miss, and foreign.
	assert.Equal(s.T(), res[1].Error, res[2].Error, "absence ≡ miss: byte-identical error payloads")
	assert.Equal(s.T(), res[0].Error, res[1].Error, "foreign ≡ miss ≡ absence: byte-identical error payloads")
	assert.False(s.T(), s.bulkDeleteGone(foreign.ID), "the foreign row is untouched")
}

// TestBulk_DeleteAgentSurfacePerimeter: the agent-manager actor's
// workspace binding scopes the batch — a foreign-workspace row (same
// owner) renders the same bare not_found, untouched.
func (s *BoardSuite) TestBulk_DeleteAgentSurfacePerimeter() {
	ctx := context.Background()
	ws1 := s.seedWorkspace()
	p1 := s.seedProject(ws1, 5)
	e1 := s.seedEpic(p1, board.EpicActive)
	mine := s.seedTask(e1, "in-ws1", nil, true)

	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 5)
	e2 := s.seedEpic(p2, board.EpicActive)
	other := s.seedTask(e2, "in-ws2", nil, true)

	agentMgr := board.Actor{Kind: board.ActorUser, UserID: testUser,
		Surface: board.SurfaceAgentManager, WorkspaceID: ws1}

	res, err := s.svc.BulkDeleteTasks(ctx, agentMgr, []string{mine.ShortID, other.ShortID}, false, "ag-del-1")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[mine.ShortID].Status, "the caller-workspace's own row deletes")
	foreignItem := byRef[other.ShortID]
	assert.Equal(s.T(), board.BulkFailed, foreignItem.Status)
	require.NotNil(s.T(), foreignItem.Error)
	assert.Equal(s.T(), board.BulkItemNotFound, foreignItem.Error.Code,
		"the workspace binding is the agent perimeter")
	assert.False(s.T(), s.bulkDeleteGone(other.ID), "the foreign-workspace row is untouched")
}

// TestBulk_DeleteNoTokensAndEchoShape: the per-item envelope carries no
// claim-token class fields anywhere (#1864 ruling), and the delete-only
// echo fields render under their JSON names exactly when non-empty.
func (s *BoardSuite) TestBulk_DeleteNoTokensAndEchoShape() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	up := s.seedTask(e, "up", nil, true)
	down := s.seedTask(e, "down", nil, true)
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(), "", down.ShortID, up.ShortID, ""))

	res, err := s.svc.BulkDeleteTasks(context.Background(), s.owner(), []string{up.ShortID}, false, "shape-1")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 1)
	require.Equal(s.T(), board.BulkDone, res[0].Status)

	b, err := json.Marshal(res[0])
	require.NoError(s.T(), err)
	var m map[string]any
	require.NoError(s.T(), json.Unmarshal(b, &m))
	assert.Contains(s.T(), m, "orphaned_dependents", "the orphan echo renders under its JSON name")
	assert.NotContains(s.T(), m, "reconnected", "omitted when empty")
	for k := range m {
		assert.NotContains(s.T(), k, "token", "no token-class field on the bulk projection (#1864)")
		assert.NotContains(s.T(), k, "claim", "no claim-class field on the bulk projection (#1864)")
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// bulk_integration_test.go — #1836's bulk claim/complete variants at the
// engine seam, red-first against origin/main: BulkTaskStart (bulk claim)
// and BulkMoveStage (bulk complete) loop the SINGLE-TASK seams per item
// and render the spec's per-item result contract:
//
//   - response = per-item result array {id, status: done|failed, error?}
//     — partial failures reported, never all-or-nothing (import_plan
//       stays the atomic multi-create by design);
//   - idempotent retry: re-submitting a batch (or its failed subset)
//     is safe — an item the batch already landed (claim: in_progress;
//     move: already at the target stage) returns done, not conflict;
//   - per-item validation: one malformed/missing id sinks only its own
//     item; the rest of the batch still processes;
//   - batch cap (#1836 contract 4): >50 refs is a whole-batch
//     invalid_input refusal, nothing processed;
//   - #1864 ruling: bulk projections NEVER carry live claim tokens —
//     the per-item rows carry id/status/escalated/error only;
//   - never-disclose perimeter: a foreign row renders the same
//     not_found item a miss renders (REST parity — no owner identity,
//     no cross-boundary suggestions).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// bulkByID indexes a result slice by the echoed caller ref.
func bulkByID(t *testing.T, results []board.BulkItemResult) map[string]board.BulkItemResult {
	t.Helper()
	out := make(map[string]board.BulkItemResult, len(results))
	for _, r := range results {
		out[r.Ref] = r
	}
	return out
}

// TestBulk_TaskStartPerItemResults is the core claim contract: mixed
// batch, per-item outcomes, one spawn per landed claim, one task_start
// audit event per landed claim.
func (s *BoardSuite) TestBulk_TaskStartPerItemResults() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	readyA := s.seedTask(e, "A", nil, true)
	readyB := s.seedTask(e, "B", nil, true)
	blocker := s.seedTask(e, "D-blocker", nil, true)
	blockedC := s.seedTask(e, "C-blocked", nil, true)
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(),
		"", blockedC.ShortID, blocker.ShortID, ""))

	res, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{readyA.ShortID, readyB.ShortID, blockedC.ShortID, "tsk-nope-does-not-exist"}, "bulk-req-1")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 4, "one result row per submitted ref, in order")

	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[readyA.ShortID].Status, "ready A claims")
	assert.Equal(s.T(), board.BulkDone, byRef[readyB.ShortID].Status, "ready B claims")
	assert.Equal(s.T(), board.BulkFailed, byRef[blockedC.ShortID].Status, "a blocked row refuses the claim")
	assert.Equal(s.T(), board.GuardStage, byRef[blockedC.ShortID].Error.Code, "the refusal carries the single-task guard code")
	assert.Equal(s.T(), board.BulkFailed, byRef["tsk-nope-does-not-exist"].Status, "a missing id is its own item failure")
	assert.Equal(s.T(), board.BulkItemNotFound, byRef["tsk-nope-does-not-exist"].Error.Code)

	// Per-item ordering preserved (the caller's ref order).
	assert.Equal(s.T(), []string{readyA.ShortID, readyB.ShortID, blockedC.ShortID, "tsk-nope-does-not-exist"},
		[]string{res[0].Ref, res[1].Ref, res[2].Ref, res[3].Ref})

	// Exactly the landed claims: two in_progress rows, two spawns, two
	// task_start events (delegation = N single-task writes, no batch
	// event to double-count).
	assert.Equal(s.T(), board.StageInProgress, s.reloadTask(readyA.ID).Stage)
	assert.Equal(s.T(), board.StageInProgress, s.reloadTask(readyB.ID).Stage)
	assert.Equal(s.T(), 2, s.spawner.count(), "one fresh worker per granted claim (R9)")
	assert.Equal(s.T(), 2, s.countEvents("task_start"))
}

// TestBulk_TaskStartIdempotentRetry: re-submitting the same batch (same
// requestID) is safe — the landed items report done (already claimed),
// never a second spawn; the failed subset re-arbitrates.
func (s *BoardSuite) TestBulk_TaskStartIdempotentRetry() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	readyA := s.seedTask(e, "A", nil, true)
	blockedC := s.seedTask(e, "C", nil, true)
	blocker := s.seedTask(e, "D", nil, true)
	require.NoError(s.T(), s.svc.AddDependency(context.Background(), s.owner(),
		"", blockedC.ShortID, blocker.ShortID, ""))

	_, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{readyA.ShortID, blockedC.ShortID}, "bulk-req-retry")
	require.NoError(s.T(), err)
	require.Equal(s.T(), 1, s.spawner.count())

	// Full re-submit, same batch requestID.
	res, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{readyA.ShortID, blockedC.ShortID}, "bulk-req-retry")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[readyA.ShortID].Status,
		"an already-claimed item returns done, not conflict (spec contract 2)")
	assert.False(s.T(), byRef[readyA.ShortID].Escalated)
	assert.Equal(s.T(), board.BulkFailed, byRef[blockedC.ShortID].Status, "the still-blocked item re-refuses")
	assert.Equal(s.T(), 1, s.spawner.count(), "no second spawn for the already-claimed row")
	assert.Equal(s.T(), 1, s.countEvents("task_start"), "no duplicate claim write")

	// The failed subset alone, under a fresh requestID, re-arbitrates.
	res2, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{blockedC.ShortID}, "bulk-req-retry-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res2[0].Status)
}

// TestBulk_TaskStartEscalated: the attempts-cap escalation is a DONE
// item carrying escalated=true (the single-task Claim.Escalated flag,
// surfaced — never conflated with failure).
func (s *BoardSuite) TestBulk_TaskStartEscalated() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	exhausted := s.seedTask(e, "exhausted", nil, true)
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET attempts=$2 WHERE id=$1`, exhausted.ID, s.cfg.AttemptsCap)
	require.NoError(s.T(), err)

	res, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{exhausted.ShortID}, "bulk-req-esc")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res[0].Status, "escalation is the claim's outcome, not a failure")
	assert.True(s.T(), res[0].Escalated, "the escalation flag rides the per-item row")
	assert.Equal(s.T(), board.StagePendingInput, s.reloadTask(exhausted.ID).Stage)
	assert.Zero(s.T(), s.spawner.count(), "an escalated row spawns no worker")
}

// TestBulk_TaskStartBatchGuards: empty batch and >cap batches are
// whole-batch invalid_input refusals — nothing processes.
func (s *BoardSuite) TestBulk_TaskStartBatchGuards() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	ready := s.seedTask(e, "ready", nil, true)

	_, err := s.svc.BulkTaskStart(context.Background(), s.owner(), nil, "b1")
	require.Error(s.T(), err)
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	assert.Equal(s.T(), board.GuardInput, guard.Code, "an empty batch is caller error")

	var refs []string
	for i := 0; i < board.BulkMaxItems+1; i++ {
		refs = append(refs, fmt.Sprintf("tsk-filler-%02d", i))
	}
	_, err = s.svc.BulkTaskStart(context.Background(), s.owner(), refs, "b2")
	require.ErrorAs(s.T(), err, &guard)
	assert.Equal(s.T(), board.GuardInput, guard.Code, "over-cap batches refuse whole")
	assert.Contains(s.T(), err.Error(), fmt.Sprintf("%d", board.BulkMaxItems), "the overflow error names the cap")

	assert.Equal(s.T(), board.StageBacklog, s.reloadTask(ready.ID).Stage, "nothing processed on a refused batch")
	assert.Zero(s.T(), s.spawner.count())
	assert.Zero(s.T(), s.countEvents("task_start"))
}

// foreignUserProject seeds a second user's board (the never-disclose
// perimeter fixture).
func (s *BoardSuite) foreignUserProject() *board.Task {
	ctx := context.Background()
	foreign := "board-test-foreign-user"
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1, $1, 'foreign@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING`, foreign)
	s.Require().NoError(err)
	ws := s.seedWorkspaceFor(foreign)
	actor := board.Actor{Kind: board.ActorUser, UserID: foreign, Surface: board.SurfaceREST}
	p, err := s.svc.CreateProject(ctx, actor,
		board.ProjectInput{Name: "foreign-proj", TargetWorkspaceID: ws, WorkerSlots: 2, Tags: []string{}}, "")
	s.Require().NoError(err)
	e, err := s.svc.CreateEpic(ctx, actor, p.ID, "", board.EpicInput{Title: "foreign-epic"}, "")
	s.Require().NoError(err)
	t, err := s.svc.CreateTask(ctx, actor, e.ID, "",
		board.TaskInput{Title: "foreign-task", Description: "charter",
			Tags: []string{}, AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}}}, "")
	s.Require().NoError(err)
	return t
}

// seedWorkspaceFor inserts a workspace owned by an arbitrary user.
func (s *BoardSuite) seedWorkspaceFor(userID string) string {
	id := uuid.NewString()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		 VALUES ($1, $2, $3, 'default', now(), now())`, id, userID, "ws-"+id[:8])
	s.Require().NoError(err)
	return id
}

// TestBulk_ForeignRowNeverDisclosed: a foreign row renders the same
// not_found item a miss renders — no cross-boundary suggestions, no
// owner identity (the REST read perimeter, per-item).
func (s *BoardSuite) TestBulk_ForeignRowNeverDisclosed() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	ready := s.seedTask(e, "mine", nil, true)
	foreign := s.foreignUserProject()

	res, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{ready.ShortID, foreign.ShortID}, "b-foreign")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[ready.ShortID].Status)
	foreignItem := byRef[foreign.ShortID]
	assert.Equal(s.T(), board.BulkFailed, foreignItem.Status)
	require.NotNil(s.T(), foreignItem.Error)
	assert.Equal(s.T(), board.BulkItemNotFound, foreignItem.Error.Code,
		"a foreign row is the same not_found a miss is — never disclosed")
	assert.Equal(s.T(), board.StageBacklog, s.reloadTask(foreign.ID).Stage, "the foreign row is untouched")

	// The move variant shares the perimeter.
	res2, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{foreign.ShortID}, board.StageDone, "https://example/e", "b-foreign-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res2[0].Status)
	assert.Equal(s.T(), board.BulkItemNotFound, res2[0].Error.Code)
}

// bulkToReview moves a backlog task through claim → in_review using the
// single-task seams (the worker's forward write), returning it in_review.
func (s *BoardSuite) bulkToReview(t *board.Task, wsID string) {
	ctx := context.Background()
	c, err := s.svc.TaskStart(ctx, s.owner(), "", t.ShortID, "")
	s.Require().NoError(err)
	s.Require().False(c.Escalated)
	_, err = s.svc.MoveStage(ctx, s.agent("worker-"+wsID, c.ClaimToken), "", t.ShortID,
		board.StageInReview, "https://example/work", "")
	s.Require().NoError(err)
}

// TestBulk_MoveStageCompletesReviewBatch: the owner's bulk complete —
// in_review → done across a batch (the review-settlement sweep), one
// board_transitions row per landed item.
func (s *BoardSuite) TestBulk_MoveStageCompletesReviewBatch() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)
	s.bulkToReview(a, ws)
	s.bulkToReview(b, ws)

	res, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID, "tsk-nope-does-not-exist"},
		board.StageDone, "https://example/proof", "bulk-move-1")
	require.NoError(s.T(), err)
	require.Len(s.T(), res, 3)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[a.ShortID].Status)
	assert.Equal(s.T(), board.BulkDone, byRef[b.ShortID].Status)
	assert.Equal(s.T(), board.BulkFailed, byRef["tsk-nope-does-not-exist"].Status)
	assert.Equal(s.T(), board.BulkItemNotFound, byRef["tsk-nope-does-not-exist"].Error.Code)

	assert.Equal(s.T(), board.StageDone, s.reloadTask(a.ID).Stage)
	assert.Equal(s.T(), board.StageDone, s.reloadTask(b.ID).Stage)

	var transitions int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_transitions WHERE to_stage=$1`, board.StageDone).Scan(&transitions))
	assert.Equal(s.T(), 2, transitions, "one board_transitions row per landed item (the single-task convention)")
}

// TestBulk_MoveStagePartialFailure: a done-guard miss on one item never
// sinks the batch — the failed item carries the single-task guard code.
func (s *BoardSuite) TestBulk_MoveStagePartialFailure() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	met := s.seedTask(e, "criteria-met", nil, true)
	unmet := s.seedTask(e, "criteria-unmet", nil, false)
	s.bulkToReview(met, ws)
	s.bulkToReview(unmet, ws)

	res, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{met.ShortID, unmet.ShortID}, board.StageDone, "https://example/proof", "bulk-move-2")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[met.ShortID].Status, "the healthy item completes")
	assert.Equal(s.T(), board.BulkFailed, byRef[unmet.ShortID].Status, "the unmet-criteria item refuses")
	require.NotNil(s.T(), byRef[unmet.ShortID].Error)
	assert.Equal(s.T(), board.GuardDone, byRef[unmet.ShortID].Error.Code, "the Done guard's own code")
	assert.Equal(s.T(), board.StageDone, s.reloadTask(met.ID).Stage)
	assert.Equal(s.T(), board.StageInReview, s.reloadTask(unmet.ID).Stage)
}

// TestBulk_MoveStageIdempotentResubmit: an item already at the target
// stage returns done with NO new write (spec contract 2 — already-done
// items return done, not conflict); fixing the failed item and
// re-submitting the same batch+requestID lands only the fix.
func (s *BoardSuite) TestBulk_MoveStageIdempotentResubmit() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, false)
	s.bulkToReview(a, ws)
	s.bulkToReview(b, ws)

	// Round 1: A lands, B refuses (criteria unmet).
	res, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, board.StageDone, "https://example/proof", "bulk-move-3")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res[0].Status)
	assert.Equal(s.T(), board.BulkFailed, res[1].Status)

	// Same batch, same requestID, after the human checks B's criterion.
	_, err = s.svc.UpdateTask(context.Background(), s.owner(), "", b.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: 0, Met: true}}, "")
	require.NoError(s.T(), err)
	_, err = s.svc.UpdateTask(context.Background(), s.owner(), "", b.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: 1, Met: true}}, "")
	require.NoError(s.T(), err)

	res2, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, board.StageDone, "https://example/proof", "bulk-move-3")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res2)
	assert.Equal(s.T(), board.BulkDone, byRef[a.ShortID].Status,
		"the already-done item returns done, not conflict")
	assert.Equal(s.T(), board.BulkDone, byRef[b.ShortID].Status, "the repaired item lands")

	var transitions int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_transitions WHERE to_stage=$1`, board.StageDone).Scan(&transitions))
	assert.Equal(s.T(), 2, transitions, "exactly one transition per landed item across both rounds")

	// And a pure no-op re-run writes nothing.
	res3, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, board.StageDone, "https://example/proof", "bulk-move-3")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res3[0].Status)
	assert.Equal(s.T(), board.BulkDone, res3[1].Status)
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_transitions WHERE to_stage=$1`, board.StageDone).Scan(&transitions))
	assert.Equal(s.T(), 2, transitions)
}

// TestBulk_MoveStageBatchGuards: invalid target stage, empty batch, and
// over-cap batches refuse whole with invalid_input — per-item legality
// (from-stage enumeration) stays per-item.
func (s *BoardSuite) TestBulk_MoveStageBatchGuards() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "t1", nil, true)
	s.bulkToReview(t1, ws)

	var guard *board.GuardError

	_, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{t1.ShortID}, "cancel", "", "g1")
	require.ErrorAs(s.T(), err, &guard)
	assert.Equal(s.T(), board.GuardInput, guard.Code, "an off-vocabulary target refuses the whole batch")

	_, err = s.svc.BulkMoveStage(context.Background(), s.owner(), nil, board.StageDone, "", "g2")
	require.ErrorAs(s.T(), err, &guard)
	assert.Equal(s.T(), board.GuardInput, guard.Code)

	var refs []string
	for i := 0; i < board.BulkMaxItems+1; i++ {
		refs = append(refs, fmt.Sprintf("tsk-filler-%02d", i))
	}
	_, err = s.svc.BulkMoveStage(context.Background(), s.owner(), refs, board.StageDone, "", "g3")
	require.ErrorAs(s.T(), err, &guard)
	assert.Equal(s.T(), board.GuardInput, guard.Code)

	assert.Equal(s.T(), board.StageInReview, s.reloadTask(t1.ID).Stage, "nothing processed on refused batches")

	// A legal target that is not enumerated FROM this row's stage is a
	// per-item guard failure (the single-task arbitration), not a batch
	// error: in_review → backlog is not in the machine.
	res, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{t1.ShortID}, board.StageBacklog, "", "g4")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res[0].Status)
	assert.Equal(s.T(), board.GuardStage, res[0].Error.Code)
}

// TestBulk_MoveStageDistinctItemsShareBatchRequestID: one batch
// requestID must not collide two same-project items into one audit
// event — per-item derived keys keep delegation's dedup correct.
func (s *BoardSuite) TestBulk_MoveStageDistinctItemsShareBatchRequestID() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)
	s.bulkToReview(a, ws)
	s.bulkToReview(b, ws)

	res, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, board.StageDone, "https://example/proof", "same-req")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res[0].Status, "both items land under one batch requestID: %v", res)
	assert.Equal(s.T(), board.BulkDone, res[1].Status)

	var events int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='move_stage' AND payload->>'resulting_stage'='done'`).Scan(&events))
	assert.Equal(s.T(), 2, events, "two items, two distinct audit events")
}

// TestBulk_NoClaimTokensInBulkResults pins the #1864 ruling extension:
// the bulk projection NEVER carries live claim tokens — the per-item
// rows are id/status/escalated/error only (the token stays on the
// per-task read for the claimant's own flow).
func (s *BoardSuite) TestBulk_NoClaimTokensInBulkResults() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)
	c := s.seedTask(e, "C", nil, true)

	startRes, err := s.svc.BulkTaskStart(context.Background(), s.owner(),
		[]string{a.ShortID, b.ShortID}, "tok-1")
	require.NoError(s.T(), err)
	for _, r := range startRes {
		assert.Equal(s.T(), board.BulkDone, r.Status, "claim items land: %v", startRes)
	}
	// The move arm needs rows the owner may move: land them in_review
	// through the worker's single-task forward writes first.
	s.bulkToReview(c, ws)
	moveRes, err := s.svc.BulkMoveStage(context.Background(), s.owner(),
		[]string{c.ShortID}, board.StageDone, "https://example/p", "tok-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, moveRes[0].Status)

	for name, rows := range map[string][]board.BulkItemResult{"start": startRes, "move": moveRes} {
		blob, err := json.Marshal(rows)
		require.NoError(s.T(), err)
		assert.NotContains(s.T(), string(blob), "token", "%s results must carry no claim token (%s)", name, blob)
	}
}

// TestBulk_WorkerTokenGuardHoldsPerItem: a worker-actor bulk move keeps
// the single-task claim-token arbitration — the token's own row moves,
// every other in_progress row refuses claim_token.
func (s *BoardSuite) TestBulk_WorkerTokenGuardHoldsPerItem() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)
	mine := s.seedTask(e, "mine", nil, true)
	theirs := s.seedTask(e, "theirs", nil, true)

	cMine, err := s.svc.TaskStart(context.Background(), s.owner(), "", mine.ShortID, "")
	require.NoError(s.T(), err)
	_, err = s.svc.TaskStart(context.Background(), s.owner(), "", theirs.ShortID, "")
	require.NoError(s.T(), err)

	res, err := s.svc.BulkMoveStage(context.Background(),
		s.agent("worker-"+ws, cMine.ClaimToken),
		[]string{mine.ShortID, theirs.ShortID}, board.StageInReview, "https://example/w", "w-1")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[mine.ShortID].Status, "the token's own row moves")
	assert.Equal(s.T(), board.BulkFailed, byRef[theirs.ShortID].Status, "a foreign claimed row refuses")
	assert.Equal(s.T(), board.GuardClaimToken, byRef[theirs.ShortID].Error.Code)
}

// TestBulk_PreCheckBoundaryDelegatesAuthority pins the pre-checks'
// read-then-act boundary (#1836 review r1, missing-test 3): the
// unlocked pre-check NEVER fabricates an outcome — authority is the
// delegated single-task seam's row-locked arbitration.
//
//   - CONVERGE arm: a completion landing between items (deterministic:
//     the failpoint during item 1's write flips item 2 to done) makes
//     item 2's pre-check see the fresh state → done, no second write.
//   - REFUSE arm: a pre-check that passes (stage ≠ target) while the
//     machine refuses the transition (backlog → done is not
//     enumerated) renders failed with the machine's own code — the
//     same path a concurrent write that invalidated the pre-check
//     takes; a retry converges.
func (s *BoardSuite) TestBulk_PreCheckBoundaryDelegatesAuthority() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 5)
	e := s.seedEpic(p, board.EpicActive)

	// REFUSE arm: backlog rows target done — pre-check passes (≠ done),
	// the machine refuses (not enumerated), item failed, nothing written.
	backlog := s.seedTask(e, "backlog", nil, true)
	res, err := s.svc.BulkMoveStage(ctx, s.owner(), []string{backlog.ShortID},
		board.StageDone, "https://example/p", "pc-1")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res[0].Status,
		"a stale pre-check never fabricates done — the seam's arbitration is authoritative")
	require.NotNil(s.T(), res[0].Error)
	assert.Equal(s.T(), board.GuardStage, res[0].Error.Code)
	assert.Equal(s.T(), board.StageBacklog, s.reloadTask(backlog.ID).Stage)
	var transitions int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_transitions WHERE task_id=$1`, backlog.ID).Scan(&transitions))
	assert.Zero(s.T(), transitions, "the refused item writes nothing")

	// CONVERGE arm: two in_review items; item 2 is completed
	// concurrently (failpoint during item 1's write) — item 2's LATER
	// pre-check sees done == target → idempotent done, one write total.
	a := s.seedTask(e, "A", nil, true)
	b := s.seedTask(e, "B", nil, true)
	s.bulkToReview(a, ws)
	s.bulkToReview(b, ws)
	s.fps.armAtFn("move_stage.effect", func() error {
		s.fps.disarm("move_stage.effect")
		_, err := s.pool.Exec(ctx,
			`UPDATE tasks SET stage='done' WHERE id=$1 AND stage='in_review'`, b.ID)
		s.Require().NoError(err)
		return nil
	})
	res2, err := s.svc.BulkMoveStage(ctx, s.owner(), []string{a.ShortID, b.ShortID},
		board.StageDone, "https://example/p", "pc-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkDone, res2[0].Status, "item 1 completes through the seam")
	assert.Equal(s.T(), board.BulkDone, res2[1].Status,
		"the concurrently-completed item reports done (the pre-check re-read is the convergent path)")
	var aDone, bDone int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_transitions WHERE task_id=$1 AND to_stage='done'`, a.ID).Scan(&aDone))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_transitions WHERE task_id=$1 AND to_stage='done'`, b.ID).Scan(&bDone))
	assert.Equal(s.T(), 1, aDone)
	assert.LessOrEqual(s.T(), bDone, 1, "no second done transition for the concurrent item")
}

// TestBulk_AgentSurfacePerimeter pins the agent branch of the bulk
// perimeter (#1836 review r1, missing-test 2): on the agent surfaces
// (worker/manager-grade) the caller-workspace binding IS the scope —
// a task living in a project targeting ANOTHER workspace renders the
// same not_found item a miss renders, even though the owner is the
// same user (the owner≡workspace-owner invariant, made
// load-bearing-visible).
func (s *BoardSuite) TestBulk_AgentSurfacePerimeter() {
	ctx := context.Background()
	ws1 := s.seedWorkspace()
	p1 := s.seedProject(ws1, 5)
	e1 := s.seedEpic(p1, board.EpicActive)
	mine := s.seedTask(e1, "in-ws1", nil, true)

	// A SECOND workspace (same owner) with its own board: unreachable
	// from an agent actor bound to ws1.
	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 5)
	e2 := s.seedEpic(p2, board.EpicActive)
	other := s.seedTask(e2, "in-ws2", nil, true)

	agentMgr := board.Actor{Kind: board.ActorUser, UserID: testUser,
		Surface: board.SurfaceAgentManager, WorkspaceID: ws1}

	res, err := s.svc.BulkTaskStart(ctx, agentMgr, []string{mine.ShortID, other.ShortID}, "ag-1")
	require.NoError(s.T(), err)
	byRef := bulkByID(s.T(), res)
	assert.Equal(s.T(), board.BulkDone, byRef[mine.ShortID].Status, "the caller-workspace's own row claims")
	foreignItem := byRef[other.ShortID]
	assert.Equal(s.T(), board.BulkFailed, foreignItem.Status)
	require.NotNil(s.T(), foreignItem.Error)
	assert.Equal(s.T(), board.BulkItemNotFound, foreignItem.Error.Code,
		"the workspace binding is the agent perimeter: a foreign-workspace row is the same not_found a miss is")
	assert.NotContains(s.T(), foreignItem.Error.Detail, "did you mean",
		"bare not_found — the bulk envelope never carries did-you-mean suggestions")

	res2, err := s.svc.BulkMoveStage(ctx, agentMgr, []string{other.ShortID},
		board.StageDone, "https://example/p", "ag-2")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.BulkFailed, res2[0].Status)
	assert.Equal(s.T(), board.BulkItemNotFound, res2[0].Error.Code, "the move variant shares the perimeter")
	assert.Equal(s.T(), board.StageBacklog, s.reloadTask(other.ID).Stage, "the foreign-workspace row is untouched")
}

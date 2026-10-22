// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// reclaim_1847_test.go — the #1847 spec item 5 pins: board_reclaim for
// prior claimants. A lapsed-then-alive claimant reclaims WITH CONTEXT —
// attempts unchanged (attempts counts context-loss events; a same-session
// reclaim lost nothing). Identity follows the claim token from the
// worker's charter envelope: the prior token's assign history names the
// context-holding session — no LLM-declared session id anywhere (the
// #1847 trust rule: worker-declared identity is not input).

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// claimAndAssign claims via the engine and returns the claim + the
// stamped assignee (fakeSpawner stamps sess-<shortID> synchronously).
func (s *BoardSuite) claimAndAssign(wsID string) (*board.Claim, string) {
	c := s.claimOrFail(wsID)
	row := s.rawTask(c.TaskID)
	s.Require().NotNil(row.Assignee, "the spawn stamped the assignee")
	return c, *row.Assignee
}

// forceReclaim drives the RC1 path to backlog (stillborn class: no
// liveness row) — the state a returning claimant finds.
func (s *BoardSuite) forceReclaim(taskID string) {
	s.expireLease(taskID)
	res := s.sweep()
	s.Require().Equal(1, res.Reclaims)
	s.Equal(board.StageBacklog, s.rawTask(taskID).Stage)
}

// ACCEPTANCE: "A lapsed-then-alive claimant reclaims with context;
// attempts unchanged."
func (s *BoardSuite) TestKill_ReclaimTask_PriorClaimantReclaimsWithContext() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "reclaim-me", nil, false)

	c, assignee := s.claimAndAssign(ws)
	priorToken := c.ClaimToken
	s.Equal(1, s.rawTask(task.ID).Attempts)
	s.Require().Equal(1, s.countEvents("claim_assign"), "the spawn-stamp wrote the assign history")

	// The claim lapsed and re-pooled while the worker was context-dead;
	// the worker returns (alive, charter in hand).
	s.forceReclaim(task.ID)
	s.clearCooldowns(p, task.ID)

	// A foreign prior token is not in this task's history: refused.
	_, err := s.svc.ReclaimTask(context.Background(),
		s.agent(assignee, ""), task.ID, "", "00000000-0000-0000-0000-000000000000", "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardClaimToken, ge.Code)

	// The real prior token reclaims: fresh claim, same session, same
	// attempts — no context-loss event happened.
	got, err := s.svc.ReclaimTask(context.Background(),
		s.agent(assignee, ""), task.ID, "", priorToken, "")
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage)
	s.Equal(1, row.Attempts, "same-session reclaim does NOT increment attempts — attempts counts context-loss events")
	s.Require().NotNil(row.Assignee)
	s.Equal(assignee, *row.Assignee, "the context-holding session is the assignee again")
	s.Require().NotNil(row.ClaimToken)
	s.NotEqual(priorToken, *row.ClaimToken, "a FRESH token is minted")
	s.Require().NotNil(row.LeaseExp)
	s.True(row.LeaseExp.After(time.Now().UTC()), "the lease is armed")
	var lastFailed bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_attempt_failed FROM tasks WHERE id=$1`, task.ID).Scan(&lastFailed))
	s.False(lastFailed, "the overlay does not mark a context-preserving reclaim")
	s.Require().NotNil(got)
	s.Equal(assignee, got.Assignee, "the result names the re-armed session")
	s.Equal(2, s.countTransitions(task.ID, board.StageInProgress),
		"exactly two in_progress entries: the engine claim + the reclaim")
}

// A LIVE claimant blocks reclaim: the row is in_progress on an
// unexpired lease — someone holds it.
func (s *BoardSuite) TestKill_ReclaimTask_LiveClaimantRefused() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "live", nil, false)

	c, assignee := s.claimAndAssign(ws)
	priorToken := c.ClaimToken

	_, err := s.svc.ReclaimTask(context.Background(), s.agent(assignee, ""), task.ID, "", priorToken, "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardClaimToken, ge.Code, "a live claim is not reclaimable — the holder keeps it")
}

// The appeal-window state (lapsed lease, in_progress, appeal open or
// ignored) is reclaimable by the prior claimant: the row never left
// them in spirit — the reclaim re-arms with a fresh token.
func (s *BoardSuite) TestKill_ReclaimTask_LapsedInProgressReclaims() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "lapsed-ip", nil, false)

	c, assignee := s.claimAndAssign(ws)
	s.expireLease(task.ID)
	// No sweep ran: the row is still in_progress with a lapsed lease.

	_, err := s.svc.ReclaimTask(context.Background(), s.agent(assignee, ""), task.ID, "", c.ClaimToken, "")
	s.Require().NoError(err, "a lapsed in_progress row has no live claimant — the prior claimant reclaims")
	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage)
	s.Equal(1, row.Attempts)
}

// pending_input is the human gate: a prior claimant cannot reclaim
// around it (the pending_input exits are manager-surface-only).
func (s *BoardSuite) TestKill_ReclaimTask_PendingInputRefused() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "pending", nil, false)

	c, assignee := s.claimAndAssign(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent(assignee, c.ClaimToken), task.ID, "",
		board.StagePendingInput, "", "")
	s.Require().NoError(err)

	_, err = s.svc.ReclaimTask(context.Background(), s.agent(assignee, ""), task.ID, "", c.ClaimToken, "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardManagerSurface, ge.Code, "pending_input is a human decision — reclaim cannot bypass it")
}

// Terminal rows are immutable (RC5): done is not reclaimable.
func (s *BoardSuite) TestKill_ReclaimTask_TerminalRefused() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "done-row", nil, true)

	c, assignee := s.claimAndAssign(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent(assignee, c.ClaimToken), task.ID, "",
		board.StageDone, "https://example.com/ev-1", "")
	s.Require().NoError(err)

	_, err = s.svc.ReclaimTask(context.Background(), s.agent(assignee, ""), task.ID, "", c.ClaimToken, "")
	s.Require().Error(err)
	ge, ok := err.(*board.GuardError)
	s.Require().True(ok)
	s.Equal(board.GuardStage, ge.Code)
}

// Paused rows refuse the reclaim mint (#1747: no re-entry while
// paused — the same belt as the bounce).
func (s *BoardSuite) TestKill_ReclaimTask_PausedRefused() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "paused-row", nil, false)

	c, assignee := s.claimAndAssign(ws)
	s.forceReclaim(task.ID)
	s.clearCooldowns(p, task.ID)
	_, err := s.pool.Exec(context.Background(), `UPDATE tasks SET paused=true WHERE id=$1`, task.ID)
	s.Require().NoError(err)

	_, err = s.svc.ReclaimTask(context.Background(), s.agent(assignee, ""), task.ID, "", c.ClaimToken, "")
	s.Require().Error(err)
	ge, ok := err.(*board.GuardError)
	s.Require().True(ok)
	s.Equal(board.GuardPaused, ge.Code)
}

// Escalation records a REASON (spec item 6): the attempts-cap
// escalation's event carries the recorded lapse class — death/no-show
// (no liveness row) vs treadmill artifacts — so the manager can
// service pending_input rows by cause.
func (s *BoardSuite) TestKill_EscalationRecordsReason() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "escalate-me", nil, false)

	// Burn attempts to the cap with stillborn lapses (no liveness rows):
	// each reclaim records lapse_reason='session_unknown'.
	for i := 0; i < 3; i++ {
		c := s.claimOrFail(ws)
		s.Require().Equal(task.ID, c.TaskID)
		s.forceReclaim(task.ID)
		s.clearCooldowns(p, task.ID)
	}

	// The (N+1)th claim attempt escalates instead of granting.
	c, err := s.svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().True(c.Escalated)
	s.Equal(board.StagePendingInput, s.rawTask(task.ID).Stage)

	// The escalate event carries the REASON, distinguishing the class.
	s.Require().GreaterOrEqual(s.countEvents("escalate"), 1)
	var reason string
	err = s.pool.QueryRow(context.Background(),
		`SELECT payload->>'reason' FROM board_events WHERE action='escalate' AND item_id=$1
		 ORDER BY created_at DESC LIMIT 1`, task.ID).Scan(&reason)
	s.Require().NoError(err)
	s.Equal(board.LapseSessionUnknown, reason,
		"the escalation names its cause: no-show/stillborn class, not a treadmill artifact")
}

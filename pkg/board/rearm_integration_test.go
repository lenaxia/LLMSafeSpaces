// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS3 — re-arm: the attempts-exhaustion path back into the pool.
// Escalation (pre-increment attempts >= cap, both the sweep's claimOnce
// and TaskStart) parks a row pending_input/escalated and the engine
// NEVER grants it again. The re-arm action is the explicit, audited
// re-pool: a manager judgment that the task deserves another round —
// attempts reset to 0 (the human-exit semantic: a fresh judgment), the
// pre-reset count preserved in the event, the row back in backlog.

func (s *BoardSuite) TestRearm_EscalatedRowRePoolsWithFreshLedger() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "exhausted", nil, false)

	// attempts at cap -> the engine escalates instead of granting.
	_, execErr := s.pool.Exec(ctx, `UPDATE tasks SET attempts=$2 WHERE id=$1`, t.ID, s.cfg.AttemptsCap)
	s.Require().NoError(execErr)
	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.Require().True(c.Escalated)
	require.Equal(s.T(), board.StagePendingInput, s.reloadTask(t.ID).Stage)

	// The re-arm: orchestrator-class manager surface.
	out, err := s.svc.RearmTask(ctx, s.agentManager(ws, "orchestrator-rearm"), "", t.ShortID, "req-rearm-1")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, out.Stage)
	row := s.rawTask(t.ID)
	require.Equal(s.T(), 0, row.Attempts, "re-pooled with the fresh-judgment ledger")
	require.Equal(s.T(), 1, s.countTransitions(t.ID, board.StageBacklog))
	require.GreaterOrEqual(s.T(), s.countEvents("rearm"), 1)

	// The re-pooled row is claimable again (the pool took it back).
	c2, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c2.Task)
	s.Require().False(c2.Escalated)
	require.Equal(s.T(), t.ID, c2.TaskID)
	require.Equal(s.T(), 1, s.rawTask(t.ID).Attempts, "the fresh round counts from 1")
}

func (s *BoardSuite) TestRearm_ReplayReturnsOriginalOutcome() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "rearm-replay", nil, false)
	_, execErr := s.pool.Exec(ctx,
		`UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='escalated', attempts=$2 WHERE id=$1`,
		t.ID, s.cfg.AttemptsCap)
	s.Require().NoError(execErr)

	out1, err := s.svc.RearmTask(ctx, s.agentManager(ws, "orchestrator-rearm"), "", t.ShortID, "req-rearm-replay")
	s.Require().NoError(err)
	// Claim the row away from backlog so a NON-idempotent second call
	// would have nothing to re-arm (the guard would refuse).
	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)

	out2, err := s.svc.RearmTask(ctx, s.agentManager(ws, "orchestrator-rearm"), "", t.ShortID, "req-rearm-replay")
	s.Require().NoError(err, "a replayed requestId returns the original outcome, never an error")
	require.Equal(s.T(), out1.Stage, out2.Stage)
	require.Equal(s.T(), 1, s.countEvents("rearm"), "exactly one rearm event")
}

func (s *BoardSuite) TestRearm_OnlyEscalatedPendingRows() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// A worker-origin pending_input row: NOT re-armable (its exits are
	// the pending_input transitions, not a re-pool judgment).
	workerRow := s.seedTask(e, "worker-pending", nil, false)
	c := s.claimOrFail(ws)
	s.Require().Equal(workerRow.ID, c.TaskID)
	_, err := s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StagePendingInput, "", "")
	s.Require().NoError(err)
	_, err = s.svc.RearmTask(ctx, s.agentManager(ws, "orchestrator-rearm"), "", workerRow.ShortID, "")
	require.Error(s.T(), err, "re-arm targets attempts-exhausted (escalated) rows only")
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardStage, guard.Code)

	// A backlog row: not re-armable either.
	plainRow := s.seedTask(e, "plain-backlog", nil, false)
	_, err = s.svc.RearmTask(ctx, s.agentManager(ws, "orchestrator-rearm"), "", plainRow.ShortID, "")
	require.Error(s.T(), err)
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardStage, guard.Code)
}

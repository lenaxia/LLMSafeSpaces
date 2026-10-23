// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS2 — pending_input exits for orchestrator-class agent
// sessions (the tsk-3do6n458 incident: an escalated task froze a whole
// dependency chain because neither the lane worker NOR the
// orchestrator-class session could exit it — both ride the agent
// surface, and the pending_input exits were manager-surface-only with
// NO manager move path on the in-pod seam).
//
// The fix: a distinct agent-manager surface for the pod seam's manager
// projection (SurfaceAgentManager) — the human-only rule HOLDS for
// ordinary workers (SurfaceAgent stays denied), and the manager move
// is the orchestrator-class session's path, audited with its session
// identity.

// agentManager is the pod manager seam's actor: the resolved owner
// (R7), the manager-grade agent surface, the workspace binding, and
// the orchestrator-class session's identity (audit attribution).
func (s *BoardSuite) agentManager(wsID, sessionID string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: testUser,
		Surface: board.SurfaceAgentManager, WorkspaceID: wsID, SessionID: sessionID}
}

func (s *BoardSuite) TestPendingExit_WorkerStillDenied_OrchestratorGradeAllowed() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "frozen", nil, false)

	// Walk to a worker-origin pending_input with a recorded assignee
	// (the re-claim branch's precondition).
	c := s.claimOrFail(ws)
	s.Require().Equal(t.ID, c.TaskID)
	_, err := s.svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StagePendingInput, "", "")
	s.Require().NoError(err)

	// The ordinary worker (SurfaceAgent, live token) stays DENIED —
	// the human-only rule for workers is unchanged.
	_, err = s.svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInProgress, "", "")
	require.Error(s.T(), err)
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardManagerSurface, guard.Code)

	// The orchestrator-class session (agent-manager surface) exits.
	out, err := s.svc.MoveStage(context.Background(), s.agentManager(ws, "orchestrator-session-1"), "", c.ShortID, board.StageInProgress, "", "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageInProgress, out.Stage)
	row := s.rawTask(t.ID)
	require.NotEmpty(s.T(), row.ClaimToken, "the exit re-arms a fresh claim (the existing reclaim branch)")
	require.Equal(s.T(), 0, row.Attempts, "the exit is the ONE attempts reset")
}

func (s *BoardSuite) TestPendingExit_AgentManagerEscalatedRowLandsBacklogReset() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "exhausted", nil, false)

	// attempts at cap -> the engine escalates instead of granting.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET attempts=$2 WHERE id=$1`, t.ID, s.cfg.AttemptsCap)
	s.Require().NoError(err)
	c, err := s.svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.Require().True(c.Escalated, "the exhausted row escalates")
	require.Equal(s.T(), board.StagePendingInput, s.reloadTask(t.ID).Stage)

	// No recorded assignee: the manager exit re-queues (the backlog
	// branch of the enumerated human exit).
	out, err := s.svc.MoveStage(context.Background(), s.agentManager(ws, "orchestrator-session-2"), "", t.ShortID, board.StageBacklog, "", "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, out.Stage)
	require.Equal(s.T(), 0, s.rawTask(t.ID).Attempts, "re-pooled with a fresh judgment ledger")
}

func (s *BoardSuite) TestPendingExit_AgentManagerWorkspaceBindingHolds() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "foreign", nil, false)
	_, execErr := s.pool.Exec(context.Background(),
		`UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='escalated' WHERE id=$1`, t.ID)
	s.Require().NoError(execErr)

	// An agent-manager actor bound to a DIFFERENT workspace must not
	// reach the row (403 workspace_binding — the structural scope).
	foreign := board.Actor{Kind: board.ActorUser, UserID: testUser,
		Surface: board.SurfaceAgentManager, WorkspaceID: "00000000-0000-0000-0000-000000000000", SessionID: "rogue"}
	_, err := s.svc.MoveStage(context.Background(), foreign, "", t.ShortID, board.StageBacklog, "", "")
	require.Error(s.T(), err)
	var guard *board.GuardError
	require.ErrorAs(s.T(), err, &guard)
	require.Equal(s.T(), board.GuardWorkspaceBinding, guard.Code)
}

// The surface vocabulary pin: the manager-grade agent surface is a
// DISTINCT audit entry (tonight's freeze diagnosis needed exactly this
// distinction in the actor trail).
func (s *BoardSuite) TestPendingExit_SurfaceVocabulary() {
	require.Equal(s.T(), "agent_manager", board.SurfaceAgentManager)
}

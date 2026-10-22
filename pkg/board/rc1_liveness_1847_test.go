// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// rc1_liveness_1847_test.go — the #1847 RC1 rework flow pins
// (integration; the classification rules are unit-pinned in
// liveness_unit_test.go). These encode the issue's acceptance bar:
//
//   - an open turn holds the lease across a long silent tool call;
//   - a stillborn spawn is never renewed;
//   - a wedged turn releases within grace + ONE appeal ping;
//   - a lapsed-then-alive claimant is re-armed (attempts unchanged).

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// seedLiveness writes one session's platform-observed liveness row
// (the agentd push projection; the #1847 observability source).
func (s *BoardSuite) seedLiveness(wsID, sessionID string, busy, started bool, lastActivity, pushedAt time.Time) {
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO board_session_liveness (workspace_id, session_id, busy, started, last_activity_at, pushed_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace_id, session_id) DO UPDATE
			SET busy=$3, started=$4, last_activity_at=$5, pushed_at=$6`,
		wsID, sessionID, busy, started, lastActivity, pushedAt)
	s.Require().NoError(err)
}

// taskLapse reads the row's appeal/lapse columns.
func (s *BoardSuite) taskLapse(taskID string) (appealUntil *time.Time, reason *string) {
	err := s.pool.QueryRow(context.Background(),
		`SELECT appeal_until, lapse_reason FROM tasks WHERE id=$1`, taskID).Scan(&appealUntil, &reason)
	s.Require().NoError(err)
	return
}

// ACCEPTANCE: "A worker inside a single 20-min silent bash call holds
// its lease (open turn)." The lease expires mid-call; the sweep sees
// busy (open turn, fresh pushes) and re-arms instead of reclaiming.
func (s *BoardSuite) TestKill_RC1Liveness_OpenTurnHoldsExpiredLease() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "silent-bash", nil, false)

	_ = s.claimOrFail(ws)
	row := s.rawTask(task.ID)
	s.Require().NotNil(row.Assignee, "spawn stamped the assignee")
	now := time.Now().UTC()
	freshPush := now.Add(-s.cfg.LivenessStaleBound / 2)

	// Mid-20-min-bash: busy, pushes fresh, last MESSAGE 20m ago.
	s.seedLiveness(ws, *row.Assignee, true, true, now.Add(-20*time.Minute), freshPush)
	s.expireLease(task.ID)

	res := s.sweep()
	s.Equal(0, res.Reclaims, "an open turn is alive — nothing reclaims")

	after := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, after.Stage, "the lease held across the silent call")
	s.Require().NotNil(after.LeaseExp)
	s.True(after.LeaseExp.After(time.Now().UTC()), "lease re-armed by platform liveness")
	s.Equal(1, after.Attempts, "attempts untouched — nothing was lost")
	s.Equal(0, s.countTransitions(task.ID, board.StageBacklog), "no reclaim transition")
}

// ACCEPTANCE: "A stillborn spawn is never renewed." No liveness row
// (the spawn never created a working session) → the lapsed claim
// reclaims; nothing renews the corpse.
func (s *BoardSuite) TestKill_RC1Liveness_StillbornReclaimed() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stillborn", nil, false)

	_ = s.claimOrFail(ws)
	s.expireLease(task.ID)
	// NO liveness row — the spawn's session never pushed.

	res := s.sweep()
	s.Equal(1, res.Reclaims, "a stillborn spawn's claim reclaims")

	row := s.rawTask(task.ID)
	s.Equal(board.StageBacklog, row.Stage)
	_, reason := s.taskLapse(task.ID)
	s.Require().NotNil(reason)
	s.Equal(board.LapseSessionUnknown, *reason, "the lapse reason records the stillborn class")
	// And no appeal ping ever fired for it.
	s.Equal(0, len(s.messenger.calls))
}

// ACCEPTANCE: "A wedged turn (tool timeout) releases within grace + one
// appeal ping." Started, idle, silent beyond grace: sweep #1 opens ONE
// appeal (ping at the turn boundary, window on the row, lease extended
// to the window) — NOT a reclaim; sweep #2 after the window reclaims
// with the reason recorded.
func (s *BoardSuite) TestKill_RC1Liveness_WedgedTurnAppealsThenReclaims() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "wedged", nil, false)

	_ = s.claimOrFail(ws)
	row := s.rawTask(task.ID)
	now := time.Now().UTC()
	grace := s.cfg.LeaseGrace
	freshPush := now.Add(-s.cfg.LivenessStaleBound / 2)

	// The wedge: turn ended (idle), activity stopped past the grace,
	// pushes still fresh (the pod is alive — the session is wedged).
	s.seedLiveness(ws, *row.Assignee, false, true, now.Add(-grace-time.Second), freshPush)
	s.expireLease(task.ID)

	res := s.sweep()
	s.Equal(0, res.Reclaims, "the appeal is pre-reclaim: sweep #1 reclaims nothing")

	after := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, after.Stage, "the appeal window holds the row")
	s.Equal(1, after.Attempts, "no attempt consumed by the appeal itself")
	appealUntil, reason := s.taskLapse(task.ID)
	s.Require().NotNil(appealUntil, "the window is recorded")
	s.True(appealUntil.After(time.Now().UTC()), "the window is in the future")
	s.Require().NotNil(reason)
	s.Equal(board.LapseSilentBeyondGrace, *reason)
	s.Equal(1, len(s.messenger.calls), "exactly ONE appeal ping")
	s.Contains(s.messenger.calls[0], board.DirectiveLeaseAppeal)

	// Re-pinging inside the window must not happen: age the lease but
	// keep the window explicitly open (deterministic, not a race with
	// the 500ms harness window) — the sweep must skip the open appeal.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 millisecond',
			appeal_until = now() + interval '10 seconds' WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	_ = s.sweep()
	s.Equal(1, len(s.messenger.calls), "no second ping inside the window")
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the open appeal holds the row")

	// The window lapses; sweep #2 reclaims.
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET appeal_until = now() - interval '1 millisecond',
			lease_expires_at = now() - interval '1 millisecond' WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	res = s.sweep()
	s.Equal(1, res.Reclaims)

	final := s.rawTask(task.ID)
	s.Equal(board.StageBacklog, final.Stage)
	_, reason = s.taskLapse(task.ID)
	s.Require().NotNil(reason)
	s.Equal(board.LapseAppealIgnored, *reason, "the reason tells the escalation what happened")
}

// ACCEPTANCE (recovery arm): "A lapsed-then-alive claimant reclaims
// with context; attempts unchanged" — the platform path: during the
// appeal window the session wakes (busy again), and the sweep RE-ARMS
// the original claim rather than reclaiming.
func (s *BoardSuite) TestKill_RC1Liveness_AppealWindowRecoveryReArms() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "revival", nil, false)

	_ = s.claimOrFail(ws)
	row := s.rawTask(task.ID)
	now := time.Now().UTC()
	grace := s.cfg.LeaseGrace
	freshPush := now.Add(-s.cfg.LivenessStaleBound / 2)

	// Wedged → appeal opens.
	s.seedLiveness(ws, *row.Assignee, false, true, now.Add(-grace-time.Second), freshPush)
	s.expireLease(task.ID)
	_ = s.sweep()
	mid := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, mid.Stage)
	s.Equal(1, len(s.messenger.calls))

	// The worker answers with work: busy again, activity fresh. Lease
	// still inside the window (not expired) — age it so the sweep sees
	// the row and must judge liveness again.
	s.seedLiveness(ws, *row.Assignee, true, true, now, now.Add(-s.cfg.LivenessStaleBound/4))
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 millisecond' WHERE id=$1`, task.ID)
	s.Require().NoError(err)

	res := s.sweep()
	s.Equal(0, res.Reclaims, "the revival holds — no reclaim")

	after := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, after.Stage, "the ORIGINAL claim continues — context kept")
	s.Equal(1, after.Attempts, "attempts unchanged — no context-loss event")
	s.Require().NotNil(after.ClaimToken)
	s.Equal(*mid.ClaimToken, *after.ClaimToken, "the same token: the claim never left the worker")
	appealUntil, _ := s.taskLapse(task.ID)
	s.Nil(appealUntil, "the appeal cleared on re-arm")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// opaque_renews_2042_integration_test.go — the second weak channel's
// bound (issue #2042, the 2026-11-15 #2040 incident's insurance cap).
//
// The incident: agentd's session-state reconcile published synthesized
// SESSION_STATUS settles for sessions that emitted nothing; the API hub
// stamped them as movement; the ladder's renewal arm renewed the halted
// workers' leases forever (three slots held 1–7 hours; one worker
// orbited nudge→heartbeat→renew past 03:45 with zero work since 23:21).
// The allowlist (API side) reclasses those frames as opaque movement;
// THESE pins bound what the engine does with it:
//
//   - within boardOpaqueRenewCap consecutive opaque-only renewals, the
//     claim renews (the counter counts) — RenewSourceOpaque;
//   - at the cap the row falls to the quiet arms as if unmoved
//     (nudge fires; the counter stays; a follow-up silent lapse wedges);
//   - any worker-activity renewal resets the counter (consecutive
//     semantics — a genuinely active worker never approaches the cap).

package board_test

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// opaqueRenewCapForTest mirrors pkg/board's boardOpaqueRenewCap (3,
// unexported — this is an external-package suite). The literal is
// drift-pinned by PIN 2042-B: if the cap moves, that pin's parked-count
// arithmetic must move with it.
const opaqueRenewCapForTest = 3

func (s *BoardSuite) opaqueRenews(taskID string) int {
	var n int32
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT opaque_renews FROM tasks WHERE id=$1`, taskID).Scan(&n))
	return int(n)
}

// PIN 2042-A: opaque-only movement within the cap renews and counts.
func (s *BoardSuite) TestPin_2042A_OpaqueMovementRenewsWithinCap() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "opaque-in-cap", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	// The incident shape: the session emitted nothing worker-side; the
	// hub reclassed a settle as opaque movement.
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedOpaque: true,
	})
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "opaque movement within the cap renews the claim")
	s.leaseFuture(task.ID)
	s.Equal(0, nudger.count(), "no sentinel — the row was renewed, not nudged")
	s.Equal(1, s.opaqueRenews(task.ID), "the opaque counter counts the consecutive opaque-only renewal")
}

// PIN 2042-B: at the cap, opaque movement is exhausted — the row takes
// the quiet arms (nudge), and a silent follow-up lapse wedges it. The
// orbit closes.
func (s *BoardSuite) TestPin_2042B_OpaqueMovementAtCapFallsToQuietArms() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "opaque-at-cap", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedOpaque: true,
	})
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	// Park the row AT the cap (the prior episodes' residue).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET opaque_renews=$1 WHERE id=$2`, opaqueRenewCapForTest, task.ID)
	s.Require().NoError(err)

	// Lapse: opaque movement is exhausted → quiet arms, not renewal.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the cap does NOT reclaim — the quiet arms answer first")
	s.Equal(1, nudger.count(), "the sentinel arms instead of a renewal")
	s.Equal(opaqueRenewCapForTest, s.opaqueRenews(task.ID), "an exhausted counter does not increment")

	// The full silent window past the sentinel. Production truth: the
	// hub HOLDS the send-mark and saw no worker activity after it (the
	// only post-sentinel frame was opaque) — the observation answers
	// the ordinal question, and MovedOpaque at the cap cannot rescue
	// the row through the quiet arms.
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedOpaque: true,
		NudgeMarkKnown: true, EventsAfterNudge: false,
	})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row = s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "opaque-true-forever cannot hold the lease — the orbit closes")
	s.True(rowIsMarkedFailed(s, task.ID))
	s.Equal(1, s.countEvents("wedge"))
}

// PIN 2042-C: consecutive semantics — a worker-activity renewal resets
// the opaque counter (a genuinely active worker never approaches the
// cap, whatever its management traffic volume).
func (s *BoardSuite) TestPin_2042C_WorkerMovementResetsOpaqueCounter() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "opaque-reset", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedSinceLastSweep: true, MovedOpaque: true,
	})
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET opaque_renews=$1 WHERE id=$2`, opaqueRenewCapForTest, task.ID)
	s.Require().NoError(err)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "worker movement renews at any opaque count")
	s.leaseFuture(task.ID)
	s.Equal(0, s.opaqueRenews(task.ID), "a worker-activity renewal resets the consecutive opaque counter")
	s.Equal(0, nudger.count())
}

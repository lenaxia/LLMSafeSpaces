// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// settle_no_authority_2042_integration_test.go — the #2040 incident's
// engine pins, round-1 review shape (issue #2042): OPAQUE movement
// (management-frame deltas — settles, reseeds, lease-state transitions,
// input materializations, unknown types) carries ZERO renewal authority.
// The reviewer's first-settle shape IS the regression shape: quiet
// session + first settle + sweep → nudge fires, NOT renewal. The capped
// intermediate shipped in the first push was rejected: it preserved the
// incident for ~3 lease windows per epoch and its counter mutually
// re-armed with the busy cap (the exact orbit this lane closes).

package board_test

import (
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// opaqueOnly is the incident observation: the session emitted nothing
// worker-side; the only delta is a management-frame stamp.
func opaqueOnly() board.AssigneeObservation {
	return board.AssigneeObservation{Observed: true, Exists: true, MovedOpaque: true}
}

// PIN 2042-A (the AC1 shape): quiet + FIRST settle + sweep → nudge, not
// renewal. Zero authority means zero preserved incident windows.
func (s *BoardSuite) TestPin_2042A_FirstSettleArmsQuietArms() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "settle-no-authority", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(c.Task.AssigneeSession, opaqueOnly())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the first settle does not reclaim — the quiet arms answer first")
	s.Equal(1, nudger.count(), "a settle on a quiet session arms the sentinel, NEVER a renewal (the incident shape, closed on the first evidence)")
	nid, nudgedAt := s.nudgeState(task.ID)
	s.NotEmpty(nid)
	s.NotNil(nudgedAt)
}

// PIN 2042-B: the settle-only orbit closes — with the ordinal question
// answered (the hub holds the mark; the only post-sentinel frame was
// opaque), the silent window past the sentinel lands the wedge verdict.
func (s *BoardSuite) TestPin_2042B_SettleOnlyOrbitCloses() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "settle-orbit-closes", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(c.Task.AssigneeSession, opaqueOnly())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Require().Equal(1, nudger.count())

	// The hub holds the sentinel's mark; the only post-sentinel frame
	// was opaque — production truth after the #2042 allowlist.
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedOpaque: true,
		NudgeMarkKnown: true, EventsAfterNudge: false,
	})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "settle-true-forever cannot hold the lease — the orbit closes on the first silent window")
	s.True(rowIsMarkedFailed(s, task.ID))
	s.Equal(1, s.countEvents("wedge"))
}

// PIN 2042-C: the healthy path is unchanged — worker movement renews
// (and an incidental settle alongside it neither helps nor harms).
func (s *BoardSuite) TestPin_2042C_WorkerMovementStillRenews() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "worker-movement-renews", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(c.Task.AssigneeSession, board.AssigneeObservation{
		Observed: true, Exists: true, MovedSinceLastSweep: true, MovedOpaque: true,
	})
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "worker movement renews")
	s.leaseFuture(task.ID)
	s.Equal(0, nudger.count())
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// heartbeat_lapse_2043_integration_test.go — the #2040 remediation's
// two-clock pins (issue #2043): lease RENEWAL is split from worker
// PROGRESS (tasks.last_renewed_at vs tasks.last_progress_at), and the
// heartbeat-lapse arm overrides all observation evidence when the
// worker's progress clock is stale past HeartbeatLapseThreshold.
//
// The incident (#2040): the ladder wrote now() into last_progress_at
// on every renewal, so a renewing-but-worker-silent row was
// indistinguishable from a progressing one — detection deferred by a
// full lease window per renewal, forever. These pins hold the split
// and the arm that closes that orbit:
//
//   - 2043-A: a ladder renewal writes last_renewed_at and NEVER
//     advances last_progress_at (born-red: pre-fix it does).
//   - 2043-B: a lapsed row (progress older than the threshold, or
//     nil) cannot be renewed by ANY observation evidence —
//     movement, busy, post-nudge reaction all fall to the quiet
//     arms (nudge → heartbeat-or-wedge).
//   - 2043-C: worker progress (the heartbeat response's
//     worker-declared stamp) resets the arm — movement renews again.
//   - 2043-D: nil last_progress_at is lapse-armed by default (no
//     evidence of worker progress = no movement veto).
//   - 2043-E: the arm itself touches neither attempts nor
//     failure_count (escalation belongs to the wedge verdict).
//
// Boundary with #2044 (stated here because the pins enforce it): the
// heartbeat RESPONSE writes the worker-DECLARED observedProgressAt
// into last_progress_at — this story trusts that column exactly as
// far as last_heartbeat_at semantics already do; the no-advance
// budget for declared-without-advance stamps is #2044's jurisdiction.

package board_test

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// ageProgress is the progress-clock seam: rewinds last_progress_at
// past the default 90m HeartbeatLapseThreshold (the expireLease house
// pattern — the ladder's lapse comparison is SQL now()-based, so a
// rewound watermark IS a stale one).
func (s *BoardSuite) ageProgress(taskID string, d time.Duration) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET last_progress_at = now() - make_interval(secs=>$2::float8) WHERE id=$1`,
		taskID, d.Seconds())
	s.Require().NoError(err)
}

// nilProgress removes the progress anchor entirely (the legacy/NULL
// shape — lapse-armed by default).
func (s *BoardSuite) nilProgress(taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET last_progress_at = NULL WHERE id=$1`, taskID)
	s.Require().NoError(err)
}

// lastRenewedAt reads the renewal clock (NULL = no ladder renewal has
// extended this row since the column's epoch).
func (s *BoardSuite) lastRenewedAt(taskID string) *time.Time {
	var t *time.Time
	err := s.pool.QueryRow(context.Background(),
		`SELECT last_renewed_at FROM tasks WHERE id=$1`, taskID).Scan(&t)
	s.Require().NoError(err)
	return t
}

// PIN 2043-A (born-red, AC1): the ladder renewal writes the renewal
// clock, not the progress clock. Pre-fix, the renewal's
// last_progress_at=now() IS the incident's lie — this pin goes red the
// moment that write returns.
func (s *BoardSuite) TestPin_2043A_RenewalWritesRenewedClockNotProgress() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "renewal-split", nil, false)

	s.claimOrFail(ws)
	// The claim mint anchors the progress clock (worker-anchored by
	// design — R1 keeps the claim write).
	claimed := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(claimed, "the claim mint anchors last_progress_at")

	lives := &fakeLiveness{}
	lives.set(s.reloadTask(task.ID).AssigneeSession, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.leaseFuture(task.ID)
	s.Equal(0, nudger.count())
	after := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(after)
	s.Equal(*claimed, *after,
		"a ladder renewal is not worker progress — the progress clock never moves (the #2040 lie, removed)")
	s.NotNil(s.lastRenewedAt(task.ID),
		"the renewal clock records the platform's evidence-based extension")
}

// PIN 2043-B (AC2): a lapsed row cannot be renewed by ANY observation
// evidence — MovedSinceLastSweep, Busy, and EventsAfterNudge (the
// would-be renewal rungs) all fall through to the quiet arms: the
// nudge lands, and the unanswered sentinel closes the orbit at the
// machine verdict.
func (s *BoardSuite) TestPin_2043B_LapseOverridesAllRenewalEvidence() {
	shapes := []struct {
		name string
		obs  board.AssigneeObservation
	}{
		{"movement cannot veto", board.AssigneeObservation{Observed: true, Exists: true, MovedSinceLastSweep: true}},
		{"busy cannot veto", board.AssigneeObservation{Observed: true, Exists: true, Busy: true}},
		{"post-nudge reaction cannot veto", board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true, EventsAfterNudge: true}},
	}
	for _, shape := range shapes {
		s.Run(shape.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "lapse-"+shape.name, nil, false)

			c := s.claimOrFail(ws)
			assignee := s.reloadTask(task.ID).AssigneeSession
			s.Require().NotEmpty(assignee)
			lives := &fakeLiveness{}
			lives.set(assignee, shape.obs)
			nudger := &fakeNudger{}
			recon := s.pinRecon(lives, nudger.Send)

			s.ageProgress(task.ID, 2*time.Hour) // past the 90m default threshold
			s.expireLease(task.ID)
			lapseBefore := testutil.ToFloat64(board.LadderHeartbeatLapseCounter())
			s.rc1Sweep(recon)

			row := s.rawTask(task.ID)
			s.Equal(board.StageInProgress, row.Stage, "the arm does not fail the row — the quiet arms answer")
			s.Require().NotNil(row.ClaimToken)
			s.Equal(c.ClaimToken, *row.ClaimToken, "the claim survives the arm (no attempt churn)")
			s.Equal(1, nudger.count(), "fresh worker-activity frames cannot renew a lapsed row — the sentinel lands")
			s.Require().Equal(1, len(nudger.sent))
			s.Equal(board.NudgeReasonHeartbeatLapse, nudger.sent[0].Payload.Reason,
				"the sentinel carries the arm's attribution (the observable arm)")
			s.Equal(lapseBefore+1, testutil.ToFloat64(board.LadderHeartbeatLapseCounter()),
				"the suppressed renewal is countable — the arm's always-on signature")
			s.leaseFuture(task.ID)
			wm := s.rawTaskWatermark(task.ID)
			s.Require().NotNil(wm, "the nudge arm never touches the progress clock either")

			// The orbit closes: sentinel positioned, no post-sentinel
			// events, no heartbeat answer — the machine verdict (the
			// escalation that WAS the incident's deferred detection).
			lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
			s.expireLease(task.ID)
			s.rc1Sweep(recon)
			row = s.rawTask(task.ID)
			s.Equal(board.StageFailed, row.Stage, "lapse + unanswered sentinel lands the wedge verdict")
			s.True(rowIsMarkedFailed(s, task.ID))
			s.Equal(1, s.countEventsFor("wedge", task.ID), "the machine verdict audits once, for this row")
		})
	}
}

// PIN 2043-C (AC3): worker progress resets the arm. The heartbeat
// RESPONSE writes the worker-declared observedProgressAt into
// last_progress_at (the worker-anchored clock — R1 keeps that write;
// the no-advance budget for it is #2044's boundary) and the next
// expiry's movement evidence renews WITHOUT a fresh nudge.
func (s *BoardSuite) TestPin_2043C_WorkerProgressResetsArm() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "lapse-reset", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.ageProgress(task.ID, 2*time.Hour)
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Require().Equal(1, nudger.count(), "the lapsed row took the sentinel")

	// The worker answers: response renews, and the DECLARED stamp
	// anchors the progress clock (worker-anchored, clamped server-side).
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
		time.Now().UTC())
	s.Require().NoError(err)
	s.True(res.Extended, "the response channel renews at the lapse rung too — it IS the reset")
	responded := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(responded, "the heartbeat response anchors last_progress_at (worker-declared)")

	// Un-lapsed: movement evidence renews again — no fresh nudge, and
	// the renewal does not advance the worker-anchored clock.
	lives.set(assignee, liveMoving())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "worker progress reset the arm — movement renews")
	s.leaseFuture(task.ID)
	s.Equal(1, nudger.count(), "no fresh sentinel for an un-lapsed row")
	after := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(after)
	s.Equal(*responded, *after, "the post-reset renewal still never advances the progress clock")
}

// PIN 2043-D (AC4): nil last_progress_at is lapse-armed BY DEFAULT —
// no evidence of worker progress means no movement veto (the honest
// direction), pinned so a future "nil = exempt" regression goes red.
func (s *BoardSuite) TestPin_2043D_NilProgressIsLapsedByDefault() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "nil-progress", nil, false)

	s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(s.reloadTask(task.ID).AssigneeSession, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.nilProgress(task.ID)
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.Equal(1, nudger.count(), "nil progress = no worker evidence = the quiet arms (never a movement renewal)")
	s.Nil(s.rawTaskWatermark(task.ID), "nothing wrote a progress stamp — the arm nudged instead")
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
	s.leaseFuture(task.ID)
}

// PIN 2043-E (AC5): the arm itself touches neither attempts nor
// failure_count — escalation belongs to the wedge verdict (and, for
// declared-without-advance stamps, #2044's orbit budget). A nudge is
// a question, not an accounting event.
func (s *BoardSuite) TestPin_2043E_ArmTouchesNoAccounting() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "lapse-no-accounting", nil, false)

	s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(s.reloadTask(task.ID).AssigneeSession, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.ageProgress(task.ID, 2*time.Hour)
	s.expireLease(task.ID)
	before := s.rawTask(task.ID)
	s.rc1Sweep(recon)

	after := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, after.Stage)
	s.Equal(before.Attempts, after.Attempts, "the arm is not an attempt")
	s.Equal(before.ClaimIncr, after.ClaimIncr, "the arm does not re-arm (or reset) the claim increment")
	overlay := struct {
		lastFail    bool
		cooldown    bool
		failures    int
		lastFailure bool
	}{}
	err := s.pool.QueryRow(context.Background(),
		`SELECT last_attempt_failed, cooldown_until IS NOT NULL, failure_count, last_failure_at IS NOT NULL
		 FROM tasks WHERE id=$1`, task.ID).
		Scan(&overlay.lastFail, &overlay.cooldown, &overlay.failures, &overlay.lastFailure)
	s.Require().NoError(err)
	s.Equal(0, overlay.failures, "the arm is not a failure (escalation belongs to the wedge verdict)")
	s.False(overlay.lastFailure)
	s.False(overlay.lastFail, "no failure overlay from the arm itself")
	s.False(overlay.cooldown, "no backoff parking from the arm itself")
}

// PIN 2043-F: a NON-lapsed quiet row's sentinel carries NO lapse
// attribution (the ordinary quiet path — the reason field is the
// arm's, not the nudge rung's).
func (s *BoardSuite) TestPin_2043F_OrdinaryNudgeCarriesNoLapseReason() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "ordinary-nudge", nil, false)

	s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(s.reloadTask(task.ID).AssigneeSession, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	lapseBefore := testutil.ToFloat64(board.LadderHeartbeatLapseCounter())
	s.rc1Sweep(recon)

	s.Require().Equal(1, nudger.count())
	s.Empty(nudger.sent[0].Payload.Reason, "the ordinary quiet nudge is unattributed — only the lapse arm stamps its reason")
	s.Equal(lapseBefore, testutil.ToFloat64(board.LadderHeartbeatLapseCounter()),
		"no suppressed renewal, no lapse count — the ordinary quiet path arms nothing")
}

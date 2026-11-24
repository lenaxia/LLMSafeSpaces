// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// heartbeat_orbit_2044_integration_test.go — the #2040 remediation's
// third orbit (issue #2044): the heartbeat-response budget. #2043's
// two-clock split bounds EVIDENCE-based renewal (the lapse arm); #2042
// bounds opaque frames; this lane bounds RESPONSE-based renewal — the
// compliant-but-stuck worker that answers every sentinel with
// board_heartbeat seconds after it lands, declares progress no newer
// than the claim mint, and never advances a thing (the live verified
// shape: tsk-polished-keystone-waits, 2026-11-15 — nudged 02:14:29,
// heartbeat 19s later, nudged 02:59:59, heartbeat 4s later, zero work
// since 23:21, attempts still 1).
//
// The budget (NoAdvanceHeartbeatCap, default 3): K consecutive
// heartbeat RESPONSES — declared or arrival-only, motionless or not —
// exhaust presence-only renewal. The handler COUNTS, it never CREDITS:
// every response increments no_advance_renews and the ONLY motion
// reset is the observer's (a movement/busy renewal in the reconciler
// zeroes the budget; the claim/persona mints reset it). The v0.44.19
// defect let a responder that stamped wall-clock now() credit motion
// at every write (the `observedProgressAt > stage_entered_at` predicate
// was vacuously true), pinning the counter at 0 so the cap never
// tripped — the live proof: probe task 615a8cf1 ran ~40 renewal cycles
// over 2.5 days, wedged only once the responder went silent (compare
// tsk-polished-keystone-waits, 2026-11-15, the declared-stale shape:
// nudged 02:14:29, heartbeat 19s later, attempts still 1). The K+1th
// response DECLINES (arrival recorded, cycle counted, sentinel kept
// armed, lease not extended), and the next lapse demotes EVERY renewal
// arm for that lapse — hbAfterNudge, MovedSinceLastSweep, Busy, and
// EventsAfterNudge all read false, because the heartbeat's own tool
// call emits worker-activity PART frames (#2042's allowlist) that are
// PRESENCE, not reaction. The row proceeds to the existing wedge
// verdict (StageFailed via the reconciler path).
//
// Pins (born-red where the fix changes behavior — the orbit renewed
// forever for a declarer pre-fix):
//
//   - 2044-A: the full orbit cycle sequence with a STALE-DECLARING
//     responder — K responses renew (counter 1..K), the K+1th declines
//     (slot kept, lease frozen), the next sweep lands the wedge
//     verdict; the wedge event's payload + evidence carry the budget
//     facts. (Green both sides — the stale stamp never crossed the mint.)
//   - 2044-B: at cap, the heartbeat's own PART frames cannot rescue the
//     row through ANY renewal arm — MovedSinceLastSweep,
//     EventsAfterNudge, and Busy all read false for the lapse.
//   - 2044-C (BORN-RED, the headline): a responder that DECLARES now()
//     every cycle — the exact v0.44.19 shape — is now bounded: K
//     cycles renew (counter climbs 1..K), the K+1th declines, the next
//     lapse wedges. Pre-fix this renewed forever (the motion-credit
//     bug), so this pin is red against pre-fix main.
//   - 2044-D (BORN-RED half a2): motionless heartbeats COUNT the budget
//     (the handler never resets it), but the OBSERVER legs still renew
//     a genuinely active worker — a movement/busy renewal in the
//     reconciler resets the budget (the mid-turn/streaming shape, no
//     regression), and the ladder's own heartbeat belt arm counts
//     motionless belt renewals the same way and skips at cap.
//   - 2044-E (BORN-RED): a committed nudge answered 4s later takes the
//     guarded RESPONSE arm — slot cleared, lease extended — but its
//     fresh-declared stamp credits NOTHING: the cycle still COUNTS
//     (counter 1), pre-fix it reset to 0.
//   - 2044-F: reviewer persona in_review heartbeats never trip the
//     budget (no nudge ever arms on an in_review row — structural).
package board_test

import (
	"context"
	"encoding/json"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// noAdvanceCount reads the row's no-advance budget counter.
func (s *BoardSuite) noAdvanceCount(taskID string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT no_advance_renews FROM tasks WHERE id=$1`, taskID).Scan(&n))
	return n
}

// orbitMint reads the claim epoch's mint (stage_entered_at) — the
// detector's comparison anchor.
func (s *BoardSuite) orbitMint(taskID string) time.Time {
	var t time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT stage_entered_at FROM tasks WHERE id=$1`, taskID).Scan(&t))
	return t
}

// staleDeclare returns a declared watermark one minute OLDER than the
// claim mint — the honest orbiting worker's stamp ("my last observed
// progress", never anything this epoch).
func (s *BoardSuite) staleDeclare(taskID string) time.Time {
	return s.orbitMint(taskID).Add(-time.Minute)
}

// orbitCycle runs one nudge→response cycle against the REAL heartbeat
// handler: expire, sweep (quiet — the sentinel arms), respond with the
// given declared watermark. Returns the handler's verdict.
func (s *BoardSuite) orbitCycle(recon *board.Reconciler, taskID, token string, declared time.Time) board.HeartbeatResult {
	s.expireLease(taskID)
	s.rc1Sweep(recon)
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", token), taskID, "", declared)
	s.Require().NoError(err)
	return res
}

// presenceLiveness is the heartbeat-frames shape at the final sweep:
// the worker's own board_heartbeat tool call emitted worker-activity
// PART frames (post-#2042 they stamp the movement index and the
// ordinal query alike), the session may even read busy — ALL of it is
// presence, not reaction, at cap.
func presenceLiveness() board.AssigneeObservation {
	return board.AssigneeObservation{
		Observed: true, Exists: true, NudgeMarkKnown: true,
		MovedSinceLastSweep: true, EventsAfterNudge: true, Busy: true,
	}
}

// PIN 2044-A (born-red, AC1): K+1 heartbeat-only cycles land
// StageFailed via the wedge verdict. Pre-fix this pin is red on its
// first assertion about the K+1th response (it renews) and on the
// verdict (never lands — the orbit renewed forever, attempts 1,
// failure_count 0).
func (s *BoardSuite) TestPin_2044A_HeartbeatOnlyCyclesWedge() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "keystone-orbit", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	cyclesBefore := testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter())
	capK := s.svc.Config().NoAdvanceHeartbeatCap
	s.Require().GreaterOrEqual(capK, 1, "the budget knob is armed in the test config")

	// Cycles 1..K: the no-advance responses RENEW (presence is alive,
	// the budget only counts) and the counter climbs.
	for i := 1; i <= capK; i++ {
		res := s.orbitCycle(recon, task.ID, c.ClaimToken, s.staleDeclare(task.ID))
		s.True(res.Extended, "cycle %d below cap: presence renews (the budget counts, it does not choke)", i)
		s.Equal(i, s.noAdvanceCount(task.ID), "the no-advance cycle is counted")
		s.leaseFuture(task.ID)
	}
	s.Equal(cyclesBefore+float64(capK), testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter()),
		"every motionless response is a countable cycle — the always-on signature")

	// Cycle K+1: the DECLINE. Arrival recorded, cycle counted, sentinel
	// KEPT armed, lease not extended — the worker is told why.
	declined := s.orbitCycle(recon, task.ID, c.ClaimToken, s.staleDeclare(task.ID))
	s.False(declined.Extended, "the K+1th motionless response declines — presence-only renewal is exhausted")
	s.NotEmpty(declined.Reason, "the decline names the budget in its reason")
	s.Contains(declined.Reason, "heartbeats without advancement", "the reason carries the budget prose")
	s.Equal(capK+1, s.noAdvanceCount(task.ID), "the declined response still counts its cycle")
	slotID, _ := s.nudgeState(task.ID)
	s.NotEmpty(slotID, "the decline keeps the sentinel armed — the wedge clock is live")

	// The final sweep: the heartbeat's own PART frames are the
	// row's only post-sentinel activity — presence, not reaction.
	// Every renewal arm is demoted at cap; the wedge verdict lands.
	lives.set(assignee, presenceLiveness())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "K+1 heartbeat-only cycles land the wedge verdict (the orbit is bounded)")
	s.True(rowIsMarkedFailed(s, task.ID))
	s.Nil(row.ClaimToken, "the verdict clears the dead claim")
	s.Equal(1, s.countEventsFor("wedge", task.ID), "the machine verdict audits once, for this row")
	s.Equal(cyclesBefore+float64(capK+1), testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter()))

	// The wedge evidence + payload carry the budget facts (R3).
	var evidence string
	var payload []byte
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT payload::text FROM board_events WHERE action='wedge' AND item_id=$1`, task.ID).Scan(&payload))
	var ev struct {
		Evidence       string `json:"evidence"`
		NoAdvanceRnews int    `json:"no_advance_renews"`
		NoAdvanceCap   int    `json:"no_advance_cap"`
	}
	s.Require().NoError(json.Unmarshal(payload, &ev))
	evidence = ev.Evidence
	s.Contains(evidence, "heartbeats without advancement", "the wedge prose names the budget")
	s.Equal(capK+1, ev.NoAdvanceRnews, "the payload carries the cycle count")
	s.Equal(capK, ev.NoAdvanceCap, "the payload carries the configured cap")
}

// PIN 2044-B (AC1, the demotion): at cap, the heartbeat's own PART
// frames cannot rescue the row through the DEFAULT arms. The final
// sweep of the orbit reports the full presence shape — movement delta
// (the heartbeat tool call's PART frames stamp the worker-activity
// index), post-sentinel ordinal events, and busy — and STILL wedges.
// This is the exact rescue route the issue names: without the demotion
// the row would renew via RenewSourceReaction (or movement) forever.
// (The presence shape is folded into 2044-A's final sweep; this pin
// isolates it per-arm for the reviewer's mutation checks.)
func (s *BoardSuite) TestPin_2044B_AtCapPresenceFramesCannotRescue() {
	for _, shape := range []struct {
		name string
		obs  board.AssigneeObservation
	}{
		{"movement frames", board.AssigneeObservation{Observed: true, Exists: true, MovedSinceLastSweep: true, NudgeMarkKnown: true, EventsAfterNudge: true}},
		{"post-sentinel reaction", board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true, EventsAfterNudge: true}},
		{"busy presence", board.AssigneeObservation{Observed: true, Exists: true, Busy: true, NudgeMarkKnown: true}},
	} {
		s.Run(shape.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "rescue-"+shape.name, nil, false)

			c := s.claimOrFail(ws)
			assignee := s.reloadTask(task.ID).AssigneeSession
			lives := &fakeLiveness{}
			lives.set(assignee, liveQuiet())
			nudger := &fakeNudger{}
			recon := s.pinRecon(lives, nudger.Send)
			capK := s.svc.Config().NoAdvanceHeartbeatCap

			// Exhaust the budget: K renewing cycles, then the decline.
			for i := 1; i <= capK; i++ {
				res := s.orbitCycle(recon, task.ID, c.ClaimToken, s.staleDeclare(task.ID))
				s.Require().True(res.Extended)
			}
			declined := s.orbitCycle(recon, task.ID, c.ClaimToken, s.staleDeclare(task.ID))
			s.Require().False(declined.Extended)

			// The presence shape answers the armed sentinel: every
			// renewal arm is demoted for this lapse — the wedge lands.
			lives.set(assignee, shape.obs)
			s.expireLease(task.ID)
			s.rc1Sweep(recon)
			s.Equal(board.StageFailed, s.rawTask(task.ID).Stage,
				"at cap the heartbeat's own frames are presence, not reaction — no arm rescues the row")
		})
	}
}

// PIN 2044-C (BORN-RED, the headline — AC1): the exact v0.44.19 defect.
// A responding-but-motionless claimant that DECLARES wall-clock now()
// on every heartbeat is now bounded. Pre-fix the handler's
// `observedProgressAt > stage_entered_at` motion predicate was
// vacuously true for such a responder, so no_advance_renews reset to 0
// at every response, the lease re-extended forever, and the orbit cap
// could NEVER trip (prod proof: probe task 615a8cf1 ran ~40 renewal
// cycles over 2.5 days, no_advance_renews pinned at 0, wedged only when
// the responder went silent). Post-fix the handler COUNTS: K responses
// climb the counter to the cap, the K+1th declines, and the next lapse
// lands the wedge verdict — the 3-strike orbit end-to-end.
func (s *BoardSuite) TestPin_2044C_DeclarerStampsNowIsBounded() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "declarer-orbit", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	cyclesBefore := testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter())
	capK := s.svc.Config().NoAdvanceHeartbeatCap
	s.Require().GreaterOrEqual(capK, 1, "the budget knob is armed in the test config")

	// Cycles 1..K: a now()-stamping responder still RENEWS (presence is
	// alive) but the budget COUNTS — the motion credit is gone.
	for i := 1; i <= capK; i++ {
		res := s.orbitCycle(recon, task.ID, c.ClaimToken, time.Now().UTC())
		s.True(res.Extended, "cycle %d below cap: presence renews even for a now-declarer", i)
		s.Equal(i, s.noAdvanceCount(task.ID), "the now()-declared cycle is COUNTED, not credited")
		s.leaseFuture(task.ID)
	}
	s.Equal(cyclesBefore+float64(capK), testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter()),
		"every motionless response is a countable cycle — the always-on signature")

	// Cycle K+1: the DECLINE — arrival recorded, cycle counted, sentinel
	// kept armed, lease not extended. The declared stamp credits nothing.
	declined := s.orbitCycle(recon, task.ID, c.ClaimToken, time.Now().UTC())
	s.False(declined.Extended, "the K+1th now()-declared response declines — the motion credit is gone")
	s.Contains(declined.Reason, "heartbeats without advancement", "the reason carries the budget prose")
	s.Equal(capK+1, s.noAdvanceCount(task.ID), "the declined response still counts its cycle")
	slotID, _ := s.nudgeState(task.ID)
	s.NotEmpty(slotID, "the decline keeps the sentinel armed — the wedge clock is live")

	// The final sweep: the heartbeat's own PART frames are the row's
	// only post-sentinel activity — presence, not reaction. Every
	// renewal arm is demoted at cap; the wedge lands.
	lives.set(assignee, presenceLiveness())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "K+1 now()-declared cycles wedge — the orbit cap finally trips for a declarer")
	s.Equal(1, s.countEventsFor("wedge", task.ID), "the machine verdict audits once, for this row")
	s.Equal(cyclesBefore+float64(capK+1), testutil.ToFloat64(board.HeartbeatNoAdvanceCyclesCounter()))

	// The progress clock was NEVER touched by the response — the
	// ladder's no-epoch-motion detector stayed honest (still at/below
	// the mint), which is what let the cap's demotion fire.
	wm := s.rawTaskWatermark(task.ID)
	s.Require().NotNil(wm, "the claim mint anchored last_progress_at")
	s.False(wm.After(s.orbitMint(task.ID)),
		"the now()-declared heartbeats never moved the progress clock (the poison is gone)")
}

// PIN 2044-D (AC2): the handler COUNTS, but a genuinely active worker is
// never bounded — and its only real escape from a cap-exhausted epoch is
// ADVANCEMENT, not a stamp. (a1) A movement renewal in the reconciler
// ZEROES the budget below cap (the mid-turn streaming shape — fresh
// PART frames / message-count delta are the real motion evidence; the
// heartbeat never credits it). (a2, BORN-RED) At cap a FRESH-DECLARED
// heartbeat is NOT motion: it counts and DECLINES (pre-fix it reset +
// renewed through the handler — the defect); the mover's recovery is a
// STAGE MOVE (which zeroes the budget and re-mints the epoch), never a
// stamp and never frames-only (2044-B pins that at cap the worker's own
// frames are presence, demoted). (b) The ladder's OWN heartbeat belt
// arm counts no-advance belt renewals the same way and skips at cap.
func (s *BoardSuite) TestPin_2044D_MotionlessHeartbeatsCountButAdvancementResets() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)

	// --- task A: the streaming-mover shape (a1 + a2) -----------------
	task := s.seedTask(e, "moving-worker", nil, false)
	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	lives := &fakeLiveness{}
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)
	capK := s.svc.Config().NoAdvanceHeartbeatCap

	// (a1) Below cap, MOVEMENT renews and resets: the row a quiet
	// stretch had climbed to cap-1 on, rescued by REAL activity — the
	// observer-side movement renewal zeroes the budget.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET no_advance_renews=$2 WHERE id=$1`, task.ID, capK-1)
	s.Require().NoError(err)
	lives.set(assignee, liveMoving())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the below-cap mover renews on movement evidence")
	s.leaseFuture(task.ID)
	s.Equal(0, s.noAdvanceCount(task.ID), "a movement renewal resets the budget — real activity is the healthy shape")

	// (a2, BORN-RED) At cap, a fresh-declared heartbeat is NOT motion:
	// the response arm COUNTS and the guarded write DECLINES (slot kept,
	// lease frozen). Pre-fix this re-credited and renewed at cap through
	// the handler — the defect.
	_, err = s.pool.Exec(context.Background(), `
		UPDATE tasks SET no_advance_renews=$2, nudge_id='nudge-mover', nudged_at=now()-interval '5 seconds'
		WHERE id=$1`, task.ID, capK)
	s.Require().NoError(err)
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.False(res.Extended, "at cap, a fresh-declared response is PRESENCE, not motion — it declines")
	s.Equal(capK+1, s.noAdvanceCount(task.ID), "the now()-declared response counted, never reset, even at cap")
	slotID, _ := s.nudgeState(task.ID)
	s.NotEmpty(slotID, "the at-cap decline keeps the sentinel armed")

	// The only real escape is ADVANCEMENT: a stage move zeroes the
	// budget and re-mints the epoch (the sole worker-anchored reset
	// post-fix). A heartbeat stamp and frames-only never do (2044-B).
	_, err = s.svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", task.ShortID,
		board.StageInReview, "", "")
	s.Require().NoError(err, "a stage move is the legitimate advancement that clears the orbit budget")
	s.Equal(board.StageInReview, s.rawTask(task.ID).Stage, "the move advanced the row out of the in_progress orbit")
	s.Equal(0, s.noAdvanceCount(task.ID), "the stage move zeroed the budget (the only worker-anchored reset post-fix)")

	// --- task B: the ladder's own heartbeat belt arm (b) --------------
	belt := s.seedTask(e, "belt-worker", nil, false)
	s.claimOrFail(ws) // claim the belt row (A already advanced out of in_progress)
	bAssignee := s.reloadTask(belt.ID).AssigneeSession
	bLives := &fakeLiveness{}
	bNudger := &fakeNudger{}
	bRecon := s.pinRecon(bLives, bNudger.Send)

	armHB := func(renews int) {
		// The motionless epoch: last_progress_at stays at/below the
		// claim mint (the honest orbiting row — a heartbeat never
		// moves it now; only a claim/persona mint does).
		_, err := s.pool.Exec(context.Background(), `
			UPDATE tasks SET no_advance_renews=$2, nudge_id='nudge-belt', nudged_at=now()-interval '5 seconds',
				last_heartbeat_at=now()-interval '1 second', lease_expires_at=now()-interval '1 millisecond',
				last_progress_at=stage_entered_at - interval '1 minute'
			WHERE id=$1`, belt.ID, renews)
		s.Require().NoError(err)
	}
	armHB(0)
	bLives.set(bAssignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
	s.rc1Sweep(bRecon)
	s.Equal(board.StageInProgress, s.rawTask(belt.ID).Stage, "the ladder heartbeat belt arm renews below cap")
	s.Equal(1, s.noAdvanceCount(belt.ID), "the ladder belt arm counts its no-advance renewal the same way")
	s.leaseFuture(belt.ID)

	armHB(capK)
	// Measured silence: the mark is known, nothing after it — the
	// honest wedge input for a sentinel the observation seam anchored.
	bLives.set(bAssignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
	s.rc1Sweep(bRecon)
	s.Equal(board.StageFailed, s.rawTask(belt.ID).Stage,
		"at cap the ladder belt arm skips — the row proceeds to the wedge verdict")
}

// PIN 2044-E (AC3, R5, BORN-RED half): the response-arm guard. A
// committed sentinel answered 4s later takes the guarded RESPONSE arm —
// slot cleared, lease extended (below cap) — but the fresh-declared
// stamp credits NOTHING: the cycle still COUNTS (counter climbs), where
// pre-fix it reset to 0 (the motion-credit defect). The early-side race
// (heartbeat before the sentinel's row write) is documented honestly:
// the advisory path there is the predicate doing its job (no armed
// nudge to answer), and it counts nothing.
func (s *BoardSuite) TestPin_2044E_ResponseGuard_CommittedNudgeSequence() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "guard-sequence", nil, false)

	c := s.claimOrFail(ws)
	lives := &fakeLiveness{}
	lives.set(s.reloadTask(task.ID).AssigneeSession, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	// The committed sentinel: quiet row lapses, the sweep arms it.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Require().Equal(1, nudger.count())
	slotID, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(slotID, "the sentinel is committed on the row")

	// The observed wall gap: the heartbeat lands 4.4s after the
	// sentinel (row-state spelled — no sleep: the comparison the guard
	// runs is the row's own nudged_at, not wall now()).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET nudged_at=now()-interval '4 seconds' WHERE id=$1 AND nudge_id IS NOT NULL`, task.ID)
	s.Require().NoError(err)

	// The live token answers: the guarded RESPONSE arm — but the
	// now()-declared stamp is presence, not motion: slot cleared +
	// lease extended (below cap), counter COUNTED.
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.True(res.Extended, "a committed sentinel answered 4s later takes the guarded RESPONSE arm")
	clearedID, _ := s.nudgeState(task.ID)
	s.Empty(clearedID, "the response clears the nudge slot")
	s.leaseFuture(task.ID)
	s.Equal(1, s.noAdvanceCount(task.ID), "a now()-declared response COUNTS its cycle (no motion credit)")

	// The early-side race, pinned as the anomaly's reachable shape:
	// the worker answers a sentinel whose row write has NOT landed (no
	// armed nudge on the row) — the advisory path records the arrival
	// and extends nothing, exactly as designed; the row then arms its
	// own sentinel and the worker's NEXT response is the convergence.
	s.expireLease(task.ID)
	res, err = s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.False(res.Extended, "no armed sentinel on the row: the advisory path is honest (not a guard misfire)")
	s.NotEmpty(res.Reason)
	s.Equal(1, s.noAdvanceCount(task.ID), "an advisory arrival counts no cycle — no nudge was answered")
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
}

// PIN 2044-F (AC2, reviewer personas): reviewer persona heartbeats
// during an in_review cycle never trip the budget — structurally: the
// nudge ladder scans in_progress rows only (rc1PersonaClaims owns
// in_review and never arms sentinels), so a persona row never carries
// a live nudge, the response arm never fires for it, and the budget
// counter never moves. The reviewer's per-cycle heartbeat is advisory
// presence, renewed by the persona re-arm pass instead.
func (s *BoardSuite) TestPin_2044F_ReviewerHeartbeatsNeverTrip() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "reviewer-orbit", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(personaRoleDirectory()))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	c := s.claimOrFail(ws)
	_, err := svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	reviewerToken := *s.rawTask(task.ID).ClaimToken

	// The reviewer heartbeats every review cycle — advisory every
	// time (no nudge ever arms on in_review), budget never engaged.
	for i := 0; i < 5; i++ {
		hb, err := svc.Heartbeat(context.Background(), s.agent("reviewer", reviewerToken), "", task.ShortID, time.Now().UTC())
		s.Require().NoError(err)
		s.False(hb.Extended, "no nudge on an in_review claim — the periodic discipline stays dead")
		s.Equal(0, s.noAdvanceCount(task.ID), "the budget never counts a persona heartbeat")
	}
	slotID, _ := s.nudgeState(task.ID)
	s.Empty(slotID, "no sentinel ever arms on the reviewer's row")
	s.Equal(board.StageInReview, s.rawTask(task.ID).Stage)
}

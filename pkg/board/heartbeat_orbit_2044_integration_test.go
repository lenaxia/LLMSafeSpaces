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
// heartbeat responses whose declared watermark never crosses the claim
// mint (last_progress_at > stage_entered_at — one Postgres clock
// domain, server-side) exhaust presence-only renewal. The K+1th
// response DECLINES (arrival recorded, cycle counted, sentinel kept
// armed, lease not extended), and the next lapse demotes EVERY renewal
// arm for that lapse — hbAfterNudge, MovedSinceLastSweep, Busy, and
// EventsAfterNudge all read false, because the heartbeat's own tool
// call emits worker-activity PART frames (#2042's allowlist) that are
// PRESENCE, not reaction. The row proceeds to the existing wedge
// verdict (StageFailed via the reconciler path).
//
// Pins (born-red against pre-fix main: the orbit renewed forever):
//
//   - 2044-A: the full orbit cycle sequence — K no-advance responses
//     renew (counter 1..K), the K+1th declines (slot kept, lease
//     frozen), the next sweep lands the wedge verdict; the wedge
//     event's payload + evidence prose carry the budget facts.
//   - 2044-B: at cap, the heartbeat's own PART frames cannot rescue
//     the row through ANY renewal arm — MovedSinceLastSweep,
//     EventsAfterNudge, and Busy are all demoted for the lapse.
//   - 2044-C: a worker that declares FRESH progress (past the mint)
//     is never bounded — every response renews, the counter stays 0,
//     the row lives indefinitely.
//   - 2044-D: a movement renewal resets the budget (activity between
//     heartbeat cycles keeps a worker alive); the ladder's own
//     heartbeat arm counts no-advance renewals the same way and skips
//     at cap.
//   - 2044-E (R5): a committed nudge answered 4s later takes the
//     guarded RESPONSE arm — slot cleared, lease extended, budget
//     reset — and the early-side race (heartbeat before the sentinel's
//     row write) is documented honestly: the advisory path there is
//     the predicate doing its job (no armed nudge to answer), and the
//     next response converges.
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

// PIN 2044-C (AC2): a worker that heartbeats AND declares fresh
// progress is never bounded — the healthy review-loop shape (respond,
// work, respond) resets the budget on every response and keeps the
// row alive indefinitely.
func (s *BoardSuite) TestPin_2044C_FreshDeclarationsNeverBounded() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "healthy-hb", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)
	capK := s.svc.Config().NoAdvanceHeartbeatCap

	// Well past the cap: a healthy worker's declared watermark is its
	// LAST OBSERVED PROGRESS — fresh every time it actually works.
	for i := 1; i <= capK+3; i++ {
		res := s.orbitCycle(recon, task.ID, c.ClaimToken, time.Now().UTC())
		s.True(res.Extended, "cycle %d: fresh progress renews — motion resets the budget every time", i)
		s.Equal(0, s.noAdvanceCount(task.ID), "the declared watermark crossed the mint: the counter resets")
		s.leaseFuture(task.ID)
	}
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the healthy worker is never bounded")
	s.Equal(0, s.noAdvanceCount(task.ID))
}

// PIN 2044-D (AC2, both halves): real activity between heartbeat
// cycles keeps a worker alive — through the mechanisms the design
// actually grants it. (a) A movement renewal below cap RESETS the
// budget (the frames-vs-presence seam cannot distinguish a mover's
// tool frames from the heartbeat's own, which is exactly WHY at cap
// frames alone renew nothing — 2044B); a cap-exhausted mover's
// recovery is the respond-first contract: its next FRESH-declared
// response resets the budget and renews, at cap, through the handler.
// (b) The ladder's OWN heartbeat arm (the ordinal belt for raced row
// states) counts no-advance renewals identically and skips at cap.
func (s *BoardSuite) TestPin_2044D_MovementResetsAndLadderArmCounts() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "moving-worker", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)
	lives := &fakeLiveness{}
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)
	capK := s.svc.Config().NoAdvanceHeartbeatCap

	// (a1) Below cap, MOVEMENT renews and resets: the row a quiet
	// stretch had climbed to cap-1 on, answered by real activity —
	// the movement renewal zeroes the budget (consecutive semantics).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET no_advance_renews=$2 WHERE id=$1`, task.ID, capK-1)
	s.Require().NoError(err)
	lives.set(assignee, liveMoving())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the below-cap mover renews on movement evidence")
	s.leaseFuture(task.ID)
	s.Equal(0, s.noAdvanceCount(task.ID), "a movement renewal resets the budget — activity between cycles is the healthy shape")

	// (a2) At cap, the mover's recovery is the RESPONSE channel (the
	// respond-first contract): a fresh-declared watermark past the
	// mint resets the budget and renews — even at cap, through the
	// handler, in the same guarded write.
	_, err = s.pool.Exec(context.Background(), `
		UPDATE tasks SET no_advance_renews=$2, nudge_id='nudge-mover', nudged_at=now()-interval '5 seconds'
		WHERE id=$1`, task.ID, capK)
	s.Require().NoError(err)
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.True(res.Extended, "at cap, a fresh-declared response is MOTION: reset and renewal in the same write")
	s.Equal(0, s.noAdvanceCount(task.ID), "the mover's declared watermark crossed the mint — the budget resets at cap")

	// (b) The ladder's heartbeat arm: shape the ordinal row state the
	// belt exists for (nudge armed, last_heartbeat_at after it — e.g.
	// a restored snapshot or an exotic multi-replica interleaving).
	// Below cap it renews AND counts; at cap it skips and the row
	// falls through to the quiet arms (measured silence lands the
	// wedge verdict).
	armHB := func(renews int) {
		// The motionless epoch: the declared watermark sits below the
		// claim mint (the honest orbiting row — part (a2)'s fresh
		// declaration is rewound; a row whose watermark DID cross the
		// mint renews with a reset, by design).
		_, err := s.pool.Exec(context.Background(), `
			UPDATE tasks SET no_advance_renews=$2, nudge_id='nudge-belt', nudged_at=now()-interval '5 seconds',
				last_heartbeat_at=now()-interval '1 second', lease_expires_at=now()-interval '1 millisecond',
				last_progress_at=stage_entered_at - interval '1 minute'
			WHERE id=$1`, task.ID, renews)
		s.Require().NoError(err)
	}
	armHB(0)
	s.rc1Sweep(recon)
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the ladder heartbeat arm renews below cap")
	s.Equal(1, s.noAdvanceCount(task.ID), "the ladder heartbeat arm counts its no-advance renewal the same way")
	s.leaseFuture(task.ID)

	armHB(capK)
	// Measured silence: the mark is known, nothing after it — the
	// honest wedge input for a sentinel the observation seam anchored.
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true})
	s.rc1Sweep(recon)
	s.Equal(board.StageFailed, s.rawTask(task.ID).Stage,
		"at cap the ladder heartbeat arm skips — the row proceeds to the wedge verdict")
}

// PIN 2044-E (AC3, R5): the response-arm guard. The incident's 03:00:03
// heartbeat (4.4s after the 02:59:59 sentinel) took the ADVISORY path
// — slot kept, no extension. This pin drives the exact committed
// sequence deterministically: a committed sentinel (row-state spelled,
// nudged_at backdated the observed 4s wall gap), then the live-token
// heartbeat — and asserts the guarded RESPONSE arm wins.
//
// The verdict on the anomaly (documented honestly, per the issue's
// "born-red pin either way"): by code trace the response and advisory
// predicates differ ONLY in `nudge_id IS NOT NULL` (same stage set,
// same token, same pending_input leg), so no token-identity mismatch
// exists; and the guarded UPDATE evaluates its predicate on the
// live locked row, so a raced READ of nudge_id cannot influence it.
// The reachable advisory outcome for an ANSWERED sentinel is the
// EARLY-side race — the worker responds to a sentinel whose row write
// has not landed yet (bus redelivery of an old sentinel, or delivery
// preceding the sweep's commit): the row genuinely carries no armed
// nudge, the advisory path is the predicate doing its job, and the
// FOLLOW-UP response converges (the second half below pins that).
// The during-sweep race lands RESPONSE by lock ordering — pinned by
// TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost.
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

	// The live token answers: the guarded RESPONSE arm.
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.True(res.Extended, "a committed sentinel answered 4s later takes the guarded RESPONSE arm")
	clearedID, _ := s.nudgeState(task.ID)
	s.Empty(clearedID, "the response clears the nudge slot")
	s.leaseFuture(task.ID)
	s.Equal(0, s.noAdvanceCount(task.ID), "a fresh-declared response resets the budget (motion)")

	// The early-side race, pinned as the anomaly's reachable shape:
	// the worker answers a sentinel whose row write has NOT landed
	// (no armed nudge on the row) — the advisory path records the
	// arrival and extends nothing, exactly as designed; the row then
	// arms its own sentinel and the worker's NEXT response is the
	// credited convergence.
	s.expireLease(task.ID)
	res, err = s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", time.Now().UTC())
	s.Require().NoError(err)
	s.False(res.Extended, "no armed sentinel on the row: the advisory path is honest (not a guard misfire)")
	s.NotEmpty(res.Reason)
	s.Equal(0, s.noAdvanceCount(task.ID), "an advisory arrival counts no cycle — no nudge was answered")
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

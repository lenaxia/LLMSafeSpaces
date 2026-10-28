// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// lease_pins_test.go — the seamless-lease design pins (spec §Pins,
// reports/specs/seamless-lease-design.md; owner rulings 2026-10-27/28,
// the item-6 amendment adding pins 9-11). Eleven born-precise pins over
// the RC1 ladder, each driving the sweep DETERMINISTICALLY:
//
//   - Clock: the house DB-clock seam (expireLease rewinds
//     lease_expires_at; every ladder comparison is SQL now()-based, so
//     a rewound lease IS an expired lease — no sleep, no flake). The
//     "chosen clock positions" are the rewind points between sweeps.
//   - Sweep: rc1Sweep() runs EXACTLY the RC1 phase — the rc1.post
//     failpoint aborts the sweep AFTER rc1Reclaims' transaction
//     committed (the kill-test fault-seam precedent), so the ladder's
//     writes are observed with no other phase running.
//
// The pins are written against fakes at the AssigneeLivenessSource and
// NudgeSender seams — harness-agnostic BY CONSTRUCTION: nothing here
// knows how a real harness answers the three questions, only that the
// engine's ladder decisions follow the answers (the opencode adapter
// that produces those answers lives api-side and carries its own
// tests).
package board_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeLiveness scripts per-session answers at the observation seam.
// The zero scripting for a session is alive-but-quiet (Observed+Exists,
// no movement) — the nudge-able shape; every pin states its answers
// explicitly anyway (the reviewer mutation-checks the mapping).
type fakeLiveness struct {
	mu  sync.Mutex
	obs map[string]board.AssigneeObservation
	// asked records every ref the engine queried (the "consults
	// liveness BEFORE re-pooling" half of the seam contract — a pin
	// that never asked is a pin that passed by luck).
	asked []board.AssigneeRef
}

func (f *fakeLiveness) ObserveAssignees(sess board.SessionObservations) board.SessionObservationResults {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(board.SessionObservationResults, len(sess.Refs))
	for _, ref := range sess.Refs {
		f.asked = append(f.asked, ref)
		o, ok := f.obs[ref.SessionID]
		if !ok {
			o = board.AssigneeObservation{Observed: true, Exists: true}
		}
		out[ref.SessionID] = o
	}
	return out
}

func (f *fakeLiveness) set(session string, o board.AssigneeObservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.obs == nil {
		f.obs = map[string]board.AssigneeObservation{}
	}
	f.obs[session] = o
}

// askedFor reports how many queries the engine made for a session.
func (f *fakeLiveness) askedFor(session string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ref := range f.asked {
		if ref.SessionID == session {
			n++
		}
	}
	return n
}

// nudgedCall is one recorded sentinel delivery.
type nudgedCall struct {
	Session string
	Payload board.NudgePayload
}

// fakeNudger records sentinel deliveries (err, when set, fails them).
type fakeNudger struct {
	mu   sync.Mutex
	sent []nudgedCall
	err  error
}

func (f *fakeNudger) Send(_ context.Context, session string, p board.NudgePayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, nudgedCall{Session: session, Payload: p})
	return nil
}

func (f *fakeNudger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// liveQuiet / liveMoving / dead are the three scripted answers the
// pins reuse.
func liveQuiet() board.AssigneeObservation {
	return board.AssigneeObservation{Observed: true, Exists: true}
}

func liveMoving() board.AssigneeObservation {
	return board.AssigneeObservation{Observed: true, Exists: true, MovedSinceLastSweep: true}
}

func dead() board.AssigneeObservation {
	return board.AssigneeObservation{Observed: true, Exists: false}
}

// pinRecon builds a reconciler carrying the liveness + nudge seams (the
// harness default reconciler runs Nil — pin 6's subject).
func (s *BoardSuite) pinRecon(lives board.AssigneeLivenessSource, nudger board.NudgeSender) *board.Reconciler {
	return board.NewReconciler(s.pool, s.cfg,
		board.WithLivenessSource(lives),
		board.WithNudgeSender(nudger),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
}

// rc1Sweep runs EXACTLY the RC1 phase: the rc1.post fault aborts the
// sweep after rc1Reclaims' transaction committed, so the ladder's
// writes are observable with no other phase interfering. The injected
// error is the failpoint's own — required, not tolerated.
func (s *BoardSuite) rc1Sweep(recon *board.Reconciler) {
	s.fps.armAt("rc1.post", errors.New("pin: stop after rc1"))
	defer s.fps.disarm("rc1.post")
	_, err := recon.Sweep(context.Background())
	s.Require().Error(err, "the rc1.post fault must abort the sweep")
	s.Require().Contains(err.Error(), "rc1.post", "the fault is the sweep's error")
}

// nudgeState reads the row's persisted nudge slot (nudge_id, nudged_at).
func (s *BoardSuite) nudgeState(taskID string) (id string, at *time.Time) {
	var nudgedAt *time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT coalesce(nudge_id,''), nudged_at FROM tasks WHERE id=$1`, taskID).Scan(&id, &nudgedAt))
	return id, nudgedAt
}

// leaseFuture asserts the row's lease is armed in the future (renewed /
// nudge-extended), tolerance for the sweep's own latency.
func (s *BoardSuite) leaseFuture(taskID string) {
	var lease time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, taskID).Scan(&lease))
	s.Require().NotNil(lease)
	s.True(lease.After(time.Now().Add(-time.Second)),
		"lease must be re-armed ahead of now (got %s)", lease)
}

// countEventsFor counts audit events for one row (subtest-safe: the
// suite's countEvents is global and subtests share the test's DB).
func (s *BoardSuite) countEventsFor(action, taskID string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action=$1 AND item_id=$2`, action, taskID).Scan(&n))
	return n
}

// reclaimOverlay asserts the #1813 failure overlay the wall-clock
// reclaim path writes.
func (s *BoardSuite) reclaimOverlay(id string, failures int) {
	row := s.rawTask(id)
	s.Equal(board.StageBacklog, row.Stage)
	s.Nil(row.ClaimToken)
	s.Nil(row.Assignee)
	s.True(rowIsMarkedFailed(s, id), "last_attempt_failed set")
	var fc int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT failure_count FROM tasks WHERE id=$1`, id).Scan(&fc))
	s.Equal(failures, fc)
}

func rowIsMarkedFailed(s *BoardSuite, id string) bool {
	var b bool
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_attempt_failed FROM tasks WHERE id=$1`, id).Scan(&b))
	return b
}

// PIN 1 (spec §Pins 1): a live worker — the harness observed advancing
// activity — NEVER lapses, across three consecutive expiries (3× the
// lease window). No nudge is ever sent to a moving worker; no reclaim,
// no overlay, no transition churn; the claim's token survives intact.
func (s *BoardSuite) TestPin_Lease1_LiveWorkerNeverLapses() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "live-worker", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee, "the harness spawner stamps the assignee")

	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	recon := s.pinRecon(lives, func(context.Context, string, board.NudgePayload) error {
		s.T().Fatalf("a MOVING worker must never be nudged")
		return nil
	})

	for round := 1; round <= 3; round++ {
		s.expireLease(task.ID) // the lease window passes; the worker keeps working
		s.rc1Sweep(recon)

		row := s.rawTask(task.ID)
		s.Equal(board.StageInProgress, row.Stage, "round %d: a moving worker never lapses", round)
		s.Require().NotNil(row.ClaimToken)
		s.Equal(c.ClaimToken, *row.ClaimToken, "round %d: the claim's token survives", round)
		s.Equal(assignee, *row.Assignee)
		s.leaseFuture(task.ID)
		s.Equal(0, s.countEvents("reclaim"), "round %d: no reclaim event", round)
		s.Equal(0, s.countEvents("nudge"), "round %d: no nudge event", round)
		s.Equal(0, s.countEvents("wedge"), "round %d: no wedge event", round)
		s.False(rowIsMarkedFailed(s, task.ID))
		nid, _ := s.nudgeState(task.ID)
		s.Empty(nid, "round %d: the nudge slot stays empty", round)
	}
	s.GreaterOrEqual(lives.askedFor(assignee), 3, "the engine consulted liveness every sweep")
	s.Equal(1, s.countTransitions(task.ID, board.StageInProgress),
		"the claim's transition is the only in_progress write — renewals write no transitions")
}

// PIN 2 (spec §Pins 2): lease expiry with no movement → EXACTLY ONE
// sentinel nudge, recorded on the row (latest-only slot), the lease
// extended one window as the clean measurement point for the ordinal
// question — and the row is NOT reclaimed (nudge replaces the silent
// re-pool the owner ruled out). A mid-window sweep adds nothing.
func (s *BoardSuite) TestPin_Lease2_QuietRowNudgedOnceNotReclaimed() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "quiet-worker", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the nudge replaces the reclaim — the row never moved")
	s.Require().NotNil(row.ClaimToken)
	s.Equal(c.ClaimToken, *row.ClaimToken, "the nudged worker keeps its claim")
	s.leaseFuture(task.ID)

	s.Equal(1, nudger.count(), "exactly one sentinel delivered")
	s.Equal(assignee, nudger.sentPayload(0).Session)
	s.Equal(task.ID, nudger.sentPayload(0).Payload.TaskID,
		"the sentinel identifies the task it concerns")
	s.Require().NotEmpty(nudger.sentPayload(0).Payload.NudgeID, "the engine mints the ordinal anchor")

	nid, nudgedAt := s.nudgeState(task.ID)
	s.Equal(nudger.sentPayload(0).Payload.NudgeID, nid,
		"the row records the delivered sentinel's id (the ordinal anchor)")
	s.Require().NotNil(nudgedAt, "nudged_at persisted (restart-safe)")

	s.Equal(1, s.countEvents("nudge"), "one nudge audit event")
	var evNudge string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT payload->>'nudge_id' FROM board_events WHERE action='nudge' AND item_id=$1`, task.ID).Scan(&evNudge))
	s.Equal(nid, evNudge, "the audit event carries the row's nudge id")
	s.Equal(0, s.countEvents("reclaim"))
	s.Equal(1, s.countTransitions(task.ID, board.StageInProgress),
		"the nudge writes no transition row")

	// A sweep INSIDE the new window observes nothing (lease not
	// expired → not even a victim): still exactly one nudge.
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count(), "mid-window sweeps never re-nudge")
	s.Equal(1, s.countEvents("nudge"))
}

func (f *fakeNudger) sentPayload(i int) nudgedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.sent) {
		return nudgedCall{}
	}
	return f.sent[i]
}

// PIN 3 (spec §Pins 3): nudged + next expiry + NOTHING after the
// sentinel → the machine WEDGE VERDICT: failed with wedge evidence,
// the claim cleared, the #1813 overlay marked — and re-arm is the exit
// (the row re-pools through the human judgment, claimable again).
func (s *BoardSuite) TestPin_Lease3_WedgeVerdictFailsAndRearms() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "wedged-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	// Episode: expiry → nudge.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count())
	nid, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid)

	// Next expiry: the ordinal answer is silence — even a thinking
	// model emits a turn-start; nothing after a positioned sentinel is
	// a machine verdict, not a guess.
	lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, EventsAfterNudge: false})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "the wedge verdict lands failed")
	s.Nil(row.ClaimToken, "the dead claim is cleared")
	s.Nil(row.Assignee)
	s.Require().NotNil(row.PendingOrigin)
	s.Equal(board.PendingOriginEscalated, *row.PendingOrigin, "the terminus carries the escalated marker (rearm's gate)")
	s.False(row.PendingInput, "a failure is never an input costume")
	s.True(rowIsMarkedFailed(s, task.ID), "the wedge is an attempt failure")
	wid, _ := s.nudgeState(task.ID)
	s.Empty(wid, "the verdict clears the nudge slot")

	s.Equal(1, s.countEvents("wedge"), "one wedge audit event")
	var verdict, evidence, evNudge string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT payload->>'verdict', payload->>'evidence', payload->>'nudge_id'
		 FROM board_events WHERE action='wedge' AND item_id=$1`, task.ID).
		Scan(&verdict, &evidence, &evNudge))
	s.Equal("wedge", verdict)
	s.Equal("no session events after the sentinel", evidence, "the evidence states the ordinal fact")
	s.Equal(nid, evNudge, "the verdict cites the sentinel it measured")
	s.Equal(1, s.countTransitions(task.ID, board.StageFailed), "the verdict writes the transition row")

	// Re-arm is the exit: the human judgment re-pools the row.
	rearmed, err := s.svc.RearmTask(context.Background(), s.owner(), task.ID, "", "")
	s.Require().NoError(err)
	s.Equal(board.StageBacklog, rearmed.Stage)
	s.clearTaskBackoff(task.ID)
	c2 := s.claimOrFail(ws)
	s.Equal(task.ID, c2.TaskID, "the re-armed row is claimable again")
}

// PIN 4 (spec §Pins 4): nudged + ANY post-sentinel activity → renewed
// and the nudge state cleared — both arms: an explicit reaction to the
// sentinel (EventsAfterNudge) and plain movement (a delta observed
// while nudged). The worker answered; the episode is over.
func (s *BoardSuite) TestPin_Lease4_PostSentinelActivityRenewsAndClears() {
	for _, tc := range []struct {
		name string
		obs  board.AssigneeObservation
	}{
		{"reaction to the sentinel", board.AssigneeObservation{Observed: true, Exists: true, EventsAfterNudge: true}},
		{"movement while nudged", liveMoving()},
	} {
		s.Run(tc.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "answered-worker", nil, false)

			c := s.claimOrFail(ws)
			assignee := s.reloadTask(task.ID).AssigneeSession

			lives := &fakeLiveness{}
			lives.set(assignee, liveQuiet())
			nudger := &fakeNudger{}
			recon := s.pinRecon(lives, nudger.Send)

			s.expireLease(task.ID)
			s.rc1Sweep(recon)
			s.Equal(1, nudger.count())
			nid, _ := s.nudgeState(task.ID)
			s.Require().NotEmpty(nid)

			lives.set(assignee, tc.obs)
			s.expireLease(task.ID)
			s.rc1Sweep(recon)

			row := s.rawTask(task.ID)
			s.Equal(board.StageInProgress, row.Stage, "an answered nudge renews — never a wedge")
			s.Require().NotNil(row.ClaimToken)
			s.Equal(c.ClaimToken, *row.ClaimToken)
			s.leaseFuture(task.ID)
			cleared, _ := s.nudgeState(task.ID)
			s.Empty(cleared, "the nudge state is cleared (episode over)")
			s.Equal(1, nudger.count(), "no second nudge")
			s.Equal(1, s.countEventsFor("nudge", task.ID), "still exactly one nudge event for this row")
			s.Equal(0, s.countEventsFor("wedge", task.ID))
			s.Equal(0, s.countEventsFor("reclaim", task.ID))
		})
	}
}

// PIN 5 (spec §Pins 5): dead-evidence (the session provably no longer
// exists) → the FAST reclaim within one window: the first post-expiry
// sweep re-pools via today's wall-clock path with the #1813 overlay —
// and no nudge is wasted on a corpse.
func (s *BoardSuite) TestPin_Lease5_DeadEvidenceFastReclaimNoNudgeWasted() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "dead-worker", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(assignee, dead())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon) // the FIRST post-expiry sweep decides — within one window

	s.reclaimOverlay(task.ID, 1)
	s.Equal(1, s.countEvents("reclaim"), "the ordinary reclaim audit (dead-evidence rides the wall-clock path)")
	s.Equal(0, nudger.count(), "no sentinel is wasted on a dead session")
	s.Equal(0, s.countEvents("nudge"))
	nid, _ := s.nudgeState(task.ID)
	s.Empty(nid)
}

// PIN 6 (spec §Pins 6): a silent assignee with NO observation AND no
// nudge seam (no evidence either way — unobservable, degraded, or
// unwired, and no way to ask) → the honest lapse within one window:
// today's wall-clock semantics, byte for byte. Correctness never
// depends on integration depth. (With a sender wired, the item-6
// heartbeat floor applies instead — pin 9/10: the no-evidence rung
// runs nudge → heartbeat-or-wedge.)
func (s *BoardSuite) TestPin_Lease6_NilSourceHonestLapse() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "nil-source", nil, false)

	s.claimOrFail(ws)

	recon := s.pinRecon(board.NilLiveness{}, nil) // Nil + NO nudger: nobody can ask

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.reclaimOverlay(task.ID, 1)
	s.Equal(1, s.countEvents("reclaim"))
	s.Equal(0, s.countEvents("nudge"), "no nudge without an observation AND a sender")
}

// PIN 7 (spec §Pins 7): claims issue ONLY to spawner-minted sessions —
// the wakeability invariant. The assignee stamped on a claim is exactly
// the session the SPAWNER minted for THAT claim (never invented,
// never stale); the stamp re-arms the lease from stamp time; and a
// spawn that loses the race to a concurrent reclaim stamps nothing and
// aborts the dead charter — no assignee exists that the spawner did
// not mint for the live claim token.
func (s *BoardSuite) TestPin_Lease7_ClaimsIssueToSpawnerMintedSessions() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	first := s.seedTask(e, "mint-a", nil, false)
	second := s.seedTask(e, "mint-b", nil, false)

	// (a) The stamped assignee IS the spawner's minted id for the claim.
	c1 := s.claimOrFail(ws)
	row := s.rawTask(first.ID)
	s.Require().NotNil(row.Assignee)
	s.Equal("sess-"+c1.ShortID, *row.Assignee,
		"the assignee is the spawner-minted session id for this claim")
	s.leaseFuture(first.ID) // (b) the stamp re-arms the lease from stamp time (r1-C4)

	// (c) A spawn that loses the race: the row is reclaimed WHILE the
	// spawn is in flight — the guarded stamp refuses, the dead charter
	// is aborted, and the row carries NO assignee.
	s.spawner.failWith(nil)
	raced := &raceSpawner{delegate: s.spawner, suite: s, taskID: second.ID}
	racedSvc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(raced.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
	c2, err := racedSvc.ClaimNext(context.Background(), "worker-race", ws)
	s.Require().NoError(err)
	s.Equal(second.ID, c2.TaskID)

	racedRow := s.rawTask(second.ID)
	s.Nil(racedRow.Assignee, "a lost-race spawn stamps nothing")
	s.Equal(1, s.aborter.count(), "the dead charter is aborted (the interrupt seam)")
	s.Equal(second.ID+"/"+raced.minted, s.aborter.callsSlice()[0],
		"the abort names the just-spawned session")
}

// raceSpawner wraps the harness spawner with an in-flight reclaim: the
// row is driven to backlog DURING the spawn call, so the guarded stamp
// back in spawnSync finds no live claim token.
type raceSpawner struct {
	delegate *fakeSpawner
	suite    *BoardSuite
	taskID   string
	minted   string
	once     sync.Once
}

func (r *raceSpawner) Spawn(ctx context.Context, c *board.Claim) (string, error) {
	id, err := r.delegate.Spawn(ctx, c)
	if err != nil {
		return "", err
	}
	r.once.Do(func() {
		r.minted = id
		_, rerr := r.suite.pool.Exec(ctx,
			`UPDATE tasks SET stage='backlog', stage_entered_at=now(),
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL
		 WHERE id=$1 AND claim_token=$2`, r.taskID, c.ClaimToken)
		r.suite.Require().NoError(rerr)
	})
	return id, nil
}

// PIN 8 (spec §Pins 8): the 409 claim_token arbitration is untouched —
// the seamless lease changes WHO loses a claim, never how the loss is
// enforced. After a Nil-source lapse + reclaim, the superseded holder's
// writes are refused with claim_token guards, and the fresh claimant
// proceeds with its own token. (The suite's unchanged pass under the
// unwired/Nil default is the standing half of this pin: every other
// test in this package runs the ladder at Nil.)
func (s *BoardSuite) TestPin_Lease8_ClaimTokenArbitrationUntouched() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "arbitration", nil, false)

	c := s.claimOrFail(ws)
	oldToken := c.ClaimToken

	// The honest lapse needs NO nudge seam (a wired sender would run the
	// rung-3 heartbeat floor instead — pin 9/10's world).
	recon := s.pinRecon(board.NilLiveness{}, nil)
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.reclaimOverlay(task.ID, 1)

	// The superseded holder's heartbeat: refused.
	_, err := s.svc.Heartbeat(context.Background(), s.agent("sess", oldToken), task.ID, "", time.Now().Add(time.Second))
	s.Require().Error(err)
	var guard *board.GuardError
	s.Require().True(errors.As(err, &guard))
	s.Equal(board.GuardClaimToken, guard.Code, "heartbeat after the reclaim: 409 claim_token")

	// Its stage move against the reclaimed (backlog) row: refused by
	// the stage machine — the row left the mover's world; from backlog
	// there is no worker stage write at all.
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", oldToken), task.ID, "", board.StageInReview, "", "")
	s.Require().Error(err)
	s.Require().True(errors.As(err, &guard))
	s.Equal(board.GuardStage, guard.Code, "stage move after the reclaim: the machine owns the row again")

	// The fresh claimant proceeds: its token works.
	s.clearTaskBackoff(task.ID)
	c2 := s.claimOrFail(ws)
	s.Equal(task.ID, c2.TaskID)
	_, err = s.svc.Heartbeat(context.Background(), s.agent("sess-fresh", c2.ClaimToken), task.ID, "", time.Now().Add(time.Second))
	s.Require().NoError(err, "the fresh claimant's token arbitrates wins")

	// The OLD token against the NEW claim's in_progress row: the
	// claim_token guard — one arbiter, the row's live token.
	_, err = s.svc.Heartbeat(context.Background(), s.agent("sess", oldToken), task.ID, "", time.Now().Add(time.Second))
	s.Require().True(errors.As(err, &guard) && guard.Code == board.GuardClaimToken,
		"the superseded token stays refused against the new claim")
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", oldToken), task.ID, "", board.StageInReview, "", "")
	s.Require().True(errors.As(err, &guard) && guard.Code == board.GuardClaimToken,
		"the superseded token's stage write on the new claim: 409 claim_token")
}

// PIN 9 (spec §Pins 9, the item-6 amendment): nudge → heartbeat
// response ⇒ renewed — at EVERY rung of the ladder. The decisive arm is
// the NO-EVIDENCE rung (Nil source): the heartbeat is the ONLY wedge
// detector there (deliver-a-message + call-a-tool is the harness
// floor), so the rung-3 ladder now NUDGES when a sender is wired and
// the response renews. The observed arm pins the OR: silence on the
// event channel + a response on the heartbeat channel = alive, never a
// wedge.
func (s *BoardSuite) TestPin_Lease9_HeartbeatResponseRenewsEveryRung() {
	s.Run("no-evidence rung (Nil source): nudge → response renews", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "hb-floor", nil, false)

		c := s.claimOrFail(ws)
		nudger := &fakeNudger{}
		recon := s.pinRecon(board.NilLiveness{}, nudger.Send) // no observation, sender wired: the floor

		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		s.Equal(1, nudger.count(), "the no-evidence rung nudges when a sender is wired (the harness floor)")

		// The worker RESPONDS FIRST — the wedge clock dies before any
		// long command.
		res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
			time.Now().UTC())
		s.Require().NoError(err)
		s.True(res.Extended, "the nudge response renews at the only-detector rung")
		s.leaseFuture(task.ID)
		cleared, _ := s.nudgeState(task.ID)
		s.Empty(cleared, "the response clears the nudge slot (the wedge clock is dead)")

		// And the renewal is real: the next sweep (still inside the new
		// window) neither re-nudges nor wedges.
		s.rc1Sweep(recon)
		s.Equal(1, nudger.count(), "no re-nudge inside the response-renewed window")
		s.Equal(0, s.countEvents("wedge"))
	})

	s.Run("observed rung: event-silent, response-alive → renew not wedge", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "hb-or", nil, false)

		c := s.claimOrFail(ws)
		assignee := s.reloadTask(task.ID).AssigneeSession
		lives := &fakeLiveness{}
		lives.set(assignee, liveQuiet())
		nudger := &fakeNudger{}
		recon := s.pinRecon(lives, nudger.Send)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		s.Equal(1, nudger.count())

		// The observation reports NOTHING after the sentinel — but the
		// worker answered on the heartbeat channel. Alive.
		lives.set(assignee, board.AssigneeObservation{Observed: true, Exists: true, EventsAfterNudge: false})
		res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
			time.Now().UTC())
		s.Require().NoError(err)
		s.True(res.Extended, "the response renews at the observed rung too")
		cleared, _ := s.nudgeState(task.ID)
		s.Empty(cleared, "the response itself clears the slot — the wedge clock dies before any long command")

		// The response ended the episode. A FULL window of renewed
		// silence starts a FRESH episode (latest-only slot): the next
		// sweep nudges again — it never wedges on a dead episode's
		// ordinal question, and it never reuses the cleared sentinel.
		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		row := s.rawTask(task.ID)
		s.Equal(board.StageInProgress, row.Stage, "a heartbeat response beats the wedge at the observed rung")
		s.Require().NotNil(row.ClaimToken)
		s.Equal(c.ClaimToken, *row.ClaimToken)
		s.leaseFuture(task.ID)
		s.Equal(0, s.countEvents("wedge"))
		s.Equal(2, nudger.count(), "renewed silence opens a NEW episode (a fresh sentinel, latest-only)")
	})
}

// PIN 10 (spec §Pins 10, the item-6 amendment): nudge → silence on
// BOTH channels (no post-sentinel events, no post-nudge heartbeat) ⇒
// wedge — including the no-evidence rung with only a sender wired (the
// harness-agnostic floor). A heartbeat that predates the nudge is the
// dead periodic discipline and saves nothing (the ordinal comparison:
// only an ARRIVAL after nudged_at is a response).
func (s *BoardSuite) TestPin_Lease10_SilenceOnBothChannelsWedges() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "both-silent", nil, false)

	c := s.claimOrFail(ws)
	nudger := &fakeNudger{}
	recon := s.pinRecon(board.NilLiveness{}, nudger.Send) // the floor: no observation, sender wired

	// A PRE-nudge heartbeat: extended nothing (pin 11's world) and must
	// not count as a response to a sentinel that does not exist yet.
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
		time.Now().UTC())
	s.Require().NoError(err)
	s.False(res.Extended, "no nudge, no extension — the periodic discipline is dead")

	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count())
	nid, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid)

	// Full window of silence on both channels.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageFailed, row.Stage, "silence on both channels is the machine wedge verdict")
	s.Nil(row.ClaimToken)
	s.Nil(row.Assignee)
	s.True(rowIsMarkedFailed(s, task.ID))
	s.Equal(1, s.countEvents("wedge"))
	var evidence string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT payload->>'evidence' FROM board_events WHERE action='wedge' AND item_id=$1`, task.ID).Scan(&evidence))
	s.Contains(evidence, "no post-nudge heartbeat",
		"the rung-3 verdict names the response channel it measured")
	s.Equal(1, s.countTransitions(task.ID, board.StageFailed))
}

// PIN 11 (spec §Pins 11, the item-6 amendment): a periodic heartbeat
// with NO nudge never extends the lease — the standing self-report
// discipline stays dead (liveness is platform-observed; the movement
// ladder renews). The arrival is still recorded (the wedge predicate's
// ordinal input) and the claim arbitration still applies.
func (s *BoardSuite) TestPin_Lease11_PeriodicHeartbeatExtendsNothing() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "periodic-dead", nil, false)

	c := s.claimOrFail(ws)
	before := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(before)

	// Fresh evidence, matching token, live claim — everything the OLD
	// evidence gate demanded. Still no extension: no nudge, no renewal.
	stamp := time.Now().UTC().Add(-time.Millisecond)
	res, err := s.svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
		stamp)
	s.Require().NoError(err)
	s.False(res.Extended, "no standing discipline: a heartbeat without a nudge extends nothing")
	s.NotEmpty(res.Reason, "the refusal names the repurposed semantics")

	after := s.rawTask(task.ID).LeaseExp
	s.Require().NotNil(after)
	s.WithinDuration(*before, *after, 50*time.Millisecond, "the lease is untouched")

	var hbAt *time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_heartbeat_at FROM tasks WHERE id=$1`, task.ID).Scan(&hbAt))
	s.Require().NotNil(hbAt, "the ARRIVAL is recorded — the wedge predicate's ordinal input")
	s.Equal(0, s.countEvents("heartbeat"), "an advisory heartbeat writes no audit event")
}

// TestPin_LeaseLadder_NudgeDeliveryFailureFallsBack pins the sender
// seam's error contract: a nudge that could not be ATTEMPTED (bounce,
// transport) leaves the row undecidable THIS sweep — it falls to the
// honest wall-clock path (reclaim + overlay), never a phantom nudge
// state claiming a sentinel that never landed.
func (s *BoardSuite) TestPin_LeaseLadder_NudgeDeliveryFailureFallsBack() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "delivery-fail", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{err: fmt.Errorf("pod unreachable")}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.reclaimOverlay(task.ID, 1)
	s.Equal(0, s.countEvents("nudge"), "no nudge event for a sentinel that never landed")
	nid, _ := s.nudgeState(task.ID)
	s.Empty(nid, "no phantom nudge state on the row")
}

// TestPin_LeaseLadder_FlaggedRowsSuspendTheLadder pins the RC1 scan's
// flag predicate: a pending_input-flagged row is not a ladder candidate
// even with its lease long dead and the source reporting it dead — the
// input wait suspends the claim lifecycle wholesale (the resume lane
// owns the flag's exit).
func (s *BoardSuite) TestPin_LeaseLadder_FlaggedRowsSuspendTheLadder() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "flagged-row", nil, false)

	s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.pending.set(assignee, true)
	res := s.sweep()
	s.Equal(1, res.PendingStaged)
	s.parkedPin(task.ID, board.StageInProgress, board.PendingOriginWorker)

	lives := &fakeLiveness{}
	lives.set(assignee, dead())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 hour' WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	s.rc1Sweep(recon)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the flagged row is invisible to the ladder")
	s.True(row.PendingInput)
	s.Equal(0, nudger.count())
	s.Equal(0, s.countEvents("reclaim"), "no second reclaim event (the flag predicates the scan)")
}

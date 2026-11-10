// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// reclaim_abort_pins_test.go — the S8 pins (owner ruling 2026-10-30:
// "When a lease is reclaimed, an abort should be sent to the session
// just to ensure it does stop"; the 2026-10-30 incident: healthy
// workers mid-marathon turns, lapsed at exactly 45:00, kept generating
// after reclaim — the 409 only surfaces on their next board WRITE).
//
// Every pass that definitively ends a claim now aborts the deposed
// session POST-COMMIT and audit-logs the deposal (action
// reclaim_abort: session, timestamp, the reclaim event it answers).
// The pins drive the ladder deterministically like lease_pins_test.go
// (the house DB-clock seam + per-phase failpoints), against fakes at
// the aborter/liveness/nudge seams — the production aborter rides the
// V1 interrupt route with NO bus dependency, pinned here by the
// nil-seam degradation arm (a reclaim without a seam still succeeds).
package board_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// s8Spawner mints a UNIQUE session per spawn (sess-1, sess-2, …) —
// ruling R9's one-fresh-session-per-claim shape. The suite's default
// fakeSpawner keys the id on the TASK short id, so the same task's
// second claim would reuse the first holder's id and the deposal pins
// could not discriminate the old holder from the fresh one.
type s8Spawner struct {
	mu    sync.Mutex
	n     int
	calls []string
}

func (f *s8Spawner) Spawn(_ context.Context, _ *board.Claim) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("sess-%d", f.n)
	f.calls = append(f.calls, id)
	return id, nil
}

// s8Recon builds a reconciler carrying the liveness/nudge seams AND an
// EXPLICIT aborter (per-arm isolation: s.aborter resets per test
// function, and these pins use one function per arm with subtests).
func (s *BoardSuite) s8Recon(lives board.AssigneeLivenessSource, nudger board.NudgeSender, aborter *fakeAborter) *board.Reconciler {
	return board.NewReconciler(s.pool, s.cfg,
		board.WithLivenessSource(lives),
		board.WithNudgeSender(nudger),
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
}

// s8Svc builds a claim service over the unique-mint spawner + the same
// aborter the pins observe.
func (s *BoardSuite) s8Svc(spawner *s8Spawner, aborter *fakeAborter) *board.Service {
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
}

// reclaimAbortEvent reads the row's (single) reclaim_abort audit event.
func (s *BoardSuite) reclaimAbortEvent(taskID string) (id, session, reclaimEvent string, createdAt time.Time) {
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT id, payload->>'session', payload->>'reclaim_event', created_at
		 FROM board_events WHERE action='reclaim_abort' AND item_id=$1`, taskID).
		Scan(&id, &session, &reclaimEvent, &createdAt))
	return
}

// eventActionFor resolves one board event's action by id (the pointer
// arm of the audit pin: the reclaim_abort row must point at a REAL
// reclaim/wedge event, not a dangling string).
func (s *BoardSuite) eventActionFor(id string) string {
	var action string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT action FROM board_events WHERE id=$1`, id).Scan(&action))
	return action
}

// PIN S8-1 (criterion 1 + 2 + 3, the incident's exact shape): a lapsed
// claim reclaimed into the pool → the OLD session is aborted exactly
// once, POST-COMMIT (the row is already backlog at abort time), the
// deposal is audit-logged with the session + a reclaim pointer that
// resolves + a timestamp, a duplicate sweep is an idempotent no-op, and
// a FRESH epoch's lapse aborts the FRESH holder — never re-aborting the
// old one.
func (s *BoardSuite) TestPin_S8_1_ReclaimAbortsDeposedSession() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "deposed-worker", nil, false)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := s.s8Svc(spawner, aborter)

	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Equal(task.ID, c.TaskID)
	oldHolder := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(oldHolder, "the spawn stamped the assignee")

	// At abort time the reclaim must ALREADY be committed (the delete
	// path's r2 ordering rule, now the reclaim's too): captured from
	// inside the abort hook, asserted after the sweep settles.
	var mu sync.Mutex
	var stagesAtAbort []string
	var tokensClearedAtAbort []bool
	aborter.onAbort = func(taskID, _ string) {
		row := s.rawTask(taskID)
		mu.Lock()
		defer mu.Unlock()
		stagesAtAbort = append(stagesAtAbort, row.Stage)
		tokensClearedAtAbort = append(tokensClearedAtAbort, row.ClaimToken == nil)
	}

	// Dead evidence (the session provably no longer exists): the fast
	// wall-clock reclaim in one sweep — the FIRST post-expiry arm.
	lives := &fakeLiveness{}
	lives.set(oldHolder, dead())
	nudger := &fakeNudger{}
	recon := s.s8Recon(lives, nudger.Send, aborter)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.reclaimOverlay(task.ID, 1)
	s.Equal(1, aborter.count(), "the deposed holder is aborted exactly once")
	s.Equal(task.ID+"/"+oldHolder, aborter.callsSlice()[0], "the abort names the OLD session")
	s.Equal(0, nudger.count(), "no sentinel wasted on a corpse (pin 5 parity)")
	mu.Lock()
	s.Require().Len(stagesAtAbort, 1)
	s.Equal(board.StageBacklog, stagesAtAbort[0],
		"the abort fires POST-COMMIT: the reclaim is already on the row")
	s.True(tokensClearedAtAbort[0], "the claim token is already gone at abort time")
	mu.Unlock()

	// The audit trail (S7's feed): session, timestamp, reclaim pointer.
	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, createdAt := s.reclaimAbortEvent(task.ID)
	s.Equal(oldHolder, session, "the audit row names the deposed session")
	s.Equal("reclaim", s.eventActionFor(reclaimEvent),
		"the reclaim pointer resolves to the reclaim event this deposal answers")
	s.WithinDuration(time.Now().UTC(), createdAt.UTC(), 2*time.Minute,
		"the audit row carries the deposal's timestamp")

	// Idempotency: a re-sweep of the reclaimed row changes nothing.
	s.rc1Sweep(recon)
	s.Equal(1, aborter.count(), "a duplicate sweep never re-aborts the stopped session")
	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID), "no duplicate audit row")

	// A FRESH epoch: the row re-claims to a FRESH session (R9), and its
	// own lapse aborts ITS holder — the old one is never re-aborted.
	s.clearTaskBackoff(task.ID)
	c2, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Equal(task.ID, c2.TaskID)
	freshHolder := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEqual(oldHolder, freshHolder, "the fresh claim minted a fresh session")

	lives.set(freshHolder, dead())
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.reclaimOverlay(task.ID, 2)
	s.Equal(2, aborter.count(), "the fresh epoch's deposal aborts once more")
	s.Equal(task.ID+"/"+freshHolder, aborter.callsSlice()[1],
		"the second abort names the FRESH holder — each deposal aborts its own session")
	s.Equal(2, s.countEventsFor("reclaim_abort", task.ID))
	// The fresh epoch's audit pointer resolves to a reclaim event too.
	_, _, reclaimEvent2, _ := s.reclaimAbortEvent(task.ID)
	s.Equal("reclaim", s.eventActionFor(reclaimEvent2))
}

// PIN S8-2 (criterion 1, the verdict arm): the WEDGE verdict ends the
// claim terminally — the deposed session is aborted and the audit
// pointer names the WEDGE event (the claim-ending write of that path).
func (s *BoardSuite) TestPin_S8_2_WedgeVerdictAbortsDeposedSession() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "wedged-deposed", nil, false)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := s.s8Svc(spawner, aborter)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Equal(task.ID, c.TaskID)
	holder := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(holder, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.s8Recon(lives, nudger.Send, aborter)

	// Episode: expiry → nudge → full window of silence on both channels.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count())
	s.Equal(0, aborter.count(), "a nudged (still-claimed) row aborts nobody")

	lives.set(holder, board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true, EventsAfterNudge: false})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.Equal(board.StageFailed, s.rawTask(task.ID).Stage, "the wedge verdict lands failed")
	s.Equal(1, aborter.count(), "the verdict deposes the holder — aborted")
	s.Equal(task.ID+"/"+holder, aborter.callsSlice()[0])

	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, createdAt := s.reclaimAbortEvent(task.ID)
	s.Equal(holder, session)
	s.Equal("wedge", s.eventActionFor(reclaimEvent),
		"the audit pointer names the WEDGE event — the claim-ending write of this path")
	s.WithinDuration(time.Now().UTC(), createdAt.UTC(), 2*time.Minute)
}

// PIN S8-3 (criterion 2, the never-on-live-claim guard): a LIVE claim —
// renewed by movement, by a heartbeat response, or merely nudged and
// waiting — is NEVER aborted, at any rung, across repeated expiries.
func (s *BoardSuite) TestPin_S8_3_LiveClaimNeverAborted() {
	s.Run("moving worker: renewed across expiries, never aborted", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "s8-moving", nil, false)

		spawner := &s8Spawner{}
		aborter := &fakeAborter{}
		svc := s.s8Svc(spawner, aborter)
		c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		s.Equal(task.ID, c.TaskID)
		holder := s.reloadTask(task.ID).AssigneeSession

		lives := &fakeLiveness{}
		lives.set(holder, liveMoving())
		recon := s.s8Recon(lives, func(_ context.Context, _ string, _ board.NudgePayload) error {
			s.T().Fatalf("a moving worker is never nudged")
			return nil
		}, aborter)

		for round := 1; round <= 3; round++ {
			s.expireLease(task.ID)
			s.rc1Sweep(recon)
			s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage,
				"round %d: a moving worker never lapses", round)
			s.Equal(0, aborter.count(), "round %d: a live renewed claim is never aborted", round)
			s.Equal(0, s.countEventsFor("reclaim_abort", task.ID), "round %d: no deposal audit row")
		}
	})

	s.Run("heartbeat response renews: never aborted", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "s8-response", nil, false)

		spawner := &s8Spawner{}
		aborter := &fakeAborter{}
		svc := s.s8Svc(spawner, aborter)
		c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		s.Equal(task.ID, c.TaskID)

		nudger := &fakeNudger{}
		recon := s.s8Recon(board.NilLiveness{}, nudger.Send, aborter)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		s.Equal(1, nudger.count())

		// The worker answers the sentinel FIRST — the claim survives.
		res, err := svc.Heartbeat(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "",
			time.Now().UTC())
		s.Require().NoError(err)
		s.True(res.Extended)

		s.rc1Sweep(recon)
		s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
		s.Equal(0, aborter.count(), "a response-renewed claim is never aborted")
		s.Equal(0, s.countEventsFor("reclaim_abort", task.ID))
	})

	s.Run("nudged-but-undecided claim: never aborted", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "s8-nudged", nil, false)

		spawner := &s8Spawner{}
		aborter := &fakeAborter{}
		svc := s.s8Svc(spawner, aborter)
		_, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		holder := s.reloadTask(task.ID).AssigneeSession

		lives := &fakeLiveness{}
		lives.set(holder, liveQuiet())
		nudger := &fakeNudger{}
		recon := s.s8Recon(lives, nudger.Send, aborter)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		s.Equal(1, nudger.count())
		s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the nudge holds the claim open")
		s.Equal(0, aborter.count(), "an undecided episode aborts nobody — only the END of a claim aborts")
		s.Equal(0, s.countEventsFor("reclaim_abort", task.ID))
	})
}

// PIN S8-4 (criterion 3, the no-seam degradation): without an aborter
// installed (the unwired/inert-engine shape — and the proof the feature
// carries NO hard transport dependency), the reclaim itself still
// converges byte-for-byte and no audit row is written (an event for an
// abort that cannot dispatch would lie).
func (s *BoardSuite) TestPin_S8_4_NoSeamStillReclaims() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-no-seam", nil, false)

	c := s.claimOrFail(ws) // the suite svc: spawner, NO aborter on the recon below
	s.Equal(task.ID, c.TaskID)

	recon := board.NewReconciler(s.pool, s.cfg,
		board.WithLivenessSource(board.NilLiveness{}),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)

	s.expireLease(task.ID)
	s.rc1Sweep(recon)

	s.reclaimOverlay(task.ID, 1)
	s.Equal(1, s.countEvents("reclaim"))
	s.Equal(0, s.countEvents("reclaim_abort"), "no audit row for an abort that could not dispatch")
	s.Equal(0, s.aborter.count(), "the suite aborter (unwired on this reconciler) saw nothing")
}

// PIN S8-5 (criterion 1, the in_review twin): a lapsed persona
// reviewer claim is reclaimed by the persona lapse pass — the deposed
// REVIEWER session is aborted (not the long-gone implementer), and the
// deposal is audit-logged beside the lapse.
func (s *BoardSuite) TestPin_S8_5_PersonaLapseAbortsReviewer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-reviewer-lapse", nil, false)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})
	recon := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	reviewer := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEqual(c.Assignee, reviewer, "the reviewer persona is a FRESH session")
	s.Require().NotNil(s.rawTask(task.ID).ClaimToken, "the in_review entry minted the reviewer claim")

	// The reviewer never turns; its lease lapses. The persona pass
	// reclaims — and deposes the reviewer.
	s.expireLease(task.ID)
	s.fps.armAt("persona.post", errors.New("pin: stop after the persona pass"))
	_, err = recon.Sweep(context.Background())
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "persona.post")
	s.fps.disarm("persona.post")

	s.Require().Nil(s.rawTask(task.ID).ClaimToken, "the lapse cleared the dead claim")
	s.Equal(1, aborter.count(), "the deposed reviewer is aborted exactly once")
	s.Equal(task.ID+"/"+reviewer, aborter.callsSlice()[0], "the abort names the REVIEWER session")

	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, _ := s.reclaimAbortEvent(task.ID)
	s.Equal(reviewer, session)
	s.Equal("reclaim", s.eventActionFor(reclaimEvent),
		"the audit pointer resolves to the persona lapse's reclaim event")
}

// PIN S8-5b (review round 1, finding 1 — the config-REMOVED arm): the
// persona lapse pass's config-cleared branch ends the reviewer's claim
// too (converge-to-unclaimed, no accounting) — the deposed reviewer is
// aborted and audited. Deleting the branch's deposal call must redden
// THIS pin.
func (s *BoardSuite) TestPin_S8_5b_PersonaConfigClearedAbortsReviewer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-reviewer-config-cleared", nil, false)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})
	recon := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	reviewer := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEqual(c.Assignee, reviewer)

	// The persona config is REMOVED mid-flight: the lapse pass takes the
	// config-cleared convergence branch (persona_cleared reclaim).
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{})

	s.expireLease(task.ID)
	s.fps.armAt("persona.post", errors.New("pin: stop after the persona pass"))
	_, err = recon.Sweep(context.Background())
	s.Require().Error(err)
	s.fps.disarm("persona.post")

	s.Require().Nil(s.rawTask(task.ID).ClaimToken, "the config-cleared lapse ended the claim")
	s.Equal(1, aborter.count(), "the deposed reviewer is aborted on the config-cleared arm too")
	s.Equal(task.ID+"/"+reviewer, aborter.callsSlice()[0])

	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, _ := s.reclaimAbortEvent(task.ID)
	s.Equal(reviewer, session)
	s.Equal("reclaim", s.eventActionFor(reclaimEvent), "the audit pointer resolves to the config-cleared reclaim")
}

// errAborter records the call and refuses — the delivery-degradation
// shape (a dead pod behind the interrupt route).
type errAborter struct {
	mu    sync.Mutex
	calls []string
}

func (f *errAborter) Abort(_ context.Context, taskID, assignee string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, taskID+"/"+assignee)
	return errors.New("pod unreachable")
}

func (f *errAborter) callsSlice() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// PIN S8-6 (the batch + delivery-degradation legs): N deposed holders
// in ONE sweep are ALL aborted (the mass-lapse incident shape), and an
// aborter error degrades to a warn — the reclaim, the audit trail, and
// the sweep all proceed (the 409 arbitration remains the authority).
func (s *BoardSuite) TestPin_S8_6_BatchAndDeliveryErrorLegs() {
	s.Run("N deposed in one sweep — all aborted", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		first := s.seedTask(e, "s8-batch-a", nil, false)
		second := s.seedTask(e, "s8-batch-b", nil, false)

		spawner := &s8Spawner{}
		aborter := &fakeAborter{}
		svc := s.s8Svc(spawner, aborter)
		c1, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		c2, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		holderA := s.reloadTask(c1.TaskID).AssigneeSession
		holderB := s.reloadTask(c2.TaskID).AssigneeSession

		lives := &fakeLiveness{}
		lives.set(holderA, dead())
		lives.set(holderB, dead())
		recon := s.s8Recon(lives, nil, aborter)

		s.expireLease(first.ID)
		s.expireLease(second.ID)
		s.rc1Sweep(recon)

		s.reclaimOverlay(first.ID, 1)
		s.reclaimOverlay(second.ID, 1)
		s.Equal(2, aborter.count(), "the one-sweep batch aborts EVERY deposed holder")
		got := aborter.callsSlice()
		s.ElementsMatch([]string{first.ID + "/" + holderA, second.ID + "/" + holderB}, got)
		s.Equal(2, s.countEvents("reclaim_abort"), "one audit row per deposal")
	})

	s.Run("aborter error degrades to a warn — nothing else affected", func() {
		ws := s.seedWorkspace()
		p := s.seedProject(ws, 4)
		e := s.seedEpic(p, board.EpicActive)
		task := s.seedTask(e, "s8-abort-err", nil, false)

		spawner := &s8Spawner{}
		aborter := &errAborter{}
		svc := board.NewService(s.pool, s.cfg,
			board.WithWorkerSpawner(spawner.Spawn),
			board.WithPRDriftSource(s.drift),
			board.WithPendingInputSource(s.pending),
			board.WithFailpoint(s.fps.fp),
		)
		c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
		s.Equal(task.ID, c.TaskID)
		holder := s.reloadTask(task.ID).AssigneeSession

		lives := &fakeLiveness{}
		lives.set(holder, dead())
		recon := board.NewReconciler(s.pool, s.cfg,
			board.WithLivenessSource(lives),
			board.WithWorkerAborter(aborter.Abort),
			board.WithPRDriftSource(s.drift),
			board.WithPendingInputSource(s.pending),
			board.WithFailpoint(s.fps.fp),
		)

		s.expireLease(task.ID)
		s.rc1Sweep(recon)

		s.reclaimOverlay(task.ID, 1)
		s.True(rowIsMarkedFailed(s, task.ID), "the reclaim itself is unaffected by the delivery error")
		s.Equal([]string{task.ID + "/" + holder}, aborter.callsSlice(), "the abort was ATTEMPTED")
		s.Equal(1, s.countEventsFor("reclaim_abort", task.ID),
			"the deposal audit row stands — the DELIVERY failed, not the decision")
	})
}

// PIN S8-7 (the rollback guard, mechanical): a fault between the
// deposal recording and the commit rolls the WHOLE reclaim back — the
// abort must NOT fire on a claim that never ended, and the next sweep
// converges with exactly one abort. This is the post-commit-only guard
// made testable (the S8 twin of the delete path's failed-transaction
// pin).
func (s *BoardSuite) TestPin_S8_7_FaultedReclaimNeverAborts() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-rollback", nil, false)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := s.s8Svc(spawner, aborter)
	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Equal(task.ID, c.TaskID)
	holder := s.reloadTask(task.ID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(holder, dead())
	recon := s.s8Recon(lives, nil, aborter)

	// The fault lands between the deposal recording and the commit.
	s.expireLease(task.ID)
	s.fps.armAt("rc1.precommit", errors.New("pin: fault before the reclaim commit"))
	_, err = recon.Sweep(context.Background())
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "rc1.precommit")
	s.fps.disarm("rc1.precommit")

	s.Equal(0, aborter.count(), "a rolled-back reclaim NEVER aborts — the claim never ended")
	s.Equal(0, s.countEvents("reclaim"), "the reclaim rolled back with the audit rows")
	s.Equal(0, s.countEvents("reclaim_abort"))
	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the row is untouched by the faulted pass")
	s.Require().NotNil(row.ClaimToken)

	// The next sweep converges — exactly one abort, exactly one trail.
	s.rc1Sweep(recon)
	s.reclaimOverlay(task.ID, 1)
	s.Equal(1, aborter.count(), "the converged reclaim aborts exactly once")
	s.Equal(1, s.countEvents("reclaim_abort"))
}

// PIN S8-8 (review round 1, finding 2 — drift-done): RC2's merged-PR
// convergence ends the in_review row's LIVE claim (the persona reviewer
// deposed by external truth) — aborted post-commit with the audit
// pointer naming the drift_done event. The implementer (whose own
// forward move ended ITS claim long before) is never touched.
func (s *BoardSuite) TestPin_S8_8_DriftDoneAbortsDeposedReviewer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-drift-done", []string{"pr:88"}, true)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})
	recon := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	reviewer := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEqual(c.Assignee, reviewer, "the persona mint stamped the reviewer")
	s.Require().NotNil(s.rawTask(task.ID).ClaimToken, "the in_review row holds a live claim")

	s.drift.set(88, board.PRState{Merged: true, URL: "https://git.example/88"})
	_, err = recon.Sweep(context.Background())
	s.Require().NoError(err)

	s.Equal(board.StageDone, s.rawTask(task.ID).Stage, "the drift convergence lands done")
	s.Equal(1, aborter.count(), "the deposed reviewer is aborted")
	s.Equal(task.ID+"/"+reviewer, aborter.callsSlice()[0])
	s.NotContains(aborter.callsSlice()[0], c.Assignee, "the implementer's own forward move was never a deposal")

	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, _ := s.reclaimAbortEvent(task.ID)
	s.Equal(reviewer, session)
	s.Equal("drift_done", s.eventActionFor(reclaimEvent), "the audit pointer names the drift_done ending")
}

// PIN S8-9 (finding 2 — closed-unmerged re-entry): RC2's re-entry mint
// supersedes the in_review claim (fresh token for a fresh worker) — the
// OLD reviewer is aborted post-commit with the audit pointer naming the
// drift_reentry event, and the abort precedes the fresh spawn.
func (s *BoardSuite) TestPin_S8_9_ReentryAbortsDeposedReviewer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "s8-reentry", []string{"pr:89"}, true)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})
	recon := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
	)

	c, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	reviewer := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEqual(c.Assignee, reviewer)

	s.drift.set(89, board.PRState{Closed: true, URL: "https://git.example/89"})
	_, err = recon.Sweep(context.Background())
	s.Require().NoError(err)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "the re-entry lands in_progress with a fresh claim")
	s.Require().NotNil(row.ClaimToken)
	s.NotEqual(reviewer, row.Assignee, "the fresh worker supersedes the reviewer's stamp")

	s.Equal(1, aborter.count(), "the superseded reviewer is aborted")
	s.Equal(task.ID+"/"+reviewer, aborter.callsSlice()[0])
	s.Equal(1, s.countEventsFor("reclaim_abort", task.ID))
	_, session, reclaimEvent, _ := s.reclaimAbortEvent(task.ID)
	s.Equal(reviewer, session)
	s.Equal("drift_reentry", s.eventActionFor(reclaimEvent), "the audit pointer names the re-entry ending")
}

// PIN S8-10 (finding 2 — the done-settle's anchored reviews): when a
// subject reaches done, the #1832 settle COMPLETES its anchored review
// tasks — a review task holding a LIVE claim is deposed by the settle
// (platform-initiated: the SUBJECT's completion, not the reviewer's own
// write) and aborted post-commit with the audit pointer naming the
// review_complete event. The SUBJECT's own reviewer (its claim ended by
// the manager's human move) is the exclusion: human-initiated moves
// never abort.
func (s *BoardSuite) TestPin_S8_10_DoneSettleAbortsAnchoredReviewer() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	subject := s.seedTask(e, "s8-settle-subject", nil, true)
	review := s.seedTask(e, "s8-settle-review", []string{"review-of:" + subject.ShortID}, true)

	spawner := &s8Spawner{}
	aborter := &fakeAborter{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithWorkerAborter(aborter.Abort),
		board.WithRoleDirectory(personaRoleDirectory()),
	)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	// The subject: worker → in_review (its own reviewer claim mints).
	cs, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(subject.ID, cs.TaskID)
	_, err = svc.MoveStage(context.Background(), s.agent(cs.Assignee, cs.ClaimToken), "", cs.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	subjectReviewer := s.reloadTask(subject.ID).AssigneeSession

	// The anchored review task: worker → in_review (ITS reviewer claim).
	cr, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(review.ID, cr.TaskID)
	_, err = svc.MoveStage(context.Background(), s.agent(cr.Assignee, cr.ClaimToken), "", cr.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	reviewReviewer := s.reloadTask(review.ID).AssigneeSession
	s.Require().NotEqual(cs.Assignee, reviewReviewer)

	// The manager completes the subject (a HUMAN move — the subject's
	// own reviewer is NOT a deposal; the settle's completion of the
	// anchored review task IS platform-initiated).
	_, err = svc.MoveStage(context.Background(), s.owner(), subject.ID, "", board.StageDone, "https://git.example/settle", "")
	s.Require().NoError(err)

	s.Equal(board.StageDone, s.rawTask(review.ID).Stage, "the anchored review completed with its subject")
	s.Equal(1, aborter.count(), "exactly the anchored review's claimant is aborted")
	s.Equal(review.ID+"/"+reviewReviewer, aborter.callsSlice()[0])
	s.NotEqual(subject.ID+"/"+subjectReviewer, aborter.callsSlice()[0],
		"the subject's own reviewer ended by the human move — never aborted (the exclusion)")

	s.Equal(1, s.countEventsFor("reclaim_abort", review.ID))
	_, session, reclaimEvent, _ := s.reclaimAbortEvent(review.ID)
	s.Equal(reviewReviewer, session)
	s.Equal("review_complete", s.eventActionFor(reclaimEvent), "the audit pointer names the review_complete ending")
	s.Equal(0, s.countEventsFor("reclaim_abort", subject.ID), "no deposal audit for the human-ended subject claim")
}

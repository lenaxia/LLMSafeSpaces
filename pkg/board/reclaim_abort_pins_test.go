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

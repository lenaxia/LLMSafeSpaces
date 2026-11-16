// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// wrapup_engine_integration_test.go — the wrapup-engine story's born-red
// pins (epic wrap-up review, low-level design §§1b/2/3/7; high-level D1-D3,
// D9). The engine seam: an epic with wrap_up_review enabled that DRAINS
// (all kind='story' rows done) mints ONE synthetic wrap-up review row born
// claimed by the wrap-up reviewer persona and DEFERS epic auto-completion
// while it flies; a disabled epic is byte-identical to today (D9).
//
// Born-red mapping (README-LLM rule 0 — these pins land BEFORE the
// implementation and redden against the pre-wrapup engine):
//
//   - [0] disabled-epic byte-identity — the regression pin (green before
//     AND after; it pins the plain branch against regressions).
//   - [1] enabled drain mints exactly once + in-flight defers + concurrent
//     double-done serializes (RED before: the plain check completes the
//     epic, no row is ever minted).
//   - [2] self-exclusion + dispatch carve-outs (RED before: ClaimNext
//     serves a backlog wrap-up row; TaskStart/dependency inserts accept
//     wrap-up endpoints).
//   - [3] claim-lifecycle carve-outs: reconciler re-claim with the
//     wrap-up persona, rearm → in_review, delete clears the pointer and
//     the next drain re-mints.
//   - [4] unresolvable reviewer role at mint defers (no row, warn event,
//     re-fires) — never a role-less spawn.
package board_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// wrapupReviewerRole joins the standard two-role suite vocabulary as the
// seeded wrap-up reviewer (slug wrapup-reviewer — the directory shape a
// healthy deployment renders after startup seeding).
var wrapupReviewerRole = board.AgentRoleRef{
	ID: "44444444-4444-4444-4444-444444444444", Slug: "wrapup-reviewer",
	Name: "Wrap-up Reviewer", Scope: "platform",
}

// wrapupDir is the healthy vocabulary for the wrap-up pins.
func wrapupDir() *fakeRoleDirectory {
	return &fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole, buildRole, wrapupReviewerRole}}
}

// wrapSvc builds a per-test service whose role directory carries the
// wrap-up reviewer (the suite service's does not — wrap-up pins resolve
// their own personas).
func (s *BoardSuite) wrapSvc(sp *fakeSpawner) *board.Service {
	if sp == nil {
		sp = &fakeSpawner{}
	}
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(sp.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithRoleDirectory(wrapupDir()))
}

// wrapRecon builds a reconciler whose internal service resolves the
// wrap-up persona too (the re-claim pass + its post-commit spawns).
func (s *BoardSuite) wrapRecon(sp *fakeSpawner) *board.Reconciler {
	if sp == nil {
		sp = &fakeSpawner{}
	}
	return board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(sp.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithRoleDirectory(wrapupDir()))
}

// seedWrapEpic creates an epic through the service with the wrap-up flag
// set per enabled, then activates it (the real path; the activation
// recheck runs the seam harmlessly on an empty epic).
func (s *BoardSuite) seedWrapEpic(svc *board.Service, p *board.Project, enabled bool) *board.Epic {
	ctx := context.Background()
	e, err := svc.CreateEpic(ctx, s.owner(), p.ID, "",
		board.EpicInput{Title: "wrap-epic-" + p.ShortID, Outcome: "ship the wrap-up seam", WrapUpReview: &enabled}, "")
	s.Require().NoError(err)
	_, err = svc.EpicActivate(ctx, s.owner(), e.ID, "", false, "")
	s.Require().NoError(err)
	return s.reloadEpic(e.ID)
}

// claimAndComplete walks ONE story backlog→done through the real engine
// path (claim → guarded →done with the live token) — the transition whose
// doneCascade runs the wrap-up seam.
func (s *BoardSuite) claimAndComplete(svc *board.Service, wsID, shortID string) {
	ctx := context.Background()
	c, err := svc.ClaimNext(ctx, "worker-"+wsID, wsID)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.Require().False(c.Escalated, "test expected a grant, got escalation")
	s.Require().Equal(shortID, c.ShortID, "FIFO claim order decided the row")
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", shortID,
		board.StageDone, "https://ev.test/"+shortID, "")
	s.Require().NoError(err)
}

// countWrapupRows counts kind='wrapup' task rows (the mint census).
func (s *BoardSuite) countWrapupRows() int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE kind='wrapup'`).Scan(&n))
	return n
}

// soleWrapupTask resolves the single live wrap-up row (require: exactly
// one exists).
func (s *BoardSuite) soleWrapupTask(svc *board.Service) *board.Task {
	s.Require().Equal(1, s.countWrapupRows(), "exactly one wrap-up row expected")
	var id string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT id FROM tasks WHERE kind='wrapup'`).Scan(&id))
	t, err := svc.GetTask(context.Background(), id, "")
	s.Require().NoError(err)
	return t
}

// epicWrapupState reads the raw wrap-up lifecycle columns.
type epicWrapupState struct {
	ReviewOn    bool
	TaskID      *string
	Count       int
	AckPending  bool
	LastWrapped *string
}

func (s *BoardSuite) epicWrapup(id string) epicWrapupState {
	var st epicWrapupState
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT wrap_up_review, wrapup_task_id::text, wrapup_count, wrapup_ack_pending,
		        last_wrapped_at::text FROM epics WHERE id=$1`, id).
		Scan(&st.ReviewOn, &st.TaskID, &st.Count, &st.AckPending, &st.LastWrapped))
	return st
}

// reopenStoryToInProgress raw-SQLs a done story back to an UNCLAIMED
// in_progress row with a LIVE lease (the manager reopen fixture; done is
// terminal for workers — the raw reset is the test seam for re-triggering
// the drain). The live lease keeps rc1's unclaimed-in_progress parking
// sweep from swallowing the row between the reopen and the →done write
// (sweep-proof: rc1 only parks NULL/expired leases).
func (s *BoardSuite) reopenStoryToInProgress(taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET stage='in_progress', claim_token=NULL, lease_expires_at=now()+interval '1 hour',
		 nudge_id=NULL, nudged_at=NULL WHERE id=$1`, taskID)
	s.Require().NoError(err)
}

// managerCompleteUnclaimed moves an UNCLAIMED in_progress row →done by
// the owner (the guard's unclaimed shape) — a second real seam trigger.
func (s *BoardSuite) managerCompleteUnclaimed(svc *board.Service, shortID string) {
	_, err := svc.MoveStage(context.Background(), s.owner(), "", shortID,
		board.StageDone, "https://ev.test/re-"+shortID, "")
	s.Require().NoError(err)
}

// --- [0] Disabled epics are byte-identical (D9 regression pin) ---------------

func (s *BoardSuite) TestWrapUp_DisabledEpicIsByteIdentical() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, false)
	require.False(s.T(), e.WrapUpReview, "the flag rides the epic read (default off)")

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)

	s.claimAndComplete(svc, ws, s1.ShortID)
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID),
		"one story open: today's semantics — no completion, no mint")

	s.claimAndComplete(svc, ws, s2.ShortID)
	require.Equal(s.T(), board.EpicCompleted, s.epicStatus(e.ID),
		"last story done completes the epic (today's statement, verbatim)")
	require.Zero(s.T(), s.countWrapupRows(), "a disabled epic NEVER mints a wrap-up row")
	require.Zero(s.T(), s.epicWrapup(e.ID).Count, "wrapup_count stays 0")
	require.Nil(s.T(), s.epicWrapup(e.ID).TaskID, "no in-flight pointer")
}

// --- [1] Enabled drain: exactly one mint, in-flight defers, concurrency ------

func (s *BoardSuite) TestWrapUp_EnabledDrainMintsOnceAndDefers() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	sp := &fakeSpawner{}
	svc := s.wrapSvc(sp)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)

	s.claimAndComplete(svc, ws, s1.ShortID)
	require.Equal(s.T(), 0, s.countWrapupRows(), "one story still open: not drained, no mint")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID))

	// The triggering →done: the drain mints in the SAME transaction.
	s.claimAndComplete(svc, ws, s2.ShortID)

	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID),
		"an enabled epic NEVER auto-completes through the drain seam while the wrap-up flies")

	w := s.soleWrapupTask(svc)
	require.Equal(s.T(), board.StageInReview, w.Stage, "wrap-up rows are born in_review")
	require.Equal(s.T(), "wrapup", w.Kind, "the engine-owned row-class marker")
	require.Contains(s.T(), w.Tags, "wrapup")
	require.Contains(s.T(), w.Title, "Epic wrap-up review: ", "the minted title shape")
	require.NotEmpty(s.T(), w.ClaimToken, "the reviewer persona claim is minted in the same transaction")
	require.Equal(s.T(), wrapupReviewerRole.ID, w.ClaimPersona,
		"claim_persona stamps the canonicalized wrap-up reviewer role id")
	require.Len(s.T(), w.AcceptanceCriteria, 3, "the engine-seeded §5 criteria ride the row")
	require.Contains(s.T(), w.Description, "COVERAGE WINDOW", "the charter renders the window (minimal inline render)")
	require.Contains(s.T(), w.Description, e.Outcome, "the charter carries the epic's outcome verbatim")

	st := s.epicWrapup(e.ID)
	require.NotNil(s.T(), st.TaskID, "the in-flight pointer is set")
	require.Equal(s.T(), w.ID, *st.TaskID)
	require.Zero(s.T(), st.Count, "wrapup_count advances at CLOSE (the verdict story's settle), not at mint")

	var mintEvents int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='wrapup_mint' AND id='wrapup:mint:'||$1`,
		w.ID).Scan(&mintEvents))
	require.Equal(s.T(), 1, mintEvents, "the deterministic wrapup:mint:<taskID> event row")

	// The spawn rode the post-commit nudge: the wrap-up row's fresh
	// reviewer was spawned (spawnFor resolves the wrap-up persona).
	require.Contains(s.T(), sp.callsSlice(), w.ShortID,
		"the mint's post-commit nudge spawned the wrap-up reviewer")

	// The in-flight pointer defers a re-triggered drain: reopen s2 and
	// complete it again — still exactly one wrap-up row.
	s.reopenStoryToInProgress(s2.ID)
	s.managerCompleteUnclaimed(svc, s2.ShortID)
	require.Equal(s.T(), 1, s.countWrapupRows(), "in-flight wrap-up defers the drain (no second mint)")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID))
}

func (s *BoardSuite) TestWrapUp_ConcurrentDoubleDoneSerializesToOneMint() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)

	// Pre-claim both rows, then race ONLY the →done writes so the epic
	// row lock (the seam's FOR UPDATE) is what serializes the drains.
	ctx := context.Background()
	claims := make([]*board.Claim, 2)
	for i, short := range []string{s1.ShortID, s2.ShortID} {
		c, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
		s.Require().NoError(err)
		s.Require().NotNil(c.Task)
		claims[i] = c
		_ = short
	}
	s.Require().Equal(s1.ID, claims[0].TaskID)
	s.Require().Equal(s2.ID, claims[1].TaskID)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range claims {
		wg.Add(1)
		go func(i int, c *board.Claim) {
			defer wg.Done()
			_, errs[i] = svc.MoveStage(context.Background(),
				s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
				board.StageDone, fmt.Sprintf("https://ev.test/race-%d", i), "")
		}(i, c)
	}
	wg.Wait()
	require.NoError(s.T(), errs[0])
	require.NoError(s.T(), errs[1])

	require.Equal(s.T(), 1, s.countWrapupRows(),
		"concurrent drains serialize on the epic FOR UPDATE: exactly ONE wrap-up row")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID), "the epic defers completion while it flies")
	require.NotNil(s.T(), s.epicWrapup(e.ID).TaskID)
}

// --- [2] Self-exclusion + dispatch carve-outs ---------------------------------

func (s *BoardSuite) TestWrapUp_SelfExclusionAndDispatchRefusals() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w := s.soleWrapupTask(svc)

	// A done wrap-up row + an open story ≠ drained: reopen s2, sweep
	// (rc4Net re-runs the seam off the move_stage events) — no mint, no
	// completion.
	s.reopenStoryToInProgress(s2.ID)
	s.sweep()
	require.Equal(s.T(), 1, s.countWrapupRows(), "an open story holds the epic: not drained")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID))

	// Manager-raw-done on the wrap-up row (criteria force-met): the row
	// lands done but the pointer stays (no verdict settle — §4's
	// fail-visible posture), and the wrap-up row itself never counts in
	// the drain math: completing s2 again defers on the pointer, never
	// completes the epic through a wrap-up-only "drain".
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET acceptance_criteria='[{"text":"c1","met":true},{"text":"c2","met":true},{"text":"c3","met":true}]'::jsonb
		WHERE id=$1`, w.ID)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.owner(), "", w.ShortID, board.StageDone, "https://ev.test/wrapup-raw-done", "")
	s.Require().NoError(err, "the manager raw-done stays legal (existing manager-grade powers)")

	s.managerCompleteUnclaimed(svc, s2.ShortID)
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID),
		"the done wrap-up row never counts in completion math (self-exclusion) and the parked pointer defers")
	require.Equal(s.T(), 1, s.countWrapupRows(), "no fresh mint while the pointer parks")

	// ClaimNext never serves kind='wrapup': park the wrap-up row into
	// backlog shape (the manager backlog exit's posture) — no candidate.
	_, err = s.pool.Exec(ctx, `
		UPDATE tasks SET stage='backlog', claim_token=NULL, lease_expires_at=NULL WHERE id=$1`, w.ID)
	s.Require().NoError(err)
	s.expectNoCandidateOn(svc, ws)

	// TaskStart refuses wrap-up targets loud.
	_, err = svc.TaskStart(ctx, s.owner(), w.ID, "", "")
	s.Require().Error(err, "TaskStart refuses kind='wrapup' targets — engine-driven rows only")
	require.Contains(s.T(), err.Error(), "wrap-up")

	// Dependency inserts on wrap-up endpoints refuse (edge-free rows).
	s.reopenStoryToInProgress(s1.ID)
	err = svc.AddDependency(ctx, s.owner(), s1.ID, "", w.ShortID, "")
	s.Require().Error(err, "a wrap-up row cannot be a dependency endpoint")
	err = svc.AddDependency(ctx, s.owner(), w.ID, "", s1.ShortID, "")
	s.Require().Error(err, "a wrap-up row cannot be a dependency source")
}

// expectNoCandidate asserts the empty-claim contract on a specific service.
func (s *BoardSuite) expectNoCandidateOn(svc *board.Service, wsID string) {
	_, err := svc.ClaimNext(context.Background(), "worker-"+wsID, wsID)
	s.Require().ErrorIs(err, board.ErrNoCandidate)
}

// --- [3] Claim lifecycle carve-outs (a wrap-up claim is a claim, D3) ----------

func (s *BoardSuite) TestWrapUp_ReviewerLapseReclaimsViaReconciler() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	sp := &fakeSpawner{}
	svc := s.wrapSvc(sp)
	recon := s.wrapRecon(sp)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	w := s.soleWrapupTask(svc)
	require.NotEmpty(s.T(), w.ClaimToken, "born claimed by the mint's persona claim")

	// The reviewer dies (lease expiry): the lapse pass runs the #1813
	// ladder; the re-arm pass re-claims with the WRAP-UP persona (the
	// same frozen charter — the description is on the row).
	s.expireLease(w.ID)
	s.sweepOn(recon)
	row := s.rawTask(w.ID)
	require.False(s.T(), row.PendingInput, "the wrap-up claim keeps the in_review stage through the lapse")
	var failures int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT failure_count FROM tasks WHERE id=$1`, w.ID).Scan(&failures))
	require.Equal(s.T(), 1, failures, "the persona lapse counted the attempt failure")

	s.clearTaskBackoff(w.ID)
	s.sweepOn(recon)
	w2 := s.reloadTask(w.ID)
	require.Equal(s.T(), board.StageInReview, w2.Stage)
	require.NotEmpty(s.T(), w2.ClaimToken, "the re-arm pass re-claimed the wrap-up row")
	require.Equal(s.T(), wrapupReviewerRole.ID, w2.ClaimPersona,
		"the reconciler's in_review pass resolves the WRAP-UP persona for wrap-up rows (not the stage persona — the harness opts out)")
	require.Equal(s.T(), w.Description, w2.Description, "the charter is frozen on the row (same window for the fresh reviewer)")
	require.Contains(s.T(), sp.callsSlice(), w.ShortID, "the re-claim spawned a fresh reviewer")
}

func (s *BoardSuite) TestWrapUp_RearmReturnsFailedWrapupToInReview() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	w := s.soleWrapupTask(svc)

	// The attempts-cap terminus (the re-arm pass's escalation arm).
	_, err := s.pool.Exec(context.Background(), `
		UPDATE tasks SET stage='failed', pending_origin='escalated', pending_input=false,
			claim_token=NULL, lease_expires_at=NULL, failure_count=3 WHERE id=$1`, w.ID)
	s.Require().NoError(err)

	out, err := svc.RearmTask(context.Background(), s.owner(), w.ID, "", "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageInReview, out.Stage,
		"a failed wrap-up row re-arms to in_review (the reconciler pass re-claims) — NOT backlog")
	require.Equal(s.T(), w.ID, *s.epicWrapup(e.ID).TaskID,
		"the epic's in-flight pointer survives the re-arm (it is still the wrap-up in flight)")
}

func (s *BoardSuite) TestWrapUp_DeleteInFlightClearsPointerAndReMints() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w := s.soleWrapupTask(svc)

	_, err := svc.DeleteTask(context.Background(), s.owner(), w.ID, "", false, "")
	s.Require().NoError(err)
	require.Nil(s.T(), s.epicWrapup(e.ID).TaskID,
		"deleting the in-flight row clears the pointer (FK floor + service event)")
	require.Equal(s.T(), 1, s.countEvents("wrapup_pointer_cleared"), "the service-level pointer-clear event")

	// The next drain re-mints.
	s.reopenStoryToInProgress(s2.ID)
	s.managerCompleteUnclaimed(svc, s2.ShortID)
	w2 := s.soleWrapupTask(svc)
	require.NotEqual(s.T(), w.ID, w2.ID, "a FRESH wrap-up row was minted")
	require.Equal(s.T(), w2.ID, *s.epicWrapup(e.ID).TaskID)
}

// --- [4] Mint defers on an unresolvable reviewer (fail-closed) ----------------

// TestWrapUp_ConsecutiveCapArmsAckWait pins the seam's recursion
// circuit-breaker (§2 steps 6-7): a drained enabled epic at
// wrapup_count >= WrapUpConsecutiveCap arms wrapup_ack_pending with
// exactly ONE wrapup_ack_required event and mints NOTHING; the armed
// flag defers every later drain (no double-event, no mint) until the
// human ack verb (wrapup-gap-recursion's) clears it.
func (s *BoardSuite) TestWrapUp_ConsecutiveCapArmsAckWait() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	require.Equal(s.T(), 1, s.countWrapupRows(), "the healthy drain minted wrap-up #1")

	// Simulate the close having happened through the cap's ledger (the
	// verdict story owns the real settle): count at the cap, in-flight
	// pointer cleared.
	capN := svc.Config().WrapUpConsecutiveCap
	_, err := s.pool.Exec(context.Background(),
		`UPDATE epics SET wrapup_count=$2, wrapup_task_id=NULL WHERE id=$1`, e.ID, capN)
	s.Require().NoError(err)

	// The next drain ARMS ack-wait: no mint, the flag set, one event.
	s.reopenStoryToInProgress(s1.ID)
	s.managerCompleteUnclaimed(svc, s1.ShortID)
	require.Equal(s.T(), 1, s.countWrapupRows(), "at the cap the drain mints NOTHING")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID), "the epic parks in ack-wait, never completes past the review")
	st := s.epicWrapup(e.ID)
	require.True(s.T(), st.AckPending, "wrapup_ack_pending armed")
	require.Equal(s.T(), 1, s.countEvents("wrapup_ack_required"), "exactly one wrapup_ack_required event")

	// The ARMED flag defers later drains: no second event, no mint, the
	// guarded arm write is idempotent.
	s.reopenStoryToInProgress(s1.ID)
	s.managerCompleteUnclaimed(svc, s1.ShortID)
	require.Equal(s.T(), 1, s.countWrapupRows(), "an armed ackPending drain still mints nothing")
	require.Equal(s.T(), 1, s.countEvents("wrapup_ack_required"), "no double-event on the re-drain")
	require.True(s.T(), s.epicWrapup(e.ID).AckPending, "the flag holds until the human ack verb clears it")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID))
}

func (s *BoardSuite) TestWrapUp_UnresolvedReviewerDefersMint() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	// The directory lacks the wrap-up reviewer (role deleted
	// post-config) AND no override resolves: the default persona's slug
	// has nothing to canonicalize to.
	blind := board.NewService(s.pool, s.cfg,
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole, buildRole}}))
	e := s.seedWrapEpic(blind, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(blind, ws, s1.ShortID)
	s.claimAndComplete(blind, ws, s2.ShortID)

	require.Zero(s.T(), s.countWrapupRows(), "unresolvable reviewer → NO row minted (never a role-less spawn)")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID), "the epic defers completion — it never silently completes past the review")
	require.GreaterOrEqual(s.T(), s.countEvents("wrapup_mint_deferred"), 1,
		"the deferred mint is audible (wrapup_mint_deferred event)")
	require.Nil(s.T(), s.epicWrapup(e.ID).TaskID, "no pointer without a row")

	// The drain RE-FIRES on the next qualifying transition once the
	// vocabulary heals (the seeded default makes this the live shape).
	s.reopenStoryToInProgress(s2.ID)
	healed := s.wrapSvc(nil)
	_, err := healed.MoveStage(ctx, s.owner(), "", s2.ShortID,
		board.StageDone, "https://ev.test/healed", "")
	s.Require().NoError(err)
	require.Equal(s.T(), 1, s.countWrapupRows(), "the healed vocabulary mints on the next drain")
	require.Equal(s.T(), board.EpicActive, s.epicStatus(e.ID))
}

// --- The epic flag through the service + the read projection -------------------

func (s *BoardSuite) TestWrapUp_FlagThroughCreateUpdateAndReadProjection() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)

	off := false
	on := true
	e, err := svc.CreateEpic(ctx, s.owner(), p.ID, "",
		board.EpicInput{Title: "flag-epic", Outcome: "o", WrapUpReview: &off}, "")
	s.Require().NoError(err)
	require.False(s.T(), e.WrapUpReview)

	_, err = svc.UpdateEpic(ctx, s.owner(), e.ID, "",
		board.EpicInput{Title: "flag-epic", Outcome: "o", WrapUpReview: &on}, "")
	s.Require().NoError(err)
	require.True(s.T(), s.reloadEpic(e.ID).WrapUpReview, "PATCH true lands")

	// nil = unchanged on PATCH.
	_, err = svc.UpdateEpic(ctx, s.owner(), e.ID, "",
		board.EpicInput{Title: "flag-epic", Outcome: "o"}, "")
	s.Require().NoError(err)
	require.True(s.T(), s.reloadEpic(e.ID).WrapUpReview, "nil keeps the stored flag")

	// The read projection renders the in-flight pointer as the wrap-up
	// row's SHORT id (join at read time, never denormalized).
	_, err = svc.EpicActivate(ctx, s.owner(), e.ID, "", false, "")
	s.Require().NoError(err)
	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	w := s.soleWrapupTask(svc)
	got := s.reloadEpic(e.ID)
	require.NotNil(s.T(), got.WrapUpTaskID)
	require.Equal(s.T(), w.ShortID, *got.WrapUpTaskID,
		"the epic read renders the live wrap-up short id")
	require.Equal(s.T(), w.ID, *s.epicWrapup(e.ID).TaskID, "the stored pointer is the UUID (identity)")
}

// sweepOn runs one reconciler pass on the given reconciler (the per-test
// service/directory shape).
func (s *BoardSuite) sweepOn(recon *board.Reconciler) {
	_, err := recon.Sweep(context.Background())
	s.Require().NoError(err)
}

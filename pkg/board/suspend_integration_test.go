// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// suspend_integration_test.go — the workspace-suspension freeze pins
// (the 2026-11-13/14 recurring incident: rate-limit suspensions lapsed
// every worker claim, attempts burned to 3/3, stories escalated, the
// ready set drained, manual rearm sweep every time).
//
// The pinned contract (suspend.go):
//
//   - Pin A  suspension mid-claim freezes: the claim is renewed in
//     place, zero attempt/failure increments, no escalation, no nudge;
//     the lease is future-armed so the post-resume sweep is a no-op —
//     work continues with NO manual rearm.
//   - Pin B  genuine lapse while RUNNING still reclaims exactly as
//     today (the regression pin — the freeze never overreaches to a
//     workspace the source does not report suspended).
//   - Pin C  rapid suspend/resume cycles are idempotent: no double
//     increments, no double requeue, convergence to the frozen shape.
//   - Pin D  the claim choke point: a suspended workspace grants
//     nothing (ClaimNext quiet no-grant, TaskStart loud 409) and the
//     dispatch drive spawns nothing into it; after resume both feed.
//   - Pin E  persona claims (in_review) freeze the same way, and the
//     re-arm pass (mint AND escalation arms) skips suspended
//     workspaces.
//   - Pin N  the nil seam is the inert contract: no source installed ⇒
//     a lapsed claim reclaims even when the workspace "is" suspended
//     (today's semantics byte-identically).

import (
	"context"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeSuspendSource scripts the freeze seam: the suspended set is
// settable per phase of the test (suspend → resume cycles).
type fakeSuspendSource struct {
	mu  sync.Mutex
	set map[string]bool
}

func (f *fakeSuspendSource) SuspendedWorkspaces(_ context.Context) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.set) == 0 {
		return nil
	}
	return f.set
}

func (f *fakeSuspendSource) setSuspended(ws ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = make(map[string]bool, len(ws))
	for _, w := range ws {
		f.set[w] = true
	}
}

func (f *fakeSuspendSource) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = nil
}

// suspendSvc builds the suite service + reconciler with the freeze seam
// installed (SetupTest's option set verbatim, one addition) — the
// reassignment pattern of the persona suite (s.recon feeds s.sweep).
func (s *BoardSuite) suspendSvc(src board.SuspendSource) *board.Service {
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
		board.WithSuspendSource(src),
	)
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
		board.WithSuspendSource(src),
	)
	return svc
}

// Pin A — suspension mid-claim: freeze, zero attempt burn, clean resume.
func (s *BoardSuite) TestSuspend_FreezeHoldsClaim_NoAttemptBurn() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "frozen-mid-claim", nil, false)

	src := &fakeSuspendSource{}
	svc := s.suspendSvc(src)
	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	attempts := s.rawTask(task.ID).Attempts // the claim's own increment (legitimate)
	s.Require().Greater(attempts, 0)
	token := s.rawTask(task.ID).ClaimToken
	s.Require().NotNil(token)

	// Suspend mid-claim; the lease lapses while suspended; sweeps run.
	// (res.Reclaims counts everything that "landed" — renewals included,
	// the r1-C5 applied counter — so the reclaim proof is the EVENT
	// count and the row state below, not the counter.)
	src.setSuspended(ws)
	for i := 0; i < 3; i++ {
		s.expireLease(task.ID)
		s.sweep()
		row := s.rawTask(task.ID)
		s.Equal(board.StageInProgress, row.Stage, "sweep %d: the freeze holds the stage", i)
		s.Equal(attempts, row.Attempts, "sweep %d: zero attempt increments while suspended", i)
		s.Equal(token, row.ClaimToken, "sweep %d: the claim token is held", i)
		s.Require().NotNil(row.LeaseExp)
		s.True(row.LeaseExp.After(time.Now()), "sweep %d: the freeze re-arms the lease (post-resume sweep is a no-op)", i)
	}
	s.Equal(0, s.countEvents("reclaim"), "no reclaim events for a frozen claim")
	s.Equal(0, s.countEvents("escalate"), "no escalation while suspended")

	// Resume: the renewed lease is live — the first running sweep is a
	// NO-OP (no rearm, no requeue, no re-claim: work continues as-is).
	src.clear()
	s.sweep()
	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "clean resume: still in_progress")
	s.Equal(attempts, row.Attempts, "clean resume: attempts untouched")
	s.Equal(token, row.ClaimToken, "clean resume: same claim")
}

// Pin B — genuine lapse while the workspace runs: today's reclaim,
// byte-identically, WITH the seam installed (the regression pin).
func (s *BoardSuite) TestSuspend_GenuineLapseWhileRunningStillReclaims() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "genuinely-dead", nil, false)

	src := &fakeSuspendSource{}
	svc := s.suspendSvc(src)
	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	attempts := s.rawTask(task.ID).Attempts

	// Workspace RUNNING (the source answers, ws NOT in the set) — the
	// worker is dead, the lease lapses, the reclaim must fire exactly
	// as today.
	src.clear()
	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(1, res.Reclaims, "a genuine lapse while running still reclaims")
	row := s.rawTask(task.ID)
	s.Equal(board.StageBacklog, row.Stage, "today's reclaim shape: backlog + cooldown")
	s.Nil(row.ClaimToken)
	s.Nil(row.LeaseExp)
	s.Equal(attempts, row.Attempts, "attempts untouched by the reclaim itself (the increment belongs to the next claim)")
	s.Equal(1, s.countEvents("reclaim"))
}

// Pin C — rapid suspend/resume cycles: idempotent, no double burn.
func (s *BoardSuite) TestSuspend_RapidCyclesIdempotent() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "rapid-cycles", nil, false)

	src := &fakeSuspendSource{}
	svc := s.suspendSvc(src)
	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	attempts := s.rawTask(task.ID).Attempts
	token := s.rawTask(task.ID).ClaimToken

	// Flap the suspension: freeze → sweep → resume → sweep → freeze →
	// expiry → sweep … The row must converge to the frozen shape with
	// exactly the claim's own attempt count — never a double increment,
	// never a requeue.
	for i := 0; i < 4; i++ {
		src.setSuspended(ws)
		s.expireLease(task.ID)
		s.sweep()
		src.clear()
		s.sweep() // resume window: the renewed lease keeps the row out of the victim set
		row := s.rawTask(task.ID)
		s.Equal(board.StageInProgress, row.Stage, "cycle %d: stage held", i)
		s.Equal(attempts, row.Attempts, "cycle %d: no double increment", i)
		s.Equal(token, row.ClaimToken, "cycle %d: no double requeue — the same claim", i)
	}
	s.Equal(0, s.countEvents("reclaim"))
	s.Equal(0, s.countEvents("escalate"))
	s.Equal(1, s.inProgressCount(p.ID))
}

// Pin D — the claim choke point: a suspended workspace grants nothing;
// after resume both the claim and the dispatch drive feed.
func (s *BoardSuite) TestSuspend_ClaimChokePointGrantsNothing() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "gated", nil, false)

	src := &fakeSuspendSource{}
	svc := s.suspendSvc(src)

	// Suspended: ClaimNext is the quiet no-grant; TaskStart is loud.
	src.setSuspended(ws)
	_, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().ErrorIs(err, board.ErrNoCandidate, "a suspended workspace grants nothing (the quiet no-grant)")
	_, err = svc.TaskStart(ctx, s.owner(), task.ID, "", "")
	s.Require().Error(err)
	var guard *board.GuardError
	s.Require().ErrorAs(err, &guard, "task_start on a suspended workspace is a loud 409-class refusal")
	s.Equal(board.GuardBackpressure, guard.Code)

	// The dispatch drive spawns nothing into the suspended workspace
	// (the sweep's backstop runs with a spawner installed).
	before := s.spawner.count()
	s.sweep()
	s.Equal(before, s.spawner.count(), "the dispatch drive spawns nothing into a suspended workspace")
	s.Equal(board.StageBacklog, s.rawTask(task.ID).Stage, "the row stays backlog-ready")

	// Resume: the ordinary machinery feeds the work — claim succeeds,
	// no cleanup, no manual rearm.
	src.clear()
	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID, "after resume the claim feeds")
}

// Pin E — persona claims freeze; the re-arm pass skips suspended
// workspaces (both the fresh-mint and the escalation arms).
func (s *BoardSuite) TestSuspend_PersonaClaimsFreezeAndRearmSkips() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "frozen-reviewer", nil, false)

	src := &fakeSuspendSource{}
	svc := s.suspendSvc(src)
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	attempts := s.rawTask(task.ID).Attempts
	reviewToken := s.rawTask(task.ID).ClaimToken
	s.Require().NotNil(reviewToken, "the reviewer persona holds the in_review claim")

	// Suspend mid-review: the persona lease lapses while suspended —
	// frozen (renewed in place), no #1813 accounting, no re-arm.
	src.setSuspended(ws)
	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(0, res.Reclaims)
	row := s.rawTask(task.ID)
	s.Equal(board.StageInReview, row.Stage, "the freeze holds the review claim")
	s.Equal(reviewToken, row.ClaimToken)
	s.Require().NotNil(row.LeaseExp)
	s.True(row.LeaseExp.After(time.Now()), "the persona lease is re-armed")
	s.Equal(attempts, row.Attempts)
	s.Equal(0, s.countEvents("reclaim"))

	// The re-arm pass skips the suspended workspace: an unclaimed
	// in_review row (the post-lapse shape) is NOT re-claimed while
	// suspended — a fresh reviewer spawn would auto-RESUME the
	// workspace through the activator.
	_, err = s.pool.Exec(ctx, `
		UPDATE tasks SET claim_token=NULL, lease_expires_at=NULL, cooldown_until=NULL WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	s.sweep()
	s.Nil(s.rawTask(task.ID).ClaimToken, "the re-arm pass mints nothing into a suspended workspace")

	// The escalation arm skips too: an at-cap unclaimed row is left
	// alone while suspended (converges after resume).
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET failure_count=3 WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	s.sweep()
	s.Equal(board.StageInReview, s.rawTask(task.ID).Stage, "no escalation write while suspended")

	// Resume: the re-arm pass converges — the at-cap row escalates (the
	// failures are genuine, pre-suspension), the human rearm exit
	// stands. This is the ordinary machinery, un-frozen.
	src.clear()
	s.sweep()
	s.Equal(board.StageFailed, s.rawTask(task.ID).Stage, "after resume the re-arm pass escalates the at-cap row")
}

// Pin N — the nil seam is inert: no source installed ⇒ today's
// semantics even for a "suspended" workspace (the existing suite's
// posture, pinned as the contract).
func (s *BoardSuite) TestSuspend_NilSeamInert() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "no-seam", nil, false)

	svc := s.suspendSvc(nil) // SetupTest's own option set, no source
	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)

	s.expireLease(task.ID)
	res := s.sweep()
	s.Equal(1, res.Reclaims, "nil seam: a lapsed claim reclaims regardless of workspace phase")
	s.Equal(board.StageBacklog, s.rawTask(task.ID).Stage)
}

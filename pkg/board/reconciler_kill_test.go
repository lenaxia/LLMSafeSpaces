// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Reconciler kill-tests (issue #1698, design 0067 §4.5 + §6): the
// self-healing convergence sweep under kill conditions — expired leases
// reclaimed EXACTLY once, crash-mid-operation leaves no stuck/orphaned
// claims, external-truth drift converges, terminal rows are immutable.
package board_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// KILL-TEST 19 (issue #1698: "expired leases are reclaimed exactly once";
// §6 invariant 1): expired rows reclaimed; under CONCURRENT sweeps each row
// is reclaimed exactly once (guarded UPDATE + SKIP LOCKED), with a
// deterministic event id per lapse episode.
func (s *BoardSuite) TestKill_ExpiredLeasesReclaimedExactlyOnce() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)
	var ids []string
	for i := 0; i < 3; i++ {
		t := s.seedTask(e, fmt.Sprintf("lapse%d", i), nil, false)
		c := s.claimOrFail(ws)
		s.Equal(t.ID, c.TaskID)
		s.expireLease(t.ID)
		ids = append(ids, t.ID)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.recon.Sweep(context.Background())
			if err != nil {
				s.T().Errorf("sweep error: %v", err)
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		row := s.rawTask(id)
		s.Equal(board.StageBacklog, row.Stage)
		s.Nil(row.ClaimToken)
		s.Nil(row.Assignee)
		s.Equal(1, row.Attempts, "reclaim never increments")
		s.Equal(1, s.countTransitions(id, board.StageBacklog), "exactly ONE reclaim write per lapse episode")
	}
	s.Equal(3, s.countEvents("reclaim"), "3 reclaims total across 4 concurrent sweeps — exactly once each")
}

// KILL-TEST 20 (issue #1698: "killing the reconciler mid-operation leaves
// no stuck/orphaned claims"; §6 invariant 4): a fault at every named
// failpoint aborts that phase's transaction; nothing partial lands; the
// next sweep converges to the exact same state an uninterrupted sweep
// reaches. Replaying a converged sweep writes nothing (idempotent
// end-to-end).
func (s *BoardSuite) TestKill_ReconcilerCrashMidOperationRecovers() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "crash-reclaim", nil, false)
	s.claimOrFail(ws)
	s.expireLease(t1.ID)

	reclaimsSeen := 0
	for _, where := range []string{"rc1.pre", "rc1.post", "rc2.pre", "rc3.pre", "rc4.pre", "watermark.pre"} {
		s.fps.armAt(where, fmt.Errorf("injected crash at %s", where))
		_, err := s.recon.Sweep(context.Background())
		s.Require().Error(err, "sweep must surface the fault at %s", where)
		s.fps.disarm(where)
		// Nothing partial: the row is either fully untouched (still
		// in_progress with its complete claim state) or fully reclaimed —
		// never a token-less in_progress orphan.
		row := s.rawTask(t1.ID)
		if row.Stage == board.StageInProgress {
			s.Require().NotNil(row.ClaimToken, "no orphaned in_progress row without a claim")
			s.Require().NotNil(row.Assignee)
			s.Equal(0, s.countTransitions(t1.ID, board.StageBacklog), "a faulted sweep writes no partial transitions")
		} else {
			s.Equal(board.StageBacklog, row.Stage)
			s.Nil(row.ClaimToken)
			s.Nil(row.Assignee)
			reclaimsSeen++
			s.Equal(reclaimsSeen, s.countTransitions(t1.ID, board.StageBacklog),
				"a landed reclaim is complete; a later fault never duplicates it")
			// Re-arm the fixture so the remaining fault points still see a
			// reclaim candidate.
			_, err := s.pool.Exec(context.Background(),
				`UPDATE tasks SET stage='in_progress', claim_token=gen_random_uuid(),
				 assignee_session='w', lease_expires_at=now()-interval '1 millisecond' WHERE id=$1`, t1.ID)
			s.Require().NoError(err)
		}
	}

	// Converge: a clean sweep reclaims exactly once more (if the fixture
	// is re-armed) or nothing (if already converged) — and the replay
	// writes no new events either way (deterministic ids).
	res := s.sweep()
	eventsAfterFirst := s.countEvents("reclaim")
	res2 := s.sweep()
	s.Equal(0, res2.Reclaims, "converged sweep is a no-op")
	s.Equal(eventsAfterFirst, s.countEvents("reclaim"), "replay adds no events (deterministic ids)")
	s.Equal(board.StageBacklog, s.rawTask(t1.ID).Stage)
	_ = res
}

// KILL-TEST 21 (§6 invariant 2): merge PR without a stage move → converges
// to Done (PR link as evidence_url) + dependents' is_blocked re-derived.
// Companion: merged with criteria unmet → pending_input/drift_review — the
// Done guard is never bypassed by external truth.
func (s *BoardSuite) TestKill_MergePRConvergesToDone() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	prTask := s.seedTask(e, "pr-task", []string{"pr:42"}, true) // criteria pre-met
	dep := s.seedTask(e, "dependent", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), dep.ID, "", prTask.ID, ""))
	s.Require().True(s.reloadTask(dep.ID).IsBlocked)

	// Park the PR task in in_review with the anchor.
	c := s.claimOrFail(ws)
	s.Equal(prTask.ID, c.TaskID)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), prTask.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)

	s.drift.set(42, board.PRState{Merged: true, URL: "https://git.example/pr/42"})
	res := s.sweep()
	s.Equal(1, res.DriftDone)

	row := s.rawTask(prTask.ID)
	s.Equal(board.StageDone, row.Stage, "merged anchor converges to done within one sweep")
	var evidence string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT evidence_url FROM board_transitions WHERE task_id=$1 AND to_stage='done'`, prTask.ID).Scan(&evidence))
	s.Equal("https://git.example/pr/42", evidence, "the PR link is the transition's evidence")
	s.False(s.reloadTask(dep.ID).IsBlocked, "reverse-edge unblock cascade fired in the same write")
	s.Equal(1, s.countEvents("drift_done"))

	// Companion: criteria unmet → drift_review, done guard holds. The
	// dependent `dep` is ready and outranks by created_at, so the manual
	// dispatch targets the named task directly.
	unmet := s.seedTask(e, "pr-unmet", []string{"pr:43"}, false) // criteria NOT met
	started, err := s.svc.TaskStart(context.Background(), s.owner(), unmet.ID, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", started.ClaimToken), unmet.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	s.drift.set(43, board.PRState{Merged: true, URL: "https://git.example/pr/43"})
	res = s.sweep()
	s.Equal(1, res.DriftReview)
	row = s.rawTask(unmet.ID)
	s.Equal(board.StagePendingInput, row.Stage)
	s.Require().NotNil(row.PendingOrigin)
	s.Equal(board.PendingOriginDriftReview, *row.PendingOrigin)
	s.Equal(board.StageDone, s.rawTask(prTask.ID).Stage, "unrelated rows untouched")
}

// KILL-TEST 22 (RC2 closed-unmerged): re-entry to in_progress mints a fresh
// claim for the recorded assignee — flag=false, attempts unchanged, lease
// set, row sweep-stable (a worker that misses the nudge heals by lapse, it
// is never stranded).
func (s *BoardSuite) TestKill_ClosedUnmergedReentryReclaims() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "reopen", []string{"pr:7"}, true)

	c := s.claimOrFail(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)

	s.drift.set(7, board.PRState{Closed: true, URL: "https://git.example/pr/7"})
	// A spawner-backed reconciler: the re-entry delivers by re-spawn (the
	// suite default runs spawner-less to isolate RC phases).
	respawning := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithPRDriftSource(s.drift),
		board.WithFailpoint(s.fps.fp))
	res, err := respawning.Sweep(context.Background())
	s.Require().NoError(err)
	s.Equal(1, res.DriftReentry)

	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage, "closed-unmerged → in_progress (the one enumerated backward drift)")
	s.Require().NotNil(row.ClaimToken)
	s.NotEqual(c.ClaimToken, *row.ClaimToken, "fresh token minted for the recorded assignee")
	s.Require().NotNil(row.LeaseExp)
	s.False(row.ClaimIncr, "re-claim-class mint: flag false (its 429 never decrements)")
	s.Equal(1, row.Attempts, "re-entry never increments")
	s.NotNil(row.Assignee)

	// The re-minted token is operable: heartbeat works.
	_, err = s.svc.Heartbeat(context.Background(), s.agent("sess", *row.ClaimToken), task.ID, "", time.Now().Add(time.Second))
	s.Require().NoError(err)
	// ...and heals by ordinary lease lapse if the worker missed the nudge.
	s.expireLease(task.ID)
	res = s.sweep()
	s.Equal(1, res.Reclaims)
	s.Equal(board.StageBacklog, s.rawTask(task.ID).Stage)
}

// KILL-TEST 23 (RC3): dependency consistency — the sweep recomputes
// is_blocked from the edge table (the belt beneath in-transaction
// maintenance), enforces N-ary ALL-Done semantics, and stages
// canceled-dependency dependents to pending_input (skipping rows already
// pending — origin preserved, no oscillation). Terminal rows'
// is_blocked is stable (A→done writes nothing on a terminal dependent).
func (s *BoardSuite) TestKill_RC3DependencyConsistency() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)

	a := s.seedTask(e, "a", nil, true)
	b := s.seedTask(e, "b", nil, true)
	nary := s.seedTask(e, "n-ary", nil, false) // depends on a AND b
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), nary.ID, "", a.ID, ""))
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), nary.ID, "", b.ID, ""))
	s.True(s.reloadTask(nary.ID).IsBlocked)

	// Half-unblock: a done, b not → STILL blocked (ALL-Done semantics).
	s.doneDirect(a)
	s.sweep()
	s.True(s.reloadTask(nary.ID).IsBlocked, "two deps, one done → still blocked")
	s.Equal(board.StageBacklog, s.reloadTask(nary.ID).Stage)

	// Missed-cascade corruption: flip the stored derivation wrong, sweep
	// repairs (the RC3 belt).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET is_blocked=false WHERE id=$1`, nary.ID)
	s.Require().NoError(err)
	res := s.sweep()
	s.Equal(1, res.DepRepairs)
	s.True(s.reloadTask(nary.ID).IsBlocked, "RC3 recomputes from the edge table")

	// Full unblock: b done too → ready.
	s.doneDirect(b)
	s.False(s.reloadTask(nary.ID).IsBlocked, "ALL deps done → ready (instant, event-driven)")

	// Canceled dependency: dependents flip to pending_input/dependency_
	// #1747: delete replaces cancel — deleting a prerequisite removes its
	// edges, so dependents UNBLOCK (no pending staging anywhere); each
	// orphan is listed in the response and carries an unblocked_by_delete
	// audit event.
	c := s.seedTask(e, "c-prereq", nil, true)
	d1 := s.seedTask(e, "d1", nil, false)
	d2 := s.seedTask(e, "d2", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), d1.ID, "", c.ID, ""))
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), d2.ID, "", c.ID, ""))

	out, err := s.svc.DeleteTask(context.Background(), s.owner(), c.ID, "", false, "")
	s.Require().NoError(err)
	s.Len(out.OrphanedDependents, 2, "both dependents are listed (affected-party visibility)")
	for _, dep := range out.OrphanedDependents {
		s.Equal(1, s.countEvents("unblocked_by_delete:"+dep))
	}
	s.False(s.rawTask(d1.ID).IsBlocked, "the edge vanished → ready, full stop")
	s.False(s.rawTask(d2.ID).IsBlocked)
	s.Equal(board.StageBacklog, s.rawTask(d1.ID).Stage, "dependents unblock in place — never staged pending")

	// The sweep's convergence net stays quiet: the in-tx rederive already
	// converged the rows (zero-write guarantee on a clean graph).
	res = s.sweep()
	s.Equal(0, res.DepRepairs, "RC3 finds nothing to repair after the in-tx rederive")

	// Done-row stability: a DONE dependent of a not-done task never gets
	// is_blocked writes (RC3's stage predicate).
	x := s.seedTask(e, "x", nil, true)
	doneDep := s.seedTask(e, "done-dep", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), doneDep.ID, "", x.ID, ""))
	s.doneDirect(doneDep)
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET is_blocked=false WHERE id=$1`, doneDep.ID)
	s.Require().NoError(err)
	res = s.sweep()
	s.Equal(0, res.DepRepairs, "RC3 never writes is_blocked on done rows (zero-write guarantee)")
	s.Equal(board.StageDone, s.rawTask(doneDep.ID).Stage)
}

// doneDirect drives a criteria-met task to done via the service (fixture
// helper with its own evidence url). A backlog task is manually dispatched
// first (the named-task path) so the Done transition has a legal source.
func (s *BoardSuite) doneDirect(t *board.Task) {
	ctx := context.Background()
	cur := s.reloadTask(t.ID)
	tok := ""
	if cur.Stage == board.StageBacklog {
		started, err := s.svc.TaskStart(ctx, s.owner(), t.ID, "", "")
		s.Require().NoError(err)
		s.Require().False(started.Escalated)
		tok = started.ClaimToken
	}
	updated, err := s.svc.UpdateTask(ctx, s.owner(), t.ID, "",
		board.TaskInput{Title: cur.Title, AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c", Met: true}}}, "")
	s.Require().NoError(err)
	_ = updated
	actor := s.owner()
	if tok != "" {
		actor = s.agent("sess", tok)
	} else if cur.ClaimToken != "" {
		actor = s.agent("sess", cur.ClaimToken)
	}
	_, err = s.svc.MoveStage(ctx, actor, t.ID, "", board.StageDone,
		"https://evidence.example/"+t.ShortID, "")
	s.Require().NoError(err)
}

// KILL-TEST 24 (RC4): epic completion is event-fed off the last
// non-terminal task reaching done; an all-canceled epic NEVER completes;
// a task-less epic never completes; the completed→active re-open stays
// active (exempt from the immediate re-check); done-while-draft → activate
// completes immediately.
func (s *BoardSuite) TestKill_RC4EpicCompletion() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "one", nil, true)
	t2 := s.seedTask(e, "two", nil, true)

	s.doneDirect(t1)
	s.Equal(board.EpicActive, s.epicStatus(e.ID), "non-terminal tasks remain — epic still feeds")

	// Delete t2 (#1747: cancel is eliminated): the remaining non-terminal
	// task is gone; t1's done transition already fired... the RC4 net
	// catches the new terminal set.
	_, err := s.svc.DeleteTask(context.Background(), s.owner(), t2.ID, "", false, "")
	s.Require().NoError(err)
	res := s.sweep()
	s.Equal(1, res.EpicCompletions, "the watermark net completes the epic once no non-terminal tasks remain")
	s.Equal(board.EpicCompleted, s.epicStatus(e.ID))
	res = s.sweep()
	s.Equal(0, res.EpicCompletions, "completed is stable — the sweep never re-fires")

	// Re-open (completed→active): exempt from the immediate re-check —
	// stays active with zero follow-up work.
	reopened, err := s.svc.EpicActivate(context.Background(), s.owner(), e.ID, "", false, "")
	s.Require().NoError(err)
	s.Equal(board.EpicActive, reopened.Status)

	// All-deleted epic never completes (RC4 requires a done task —
	// deletion leaves no rows at all).
	e2 := s.seedEpic(p, board.EpicActive)
	c1 := s.seedTask(e2, "c1", nil, false)
	_, err = s.svc.DeleteTask(context.Background(), s.owner(), c1.ID, "", false, "")
	s.Require().NoError(err)
	res = s.sweep()
	s.NotEqual(board.EpicCompleted, s.epicStatus(e2.ID), "an all-deleted epic never satisfies the completion trigger")

	// Task-less epic never completes.
	e3 := s.seedEpic(p, board.EpicActive)
	res = s.sweep()
	s.NotEqual(board.EpicCompleted, s.epicStatus(e3.ID))
	s.Equal(0, res.EpicCompletions)

	// Done-while-draft: activating a finished epic completes immediately.
	e4 := s.seedEpic(p, board.EpicDraft)
	dt := s.seedTask(e4, "done-while-draft", nil, true)
	s.doneDirect(dt)
	act, err := s.svc.EpicActivate(context.Background(), s.owner(), e4.ID, "", false, "")
	s.Require().NoError(err)
	s.Equal(board.EpicCompleted, act.Status, "draft-origin activation of an all-terminal epic completes immediately")
}

// KILL-TEST 25 (§6 invariant 3): cancel mid-flight → worker aborted, token
// revoked, stage canceled, non-terminal dependents staged pending_input;
// then EVERY reconciler path runs against it → ZERO writes (trigger + pin).
func (s *BoardSuite) TestKill_CancelMidFlightNeverResurrects() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "cxl-core", nil, false)
	dep := s.seedTask(e, "victim-dep", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), dep.ID, "", task.ID, ""))

	c := s.claimOrFail(ws)
	s.Equal(task.ID, c.TaskID)
	assignee := s.reloadTask(task.ID).AssigneeSession

	// #1747: delete replaces cancel — the in-flight row gets a clean
	// abort FIRST (the interrupt seam), then the row vanishes; the
	// dependent's edge vanishes with it (listed + audited as orphan).
	_, err := s.svc.DeleteTask(context.Background(), s.owner(), task.ID, "", false, "")
	s.Require().NoError(err)
	s.Equal(1, s.aborter.count(), "worker abort issued (rides the interrupt seam)")
	s.Equal(task.ID+"/"+assignee, s.aborter.callsSlice()[0])

	var gone int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE id=$1`, task.ID).Scan(&gone))
	s.Equal(0, gone, "delete removes the object")
	dRow := s.rawTask(dep.ID)
	s.Equal(board.StageBacklog, dRow.Stage, "the orphan unblocks in place — never staged pending")
	s.False(dRow.IsBlocked)

	// Content preservation (#1747 6261349020): the delete event embeds
	// the FULL final-state snapshot — the only content-history a deleted
	// task body has.
	var snap int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='delete_task' AND item_id=$1
		 AND payload ? 'snapshot' AND payload->'snapshot'->>'title' = $2`,
		task.ID, task.Title).Scan(&snap))
	s.Equal(1, snap, "one delete_task event with the full snapshot")

	eventsBefore := s.countEvents("")
	transitionsBefore := s.countTransitions(task.ID, "")

	// Arm drift on a pr tag, run every reconciler path — nothing
	// resurrects or writes the gone row (the history rows stay intact).
	s.drift.set(99, board.PRState{Merged: true, URL: "https://git.example/pr/99"})
	res := s.sweep()
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE id=$1`, task.ID).Scan(&gone))
	s.Equal(0, gone, "never resurrects")
	s.Equal(0, res.Reclaims, "RC1 skips gone rows")
	s.Equal(0, res.DriftDone, "RC2 skips gone rows")
	s.Equal(transitionsBefore, s.countTransitions(task.ID, ""), "zero writes on the gone row (history intact)")
	s.Equal(eventsBefore, s.countEvents(""), "no system writes either")
}

// KILL-TEST 26 (§4.4 pending_input coupling): worker-origin auto-stage →
// resolution resume re-arms the lease (NO mint: token + provenance flag
// untouched — both token provenances asserted), and a lease-window-length
// human answer never hands the task to reclaim.
func (s *BoardSuite) TestKill_PendingInputResumeRearmsLease() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "q", nil, false)

	c := s.claimOrFail(ws)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.True(s.reloadTask(task.ID).ClaimIncremented)

	// The worker asks a question: pending-input projection flips.
	s.pending.set(assignee, true)
	res := s.sweep()
	s.Equal(1, res.PendingStaged)
	row := s.rawTask(task.ID)
	s.Equal(board.StagePendingInput, row.Stage)
	s.Require().NotNil(row.PendingOrigin)
	s.Equal(board.PendingOriginWorker, *row.PendingOrigin)
	s.Equal(c.ClaimToken, *row.ClaimToken, "auto-stage keeps the token (no mint)")
	s.True(row.ClaimIncr, "and keeps the provenance flag (claim-token provenance)")

	// The lease ages out under pending_input by more than a lease window:
	// still never reclaimed (stage predicate excludes pending_input).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 hour' WHERE id=$1`, task.ID)
	s.Require().NoError(err)
	res = s.sweep()
	s.Equal(0, res.Reclaims, "pending_input suspends the lease clock by construction")
	s.Equal(board.StagePendingInput, s.rawTask(task.ID).Stage)

	// Human answers: resume re-arms the lease, token + flag untouched.
	s.pending.set(assignee, false)
	res = s.sweep()
	s.Equal(1, res.Resumes)
	row = s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage)
	s.Require().NotNil(row.LeaseExp, "resume re-arms the lease (now()+lease)")
	s.Require().NotNil(row.ClaimToken)
	s.Equal(c.ClaimToken, *row.ClaimToken, "resume is a NO-MINT preserve-case (r11 fix)")
	s.True(row.ClaimIncr, "flag preserved: the claim→pending→resume→429 cycle still rolls back its increment")

	// A worker-origin cycle with a BOUNCE-provenance token keeps flag=false
	// through the same cycle (the other provenance arm).
	b := s.seedTask(e, "bounce-cycle", nil, false)
	cb := s.claimOrFail(ws)
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", cb.ClaimToken), b.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(context.Background(), s.owner(), b.ID, "", board.StageInProgress, "", "")
	s.Require().NoError(err)
	s.False(s.rawTask(b.ID).ClaimIncr)
	bounceAssignee := s.reloadTask(b.ID).AssigneeSession
	bounceToken := s.reloadTask(b.ID).ClaimToken
	s.pending.set(bounceAssignee, true)
	s.sweep()
	s.pending.set(bounceAssignee, false)
	s.sweep()
	rowB := s.rawTask(b.ID)
	s.Equal(board.StageInProgress, rowB.Stage)
	s.False(rowB.ClaimIncr, "bounce-token provenance preserved through the cycle (its 429 still skips the decrement)")
	s.Require().NotNil(rowB.ClaimToken)
	s.Equal(bounceToken, *rowB.ClaimToken)

	// The resumed claim is live: its 429 rolls back the original increment.
	_, err = s.svc.ReportThrottle(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", s.cfg.ThrottleBackoff, "")
	s.Require().NoError(err)
	s.Equal(0, s.rawTask(task.ID).Attempts, "claim→pending→resume→429: net attempts 0")
}

// KILL-TEST 27 (§4.5 watermark incrementality): the drift checks are
// event-fed off the watermark — no rescan of pre-watermark anchors, and
// second sweeps do not re-ask the drift host.
func (s *BoardSuite) TestKill_WatermarkIncremental() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	old := s.seedTask(e, "old-anchor", []string{"pr:5"}, true)
	c := s.claimOrFail(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), old.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	s.drift.set(5, board.PRState{Merged: true, URL: "u"})

	s.sweep()
	asks1 := s.drift.askCount()
	s.Greater(asks1, 0)
	s.sweep()
	s.Equal(asks1, s.drift.askCount(), "no drift-host re-asks for pre-watermark rows on the second pass")
	s.Equal(board.StageDone, s.rawTask(old.ID).Stage)
}

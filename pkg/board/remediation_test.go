// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// r1 remediation pins (PR #1719 review): the C1 RemoveDependency
// regression (short-ID targets), the §6 r3 TaskStart residues (all-draft
// dispatch; paused/completed/archived refusals; DRR bypass with counters
// advancing), the §4.4 dispatch-drive sweep backstop (r1-C2), the
// watermark no-skip race (r1-C3), and the S1 cross-tenant guards on
// in-flight writes.
package board_test

import (
	"context"
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// KILL-TEST 37 (r1-C1): remove_dependency resolves its target like every
// other addressing surface — a short ID must remove the edge (the raw-ID
// DELETE matched zero rows and reported success while the edge survived:
// the design's named pending_input resolution was the broken write).
func (s *BoardSuite) TestKill_RemoveDependencyShortID() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "dep-target", nil, false)
	b := s.seedTask(e, "dep-holder", nil, false)

	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), b.ID, "", a.ID, ""))
	s.True(s.reloadTask(b.ID).IsBlocked)

	// The short-ID form removes the edge and re-derives is_blocked.
	s.Require().NoError(s.svc.RemoveDependency(ctx, s.owner(), b.ID, "", a.ShortID, ""))
	s.False(s.reloadTask(b.ID).IsBlocked, "the edge is GONE (not a silent no-op)")
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_dependencies WHERE task_id=$1`, b.ID).Scan(&n))
	s.Equal(0, n)
	s.Equal(1, s.countEvents("remove_dependency"))

	// The UUID form keeps working.
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), b.ID, "", a.ID, ""))
	s.Require().NoError(s.svc.RemoveDependency(ctx, s.owner(), b.ID, "", a.ID, ""))
	s.False(s.reloadTask(b.ID).IsBlocked)

	// Removing an edge off a pending_input dependency-abandoned row
	// unblocks it (the §4.2 resolution path).
	c := s.seedTask(e, "dep-cxl", nil, false)
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), b.ID, "", c.ID, ""))
	_, err := s.svc.CancelTask(ctx, s.owner(), c.ID, "", "abandon", "")
	s.Require().NoError(err)
	s.Equal(board.StagePendingInput, s.rawTask(b.ID).Stage)
	s.Require().NoError(s.svc.RemoveDependency(ctx, s.owner(), b.ID, "", c.ShortID, ""))
	s.False(s.reloadTask(b.ID).IsBlocked, "edge removed → ready (the human re-queue is the move_stage exit)")
}

// KILL-TEST 38 (§6 r3 residues, r1): task_start's three enumerated pins —
// (a) manual dispatch in an ALL-DRAFT project succeeds; (b) paused/
// completed/archived epics and archived projects refuse it; (c) the DRR
// eligibility bypass still advances the counters.
func (s *BoardSuite) TestKill_TaskStartR3Pins() {
	ctx := context.Background()

	// (a) all-draft project: the manual path is the point.
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	eDraft := s.seedEpic(p, board.EpicDraft) // never activated
	t := s.seedTask(eDraft, "draft-only", nil, false)
	started, err := s.svc.TaskStart(ctx, s.owner(), t.ID, "", "")
	s.Require().NoError(err)
	s.Require().False(started.Escalated)
	s.Equal(board.StageInProgress, s.rawTask(t.ID).Stage, "an all-draft project must still dispatch")

	// (c) the DRR bypass advances the counters: park this epic at quota
	// first, then task_start a second task in it — vft/deficit both +1.
	s.finishClaim(started)
	vft0, def0, _ := s.epicDRR(eDraft.ID)
	t2 := s.seedTask(eDraft, "at-quota-manual", nil, false)
	_, err = s.pool.Exec(ctx, `UPDATE epics SET deficit=quantum WHERE id=$1`, eDraft.ID)
	s.Require().NoError(err)
	started2, err := s.svc.TaskStart(ctx, s.owner(), t2.ID, "", "")
	s.Require().NoError(err, "task_start bypasses the deficit<quantum eligibility (a human override)")
	s.Require().False(started2.Escalated)
	vft1, def1, _ := s.epicDRR(eDraft.ID)
	s.Equal(vft0+1, vft1, "vft advances (claims served)")
	s.Equal(def0+1, def1, "deficit advances — fairness accounting stays true")
	s.finishClaim(started2)

	// (b) refusals: paused / completed / archived epics, archived project.
	for i, status := range []string{board.EpicPaused, board.EpicCompleted, board.EpicArchived} {
		e := s.seedEpic(p, status)
		tt := s.seedTask(e, fmt.Sprintf("refuse-%s", status), nil, false)
		_, err := s.svc.TaskStart(ctx, s.owner(), tt.ID, "", "")
		s.Require().Error(err, "%s epic must refuse task_start", status)
		var ge *board.GuardError
		s.Require().ErrorAs(err, &ge)
		s.Equal(board.GuardEpicState, ge.Code)
		s.Equal(board.StageBacklog, s.rawTask(tt.ID).Stage)
		_ = i
	}
	pa := s.seedProject(ws, 4)
	ea := s.seedEpic(pa, board.EpicActive)
	ta := s.seedTask(ea, "archived-project", nil, false)
	_, err = s.svc.ArchiveProject(ctx, s.owner(), pa.ID, "")
	s.Require().NoError(err)
	_, err = s.svc.TaskStart(ctx, s.owner(), ta.ID, "", "")
	s.Require().Error(err, "archived projects refuse task_start (terminal storage stays closed)")
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardProjectState, ge.Code)
}

// KILL-TEST 39 (§4.4 dispatch drive, r1-C2): the sweep is the backstop —
// work the event path misses (here: every bit of it, Lane 1 dark-run) is
// re-fed by the drive. A claimed-and-lapsed task is reclaimed by RC1 and
// IMMEDIATELY re-dispatched by the same sweep's drive: it never sits in
// backlog. Runs with a spawn seam installed (the drive's documented
// precondition).
func (s *BoardSuite) TestKill_DispatchDriveResweepFeedsWork() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "drive-1", nil, true)
	t2 := s.seedTask(e, "drive-2", nil, true)

	drive := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithPRDriftSource(s.drift),
		board.WithFailpoint(s.fps.fp),
	)

	// First sweep: the drive claims BOTH slots' worth of work and spawns
	// fresh workers (R9 claim-then-spawn).
	res, err := drive.Sweep(context.Background())
	s.Require().NoError(err)
	s.Equal(2, res.Dispatched, "the drive feeds claimable work at sweep cadence")
	s.Equal(2, s.inProgressCount(p.ID))
	s.Equal(2, s.spawner.count())
	for _, id := range []string{t1.ID, t2.ID} {
		row := s.rawTask(id)
		s.Require().NotNil(row.ClaimToken)
		s.Require().NotNil(row.Assignee, "the spawn stamped the fresh session's identity (R9)")
	}

	// Lapse everything; the next sweep reclaims (RC1) AND re-drives the
	// reclaimed rows in one pass — a reclaimed task never sits in backlog.
	s.expireLease(t1.ID)
	s.expireLease(t2.ID)
	res, err = drive.Sweep(context.Background())
	s.Require().NoError(err)
	s.Equal(2, res.Reclaims)
	s.Equal(2, res.Dispatched, "the sweep backstop re-feeds reclaimed work")
	s.Equal(2, s.inProgressCount(p.ID))
	s.Equal(4, s.spawner.count(), "a replacement worker is simply the next fresh spawn")

	// Escalation composes with the drive: at the attempts cap the drive
	// escalates and stops granting (rows leave the candidate set).
	for i := 0; i < s.cfg.AttemptsCap; i++ {
		s.expireLease(t1.ID)
		s.expireLease(t2.ID)
		res, err = drive.Sweep(context.Background())
		s.Require().NoError(err)
	}
	s.Equal(2, s.countEvents("escalate"))
	s.Equal(board.StagePendingInput, s.rawTask(t1.ID).Stage)
	s.Equal(board.StagePendingInput, s.rawTask(t2.ID).Stage)
	s.Equal(3, s.rawTask(t1.ID).Attempts, "escalation withholds the (N+1)th grant — attempts stay at N")
}

// KILL-TEST 40 (r1-C3): the watermark never skips a concurrent writer's
// event. An event committed DURING a sweep (here: injected at the rc2.pre
// failpoint, after the high-water capture) must still be above the
// advanced watermark — the NEXT sweep processes it.
func (s *BoardSuite) TestKill_WatermarkNeverSkipsConcurrentEvents() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "late-anchor", []string{"pr:77"}, true)

	// Park the task in in_review BEFORE the sweep (its move_stage event is
	// below the sweep's high-water), then arm a drift host answer.
	c := s.claimOrFail(ws)
	s.Require().Equal(task.ID, c.TaskID)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	s.drift.set(77, board.PRState{Merged: true, URL: "https://git.example/77"})

	// A "concurrent writer" commits an event MID-sweep: at rc2.pre (after
	// the high-water capture), inject an event with created_at strictly
	// above everything captured — the shape of a commit racing past the
	// capture point. r1-C3's guarantee: the advanced watermark stays
	// BELOW it, so the next sweep re-scans it.
	poked := false
	s.fps.armAtFn("rc2.pre", func() error {
		if poked { // one-shot: later sweeps must not re-poke (PK)
			return nil
		}
		poked = true
		_, err := s.pool.Exec(context.Background(), `
			INSERT INTO board_events (id, project_id, item_kind, item_id, action, actor, payload, created_at)
			VALUES ('poke-r1-c3', $1, 'task', $2, 'concurrent_poke', '{}', '{}', now() + interval '1 second')`,
			p.ID, task.ID)
		s.Require().NoError(err)
		return nil
	})

	res := s.sweep()
	s.Equal(1, res.DriftDone, "the in_review anchor converges this sweep")
	s.Equal(board.StageDone, s.rawTask(task.ID).Stage)

	// The poke's created_at (now+1s) is ABOVE the sweep's advanced
	// watermark — verify directly, then pin the behavioral consequence:
	// a SECOND anchored task whose in_review event lands late (after the
	// capture) is still seen by the NEXT sweep.
	var wm time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_event_at FROM board_sweeps WHERE scope='default'`).Scan(&wm))
	var pokeAt time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT created_at FROM board_events WHERE id='poke-r1-c3'`).Scan(&pokeAt))
	s.True(pokeAt.After(wm), "a concurrent event above the capture is never skipped (advanced watermark < event)")

	lateE := s.seedEpic(p, board.EpicActive)
	late := s.seedTask(lateE, "late-event", []string{"pr:78"}, true)
	started, err := s.svc.TaskStart(context.Background(), s.owner(), late.ID, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", started.ClaimToken), late.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	s.drift.set(78, board.PRState{Merged: true, URL: "https://git.example/78"})

	// This sweep's high-water capture INCLUDES the late event (it
	// committed before this sweep started) → RC2 converges it now.
	res = s.sweep()
	s.Equal(1, res.DriftDone)
	s.Equal(board.StageDone, s.rawTask(late.ID).Stage)
}

// KILL-TEST 41 (r1-S1): in-flight writes bind to the caller's workspace —
// a worker in workspace A holding workspace B's (leaked) token is
// rejected on heartbeat AND throttle; the row never moves.
func (s *BoardSuite) TestKill_InFlightWritesWorkspaceBound() {
	wsA := s.seedWorkspace()
	wsB := s.seedWorkspace()
	pB := s.seedProject(wsB, 4)
	eB := s.seedEpic(pB, board.EpicActive)
	task := s.seedTask(eB, "b-claimed", nil, false)

	c := s.claimOrFail(wsB) // B's own worker claims
	s.Require().Equal(task.ID, c.TaskID)
	token := c.ClaimToken

	// A's worker presents B's leaked token: the workspace-scoped resolution
	// makes B's task INVISIBLE to A (404 — the stronger half of the S1
	// fix), so neither the lease extension nor the 429 requeue can even
	// address the row (dispatch DoS closed).
	leaker := board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
		ClaimToken: token, WorkspaceID: wsA}
	_, err := s.svc.Heartbeat(context.Background(), leaker, task.ID, "", time.Now().Add(time.Second))
	s.Require().Error(err)
	var nf *board.NotFoundError
	s.Require().ErrorAs(err, &nf, "a foreign workspace cannot even resolve the task")

	_, err = s.svc.ReportThrottle(context.Background(), leaker, task.ID, "", time.Minute, "")
	s.Require().Error(err)
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "no cross-tenant requeue")
	s.Equal(1, s.rawTask(task.ID).Attempts, "no attempt rollback either")
	var tok string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT claim_token::text FROM tasks WHERE id=$1`, task.ID).Scan(&tok))
	s.Equal(token, tok, "B's claim is untouched")

	// B's own worker (same token) still works.
	own := board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent,
		ClaimToken: token, WorkspaceID: wsB}
	_, err = s.svc.Heartbeat(context.Background(), own, task.ID, "", time.Now().Add(time.Second))
	s.Require().NoError(err)
}

// KILL-TEST 42 (merged doc §4.4 engine pins): the interim state —
// post-claim/pre-spawn rows are in_progress + token + lease +
// assignee_session=NULL (the merged design's claim shape), lease re-armed
// at stamp, and a killed spawn heals via RC1 lapse → re-spawn.
func (s *BoardSuite) TestKill_InterimStateClaimShape() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "interim", nil, false)

	// A spawner-less service pins the raw claim shape (the spawn-stamp
	// never lands).
	bare := board.NewService(s.pool, s.cfg, board.WithFailpoint(s.fps.fp))
	c, err := bare.ClaimNext(context.Background(), "w", ws)
	s.Require().NoError(err)
	s.Require().False(c.Escalated)
	s.Equal(ws, c.WorkspaceID, "the claim envelope carries the project's target workspace")
	row := s.rawTask(task.ID)
	s.Equal(board.StageInProgress, row.Stage)
	s.Require().NotNil(row.ClaimToken)
	s.Require().NotNil(row.LeaseExp)
	s.Nil(row.Assignee, "assignee NULL during the claim→spawn interim (the merged design's shape)")

	// The interim heals by lapse: expired → reclaim (the spawner-less
	// sweep) → backlog; a spawner-backed drive re-spawns.
	s.expireLease(task.ID)
	s.sweep()
	s.Equal(board.StageBacklog, s.rawTask(task.ID).Stage)
	drive := board.NewReconciler(s.pool, s.cfg, board.WithWorkerSpawner(s.spawner.Spawn))
	res, err := drive.Sweep(context.Background())
	s.Require().NoError(err)
	s.Equal(1, res.Dispatched)
	s.Require().NotNil(s.rawTask(task.ID).Assignee, "the guarded spawn-stamp landed")
}

// KILL-TEST 43 (merged doc §4.4 dispatch-drive pin): the event-driven
// half — activate/create/unblock events enqueue a drive of the affected
// project; DriveProject (the consumer's call) produces the same claim a
// sweep-path drive would.
func (s *BoardSuite) TestKill_EventDrivenDispatchDrive() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicDraft)
	task := s.seedTask(e, "event-driven", nil, true)

	driveCh := make(chan string, 8)
	driven := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithDriveQueue(driveCh),
		board.WithFailpoint(s.fps.fp))

	// Task creates poke the drive unconditionally (§4.4 "task/edge
	// creates"); the predicates resolve claimability — in a still-draft
	// epic the drive is an inert no-op.
	_, err := driven.CreateTask(context.Background(), s.owner(), e.ID, "",
		board.TaskInput{Title: "draft-no-drive-yet"}, "")
	s.Require().NoError(err)
	select {
	case pid := <-driveCh:
		s.Equal(p.ID, pid, "the create enqueues the affected project")
		n, err := driven.DriveProject(context.Background(), pid)
		s.Require().NoError(err)
		s.Equal(0, n, "nothing claimable in a draft epic — the drive's predicates gate")
	default:
		s.T().Fatal("a task create must poke the drive (§4.4 event path)")
	}

	// Activation pokes again; the consumer now claims BOTH ready tasks
	// (slots permitting) with fresh spawns — identical in shape to the
	// sweep path.
	_, err = driven.EpicActivate(context.Background(), s.owner(), e.ID, "", "")
	s.Require().NoError(err)
	select {
	case pid := <-driveCh:
		s.Equal(p.ID, pid, "the affected project is enqueued")
		n, err := driven.DriveProject(context.Background(), pid)
		s.Require().NoError(err)
		s.Equal(2, n, "the event-driven drive claims the now-ready work (both tasks)")
	default:
		s.T().Fatal("epic_activate must enqueue a drive (§4.4 event path)")
	}
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage)
	s.Equal(2, s.spawner.count())

	// The unblocking events poke too: finish the task (→done), create a
	// dependent pair, and remove the edge — the poke fires on remove.
	claim := &board.Claim{TaskID: task.ID, ClaimToken: s.reloadTask(task.ID).ClaimToken, ShortID: task.ShortID}
	s.finishClaim(claim)
	blocker := s.seedTask(e, "blocker2", nil, true)
	dependent := s.seedTask(e, "dependent2", nil, true)
	s.Require().NoError(driven.AddDependency(context.Background(), s.owner(), dependent.ID, "", blocker.ID, ""))
	s.True(s.reloadTask(dependent.ID).IsBlocked)
	s.Require().NoError(driven.RemoveDependency(context.Background(), s.owner(), dependent.ID, "", blocker.ID, ""))
	select {
	case pid := <-driveCh:
		s.Equal(p.ID, pid, "the unblocking event enqueues the drive")
	default:
		s.T().Fatal("remove_dependency must poke the drive (§4.4 unblocking events)")
	}
}

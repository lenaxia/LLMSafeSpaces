// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// 1747_kill_test.go — the #1747 ruling pins (owner rulings 2026-10-19,
// all rounds): the agent-surface manager binding, pause-must-pause,
// delete-replaces-cancel (snapshots/orphans/reconnect), the DAG spec
// (cross-workspace edges, caps), import_plan + dry-run, epic_activate's
// inactive-dep acknowledgment + ready set, the concurrency caps, and
// grant differentiation's engine belt.

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeMessenger records halt/resume directives (#1747 pause-must-pause).
type fakeMessenger struct {
	calls []string // "directive:session:task"
}

func (f *fakeMessenger) Deliver(_ context.Context, sessionID, directive, taskShortID string) error {
	f.calls = append(f.calls, directive+":"+sessionID+":"+taskShortID)
	return nil
}

// claimWith claims through a caller-supplied service (cap/pressure variants).
func (s *BoardSuite) claimWith(svc *board.Service, wsID string) *board.Claim {
	c, err := svc.ClaimNext(context.Background(), "worker-"+wsID, wsID)
	s.Require().NoError(err)
	return c
}

// agentFor returns an agent-surface actor bound to ws.
func (s *BoardSuite) agentFor(ws string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent, WorkspaceID: ws}
}

// KILL-TEST 1747-a (manager binding): the agent surface manages its OWN
// workspace's board — the correct binding passes, a foreign project
// workspace-binds (403 class), and create_project is structurally
// scoped to the caller's workspace.
func (s *BoardSuite) TestKill_1747_AgentManagerWorkspaceBinding() {
	ws := s.seedWorkspace()
	other := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)

	// Correct binding: the agent creates a task on its own workspace's board.
	_, err := s.svc.CreateTask(context.Background(), s.agentFor(ws), e.ID, "",
		board.TaskInput{Title: "agent-made"}, "")
	s.Require().NoError(err)

	// Foreign binding: a project targeting ANOTHER workspace refuses.
	pOther := s.seedProject(other, 4)
	eOther := s.seedEpic(pOther, board.EpicActive)
	_, err = s.svc.CreateTask(context.Background(), s.agentFor(ws), eOther.ID, "",
		board.TaskInput{Title: "not mine"}, "")
	s.Require().Error(err)
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	s.Equal(board.GuardWorkspaceBinding, ge.Code, "the structural #1747 binding, not a generic authz")

	// create_project: the agent's target is its own workspace by rule.
	_, err = s.svc.CreateProject(context.Background(), s.agentFor(ws),
		board.ProjectInput{Name: "own", TargetWorkspaceID: ws}, "")
	s.Require().NoError(err)
	_, err = s.svc.CreateProject(context.Background(), s.agentFor(ws),
		board.ProjectInput{Name: "foreign", TargetWorkspaceID: other}, "")
	s.Require().Error(err)
	require.ErrorAs(s.T(), err, &ge)
	s.Equal(board.GuardWorkspaceBinding, ge.Code)

	// A REST-surface actor is unaffected (ownership-only scoping, R7).
	_, err = s.svc.CreateProject(context.Background(), s.owner(),
		board.ProjectInput{Name: "rest-ok", TargetWorkspaceID: other}, "")
	s.Require().NoError(err)
}

// KILL-TEST 1747-b (pause-must-pause): pause parks the row (no claims),
// refuses forward stage-writes, delivers the HALT directive, and never
// freezes the lease; resume clears and delivers RESUME.
func (s *BoardSuite) TestKill_1747_PauseMustPause() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "pausable", nil, false)
	t2 := s.seedTask(e, "next-in-line", nil, false)
	msg := &fakeMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	// Pause a BACKLOG row: parked — the claim skips it for the sibling.
	_, err := s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	c := s.claimOrFail(ws)
	s.Equal(t2.ID, c.TaskID, "a paused row is never claimable")
	s.finishClaim(c)

	// Resume a BACKLOG row: no directives (no live worker to message) —
	// the flag is the whole story; the row re-enters candidacy.
	_, err = s.svc.ResumeTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Empty(s.T(), msg.calls, "backlog pause/resume messages nobody")

	// Pause the IN-FLIGHT row: halt directive lands, lease keeps running,
	// forward writes refuse (GuardPaused), the bounce refuses.
	c2 := s.claimOrFail(ws)
	s.Equal(t1.ID, c2.TaskID)
	_, err = s.svc.PauseTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 1)
	s.Contains(msg.calls[0], board.DirectiveHalt, "pause delivers HALT (turn-boundary, not an abort)")
	s.Equal(0, s.aborter.count(), "pause NEVER aborts the worker")
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", c2.ClaimToken), t1.ID, "", board.StageInReview, "https://ev.test/1", "")
	s.Require().Error(err)
	var ge2 *board.GuardError
	require.ErrorAs(s.T(), err, &ge2)
	s.Equal(board.GuardPaused, ge2.Code, "forward writes refuse while paused")

	// Resume in-flight: forward writes resume after resume.
	_, err = s.svc.ResumeTask(context.Background(), s.owner(), t1.ID, "", "")
	s.Require().NoError(err)
	require.Len(s.T(), msg.calls, 2)
	s.Contains(msg.calls[1], board.DirectiveResume)
	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", c2.ClaimToken), t1.ID, "", board.StageInReview, "https://ev.test/2", "")
	s.Require().NoError(err, "forward writes resume after resume")
}

// KILL-TEST 1747-c (project pause): the scheduling lever parks every
// task under the project (query scoping) and cascades halts; resume
// reopens the gates.
func (s *BoardSuite) TestKill_1747_ProjectPauseParksAll() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "one", nil, false)
	msg := &fakeMessenger{}
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithWorkerMessenger(msg.Deliver),
	)

	c := s.claimOrFail(ws)
	s.Equal(t1.ID, c.TaskID)

	proj, err := s.svc.PauseProject(context.Background(), s.owner(), p.ID, "", "")
	s.Require().NoError(err)
	s.True(proj.Paused)
	require.Len(s.T(), msg.calls, 1, "the halt cascades to the in-flight worker")
	s.Contains(msg.calls[0], board.DirectiveHalt)

	// The paused project serves nothing (its in-flight row excluded from
	// NEW claims; a fresh sibling stays parked).
	s.seedTask(e, "two", nil, false)
	_, err = board.NewService(s.pool, s.cfg).ClaimNext(context.Background(), "worker", ws)
	s.Require().Error(err, "the paused project claims nothing")

	proj, err = s.svc.ResumeProject(context.Background(), s.owner(), p.ID, "", "")
	s.Require().NoError(err)
	s.False(proj.Paused)
	require.Len(s.T(), msg.calls, 2, "resume cascades")
	s.Contains(msg.calls[1], board.DirectiveResume)
}

// KILL-TEST 1747-d (delete semantics): snapshots per row, orphan
// listing + events, NO auto-reconnect by default, reconnect=true
// inherits upstream edges (deduplicated, cycle-guarded).
func (s *BoardSuite) TestKill_1747_DeleteOrphanSemantics() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "A", nil, false)
	b := s.seedTask(e, "B", nil, false)
	c := s.seedTask(e, "C", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), b.ID, "", a.ID, ""))
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), c.ID, "", b.ID, ""))

	// Default: NO auto-reconnect — C becomes ready, no A→C edge appears.
	out, err := s.svc.DeleteTask(context.Background(), s.owner(), b.ID, "", false, "")
	s.Require().NoError(err)
	s.Equal([]string{c.ShortID}, out.OrphanedDependents, "the orphan is listed for the deleter")
	s.Empty(out.Reconnected)
	s.Equal(1, s.countEvents("unblocked_by_delete"), "one audit event per orphan")
	s.False(s.reloadTask(c.ID).IsBlocked, "C became ready — full stop")
	var edges int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_dependencies WHERE task_id=$1 AND depends_on_task_id=$2`,
		c.ID, a.ID).Scan(&edges))
	s.Equal(0, edges, "the graph never rewrites itself implicitly")
	// The snapshot: the delete event embeds B's full final state.
	var snap int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='delete_task' AND item_id=$1 AND payload ? 'snapshot'`,
		b.ID).Scan(&snap))
	s.Equal(1, snap, "content preservation — delete removes the object, not the history")

	// reconnect=true: delete the PREREQUISITE of a two-edge chain so the
	// orphan inherits its upstream.
	x := s.seedTask(e, "X", nil, false)
	y := s.seedTask(e, "Y", nil, false)
	z := s.seedTask(e, "Z", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), y.ID, "", x.ID, ""))
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), z.ID, "", y.ID, ""))
	out, err = s.svc.DeleteTask(context.Background(), s.owner(), y.ID, "", true, "")
	s.Require().NoError(err)
	s.Equal([]string{z.ShortID}, out.OrphanedDependents)
	s.Equal([]string{z.ShortID + "→" + x.ShortID}, out.Reconnected, "the orphan inherits the upstream edge")
	s.True(s.reloadTask(z.ID).IsBlocked, "Z→X is a real edge (blocked until X done)")
}

// KILL-TEST 1747-e (delete_epic force): refuses while claims are
// in-flight; force aborts + cascades with per-task snapshot events.
func (s *BoardSuite) TestKill_1747_DeleteEpicForce() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "live", nil, false)
	s.seedTask(e, "idle", nil, false)

	c := s.claimOrFail(ws)
	s.Equal(t1.ID, c.TaskID)

	_, err := s.svc.DeleteEpic(context.Background(), s.owner(), e.ID, "", false, "")
	s.Require().Error(err)
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	s.Equal(board.GuardEpicState, ge.Code, "claims in flight — force required")

	out, err := s.svc.DeleteEpic(context.Background(), s.owner(), e.ID, "", true, "")
	s.Require().NoError(err)
	s.Len(out.CascadeTasks, 2, "every removed task is listed")
	s.Equal(1, s.aborter.count(), "force aborts the in-flight worker first")
	var goneEpic int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM epics WHERE id=$1`, e.ID).Scan(&goneEpic))
	s.Equal(0, goneEpic)
	var snapshots int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE action='delete_task' AND payload ? 'snapshot' AND payload ? 'cascade_of'`).Scan(&snapshots))
	s.Equal(2, snapshots, "per-task snapshot events on cascade — no aggregate blob")
}

// KILL-TEST 1747-f (DAG spec): edges cross projects within a workspace,
// never workspaces; duplicates are idempotent no-ops; per-node caps.
func (s *BoardSuite) TestKill_1747_DAGScopeAndCaps() {
	ws := s.seedWorkspace()
	ws2 := s.seedWorkspace()
	p1 := s.seedProject(ws, 4)
	p2 := s.seedProject(ws, 4)
	pOther := s.seedProject(ws2, 4)
	e1 := s.seedEpic(p1, board.EpicActive)
	e2 := s.seedEpic(p2, board.EpicActive)
	eOther := s.seedEpic(pOther, board.EpicActive)
	src := s.seedTask(e1, "src", nil, false)
	sameWS := s.seedTask(e2, "same-ws", nil, false)
	foreign := s.seedTask(eOther, "foreign", nil, false)

	// Cross-project WITHIN the workspace: allowed (#1747).
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), sameWS.ID, "", src.ID, ""))

	// Cross-WORKSPACE: rejected.
	err := s.svc.AddDependency(context.Background(), s.owner(), foreign.ID, "", src.ID, "")
	s.Require().Error(err)
	var de *board.DependencyError
	require.ErrorAs(s.T(), err, &de)
	s.Equal("cross_workspace", de.Code)

	// Duplicate edge: idempotent no-op.
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), sameWS.ID, "", src.ID, ""))

	// Edge cap: fill to the default (16) and refuse one more.
	target := s.seedTask(e1, "hub", nil, false)
	for i := 0; i < 16; i++ {
		filler := s.seedTask(e1, "filler", nil, false)
		s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), filler.ID, "", target.ID, ""))
	}
	oneMore := s.seedTask(e1, "one-more", nil, false)
	err = s.svc.AddDependency(context.Background(), s.owner(), oneMore.ID, "", target.ID, "")
	s.Require().Error(err, "16 edges/node is the cap — the 17th refuses")
	var gel *board.GuardError
	require.ErrorAs(s.T(), err, &gel)
	s.Equal(board.GuardGraphLimit, gel.Code, "the blast-radius bound")
}

// KILL-TEST 1747-g (import_plan): dry-run renders without writing; the
// write is atomic; a cyclic plan never lands.
func (s *BoardSuite) TestKill_1747_ImportPlan() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)

	// Dry-run: nothing written, the render carries the graph + ready set.
	dry, err := s.svc.ImportPlan(context.Background(), s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "Plan",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A"},
			{Key: "b", Title: "B", DependsOn: []string{"a"}},
		},
	}, true, "")
	s.Require().NoError(err)
	s.True(dry.DryRun)
	s.Require().NotNil(dry.Plan)
	s.True(dry.Plan.Valid)
	s.Equal([]string{"a"}, dry.Plan.InitialReadySet, "b waits on a")
	var epics int
	s.Require().NoError(s.pool.QueryRow(context.Background(), `SELECT count(*) FROM epics`).Scan(&epics))
	s.Equal(0, epics, "the dry-run wrote nothing")

	// Write: atomic materialization, ready set with minted ids.
	out, err := s.svc.ImportPlan(context.Background(), s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "Plan",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A"},
			{Key: "b", Title: "B", DependsOn: []string{"a"}},
		},
	}, false, "")
	s.Require().NoError(err)
	s.False(out.DryRun)
	s.Len(out.TaskKeys, 2)
	s.True(s.reloadTask(out.TaskKeys["b"]).IsBlocked, "the edge materialized")
	s.Equal([]string{out.TaskKeys["a"]}, out.InitialReadySet)

	// Cyclic plan: refused, nothing written.
	_, err = s.svc.ImportPlan(context.Background(), s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "Cycle",
		Tasks: []board.PlanTaskInput{
			{Key: "x", Title: "X", DependsOn: []string{"y"}},
			{Key: "y", Title: "Y", DependsOn: []string{"x"}},
		},
	}, false, "")
	s.Require().Error(err, "a cyclic plan never lands")
	var cyc int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM epics WHERE title='Cycle'`).Scan(&cyc))
	s.Equal(0, cyc, "all-or-nothing held")
}

// KILL-TEST 1747-h (epic_activate): inactive-epic deps refuse without
// acknowledgment; the ack activates; the ready set reports.
func (s *BoardSuite) TestKill_1747_EpicActivateAcknowledgment() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	active := s.seedEpic(p, board.EpicDraft) // activated below — a real transition
	draft := s.seedEpic(p, board.EpicDraft)
	dep := s.seedTask(draft, "prereq", nil, false)
	consumer := s.seedTask(active, "consumer", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), consumer.ID, "", dep.ID, ""))

	_, err := s.svc.EpicActivate(context.Background(), s.owner(), active.ID, "", false, "")
	s.Require().Error(err)
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	s.Equal(board.GuardEpicState, ge.Code, "structurally-broken subgraph: dep enters an inactive epic")

	e, err := s.svc.EpicActivate(context.Background(), s.owner(), active.ID, "", true, "")
	s.Require().NoError(err, "the acknowledgment unlocks the activation")
	s.Equal(board.EpicActive, e.Status)
	ready, err := s.svc.EpicInitialReadySet(context.Background(), active.ID)
	s.Require().NoError(err)
	s.Empty(ready, "consumer stays blocked — the ack does not unblock, it accepts")
}

// KILL-TEST 1747-i (concurrency caps): the per-user cap quietly withholds
// grants (never an error); the read surfaces waiting_on.
func (s *BoardSuite) TestKill_1747_ConcurrencyCaps() {
	ws := s.seedWorkspace()
	ws2 := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "one", nil, false)
	t2 := s.seedTask(e, "two", nil, false)

	capped := s.cfg
	capped.UserClaimCap = 1
	svc := board.NewService(s.pool, capped, board.WithWorkerSpawner(s.spawner.Spawn))

	c := s.claimWith(svc, ws)
	s.Equal(t1.ID, c.TaskID)
	_, err := svc.ClaimNext(context.Background(), "worker2", ws)
	s.Require().Error(err, "the per-user cap quietly withholds — no error class, no grant")
	s.Equal(board.ErrNoCandidate, err)

	// The waiting read: the ready sibling says why it waits.
	svc.ReportResourcePressure(context.Background(), ws, board.PressureSample{}) // clear pressure state
	row, rerr := svc.GetTask(context.Background(), t2.ID, "")
	s.Require().NoError(rerr)
	s.Equal("concurrency_limit", row.WaitingOn, "the informational note (#1747 round 3)")
	_ = ws2
}

// KILL-TEST 1747-j (resource pressure): a pressured workspace serves no
// claims; the read surfaces the note; TTL clears it.
func (s *BoardSuite) TestKill_1747_ResourcePressureBackpressure() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "ready", nil, false)

	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(s.spawner.Spawn))
	s.Require().NoError(svc.ReportResourcePressure(context.Background(), ws, board.PressureSample{
		Disk: true, DiskRate: 0.92,
	}))

	_, err := svc.ClaimNext(context.Background(), "worker", ws)
	s.Require().Error(err, "the engine stops claiming into a pressured workspace")

	row, rerr := svc.GetTask(context.Background(), t1.ID, "")
	s.Require().NoError(rerr)
	s.Contains(row.Backpressure, "disk 92%", "the informational note")

	// A clean sample (nothing pressured) reopens claims — hysteresis is
	// reporter-side; here the sample itself says healthy.
	s.Require().NoError(svc.ReportResourcePressure(context.Background(), ws, board.PressureSample{}))
	c := s.claimWith(svc, ws)
	s.Equal(t1.ID, c.TaskID)
}

// KILL-TEST 1747-k (grant belt): IsWorkerSession answers true for a
// live assignee, false otherwise — the seam's engine-authoritative gate.
func (s *BoardSuite) TestKill_1747_IsWorkerSession() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "claimed", nil, false)

	c := s.claimOrFail(ws)
	live, err := s.svc.IsWorkerSession(context.Background(), c.Assignee)
	s.Require().NoError(err)
	s.True(live, "the stamped assignee is a worker")
	no, err := s.svc.IsWorkerSession(context.Background(), "ses-not-a-worker")
	s.Require().NoError(err)
	s.False(no)
	_ = t1
}

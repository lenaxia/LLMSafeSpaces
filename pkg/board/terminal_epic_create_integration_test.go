// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1948 (integration tier): creation into a terminal epic refuses LOUD
// through the REAL service write path. Live evidence: a task created
// into an auto-completed epic sat `pending`/unfed for 20+ minutes — no
// error, no audit signal — because the claim set gates on
// e.status='active' (candidateSQL) while create gated on nothing.
//
// Terminal here = completed and archived (no path back into the
// claimable set without an explicit lifecycle move: reopen /
// unarchive). Paused is NOT terminal — pause is a scheduling lever and
// resume re-feeds, so creation stays green (the trap a naive guard
// falls into). Draft is not terminal either — create-then-activate is
// the documented flow. The archived-project twin refuses the same way
// (eligibleProjectsSQL gates on p.status='active'; engine precedent:
// task_start's own "terminal storage stays closed").
//
// import_plan mints a FRESH draft epic inside one transaction, so a
// terminal TARGET epic is structurally unreachable there; its terminal
// target is the project (archived). Pins: the whole transaction
// refuses before any write (nothing strands), and a mid-payload
// failure (the import_plan.effect failpoint, armed between effect and
// marker) rolls back epic + tasks + edges — all-or-nothing holds.

func (s *BoardSuite) TestGuard_1948_CreateIntoCompletedEpicRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicCompleted)

	before := s.countEvents("create_task")
	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "starved row", Description: "would sit unfed",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err, "create into a completed epic must refuse loud (#1948)")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardEpicState, ge.Code)
	require.Contains(s.T(), err.Error(), e.ShortID, "the refusal names the epic")
	require.Contains(s.T(), err.Error(), board.EpicCompleted, "the refusal names the epic's state")

	// Nothing landed: no task row, no audit event (a silent partial
	// write would be a second starvation channel).
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	require.Zero(s.T(), n, "no task row may land from a refused create")
	require.Equal(s.T(), before, s.countEvents("create_task"), "no create_task audit event from the refusal")
}

func (s *BoardSuite) TestGuard_1948_CreateIntoArchivedEpicRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicArchived)

	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "storage row", Description: "terminal storage stays closed",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err, "create into an archived epic must refuse (same silent-starvation shape)")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardEpicState, ge.Code)
	require.Contains(s.T(), err.Error(), e.ShortID)
	require.Contains(s.T(), err.Error(), board.EpicArchived)

	// Symmetry with the completed-epic pin: nothing landed, nothing
	// audited (r1 review: the cheap twin the completed pin already
	// asserted — the silent-partial-write channel).
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	require.Zero(s.T(), n, "no task row may land from a refused create")
	require.Zero(s.T(), s.countEvents("create_task"), "no create_task audit event from the refusal")
}

func (s *BoardSuite) TestGuard_1948_CreateIntoActiveEpicGreen() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	t, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "healthy backlog row", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	s.Require().NoError(err, "create into an ACTIVE epic stays green")
	require.Equal(s.T(), board.StageBacklog, t.Stage)
}

func (s *BoardSuite) TestGuard_1948_CreateIntoPausedEpicGreen() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	// Paused is a scheduling lever, not a lifecycle terminal: the row
	// waits for resume, which re-feeds it (board_epic_resume). A naive
	// "not active → refuse" guard would break this pin.
	e := s.seedEpic(p, board.EpicPaused)

	t, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "deferred row", Description: "waits for epic resume",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	s.Require().NoError(err, "create into a PAUSED epic must stay green (pause is a scheduling lever)")
	require.Equal(s.T(), board.StageBacklog, t.Stage)
}

func (s *BoardSuite) TestGuard_1948_CreateIntoArchivedProjectRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	_, err := s.svc.ArchiveProject(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)

	_, err = s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "project-terminal row", Description: "archived project starves every epic",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err, "create into an archived project's epic must refuse (the project is outside the claimable set)")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardProjectState, ge.Code)
	require.Contains(s.T(), err.Error(), p.ShortID)
}

func (s *BoardSuite) TestGuard_1948_ImportPlanIntoArchivedProjectRefusesWhole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	_, err := s.svc.ArchiveProject(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)

	var epicsBefore, tasksBefore int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsBefore))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id=$1`, p.ID).Scan(&tasksBefore))

	in := board.PlanInput{EpicTitle: "terminal target", Outcome: "never lands",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
			{Key: "b", Title: "B", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
			{Key: "c", Title: "C", Description: "charter", DependsOn: []string{"a", "b"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		}}
	_, err = s.svc.ImportPlan(ctx, s.owner(), p.ID, "", in, false, "")
	require.Error(s.T(), err, "import_plan into an archived project must refuse the WHOLE transaction")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardProjectState, ge.Code)
	require.Contains(s.T(), err.Error(), p.ShortID)

	// All-or-nothing from the guard: the refusal precedes any write —
	// no epic, no tasks, nothing stranded.
	var epicsAfter, tasksAfter int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsAfter))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id=$1`, p.ID).Scan(&tasksAfter))
	require.Equal(s.T(), epicsBefore, epicsAfter, "no epic may strand from a refused import_plan")
	require.Equal(s.T(), tasksBefore, tasksAfter, "no task may strand from a refused import_plan")
}

func (s *BoardSuite) TestGuard_1948_ImportPlanMidPayloadFailureRollsBackWhole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	var epicsBefore int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsBefore))

	// The crash-between-action-and-effect seam, armed at import_plan's
	// own effect failpoint: epic + tasks + edges have been INSERTed
	// inside the open transaction — the abort must take ALL of them.
	s.fps.armAt("import_plan.effect", &board.GuardError{Code: board.GuardInput, Detail: "armed fault: crash between effect and marker"})
	defer s.fps.disarm("import_plan.effect")

	in := board.PlanInput{EpicTitle: "all-or-nothing", Outcome: "ship",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
			{Key: "b", Title: "B", Description: "charter", DependsOn: []string{"a"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		}}
	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", in, false, "")
	require.Error(s.T(), err, "the armed failpoint must fail the import")

	// All-or-nothing (§ atomicity): mid-payload failure strands NOTHING.
	var epicsAfter, tasksAfter, edgesAfter int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsAfter))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id=$1`, p.ID).Scan(&tasksAfter))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_dependencies d JOIN tasks t ON t.id=d.task_id
		 WHERE t.project_id=$1`, p.ID).Scan(&edgesAfter))
	require.Equal(s.T(), epicsBefore, epicsAfter, "no epic may strand from a mid-payload failure")
	require.Zero(s.T(), tasksAfter, "no task may strand from a mid-payload failure")
	require.Zero(s.T(), edgesAfter, "no edge may strand from a mid-payload failure")
}

func (s *BoardSuite) TestGuard_1948_ImportPlanDryRunIntoArchivedProjectRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	_, err := s.svc.ArchiveProject(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)

	// r1 review pin: the guard precedes the dry-run branch — the
	// plan-approval moment never blesses an unlandable plan (a
	// regression that moves the guard below `if dryRun` reddens here).
	out, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "unapprovable", Outcome: "never lands",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, true, "")
	require.Error(s.T(), err, "dry-run into an archived project refuses — no approval moment for a terminal target")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardProjectState, ge.Code)
	s.Require().Nil(out)
}

func (s *BoardSuite) TestGuard_1948_CreateEpicIntoArchivedProjectRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	_, err := s.svc.ArchiveProject(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)

	// r2 review pin: the last unguarded creation seam of this class —
	// a draft epic minted into an archived project would sit outside
	// the claimable set with no signal, one seam over create_task.
	before := s.countEvents("create_epic")
	_, err = s.svc.CreateEpic(ctx, s.owner(), p.ID, "",
		board.EpicInput{Title: "starved epic", Outcome: "never feeds"}, "")
	require.Error(s.T(), err, "create_epic into an archived project must refuse loud")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardProjectState, ge.Code)
	require.Contains(s.T(), err.Error(), p.ShortID)
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&n))
	require.Zero(s.T(), n, "no epic row may land from a refused create_epic")
	require.Equal(s.T(), before, s.countEvents("create_epic"))
}

func (s *BoardSuite) TestGuard_1948_ConcurrentTerminalizationStillRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// r1 review pin: the pre-write guard is check-then-act — the epic
	// terminalizes BETWEEN the pre-check and the in-transaction
	// re-check (the create_task.recheck failpoint fires there), and
	// the locked re-read must still refuse. This is the deterministic
	// form of racing epicCompletionCheck against CreateTask.
	s.fps.armAtFn("create_task.recheck", func() error {
		_, err := s.pool.Exec(context.Background(),
			`UPDATE epics SET status=$1 WHERE id=$2`, board.EpicCompleted, e.ID)
		return err
	})
	defer s.fps.disarm("create_task.recheck")

	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "raced row", Description: "terminalized mid-flight",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err, "a mid-flight terminalization must still refuse (in-tx re-check)")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardEpicState, ge.Code)
	require.Contains(s.T(), err.Error(), e.ShortID)
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	require.Zero(s.T(), n, "the raced create landed nothing")
}

func (s *BoardSuite) TestGuard_1948_ImportPlanConcurrentArchiveStillRefusesWhole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	var epicsBefore int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsBefore))

	// r1 review pin: the project archives BETWEEN the pre-check and
	// the in-tx re-check (the import_plan.recheck failpoint fires
	// there) — the whole transaction aborts, nothing strands.
	s.fps.armAtFn("import_plan.recheck", func() error {
		_, err := s.pool.Exec(context.Background(),
			`UPDATE projects SET status=$1 WHERE id=$2`, board.ProjectArchived, p.ID)
		return err
	})
	defer s.fps.disarm("import_plan.recheck")

	in := board.PlanInput{EpicTitle: "raced plan", Outcome: "never lands",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
			{Key: "b", Title: "B", Description: "charter", DependsOn: []string{"a"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		}}
	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", in, false, "")
	require.Error(s.T(), err, "a mid-flight archive must still fail the whole import")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardProjectState, ge.Code)
	var epicsAfter, tasksAfter int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&epicsAfter))
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id=$1`, p.ID).Scan(&tasksAfter))
	require.Equal(s.T(), epicsBefore, epicsAfter, "no epic may strand from the raced import")
	require.Zero(s.T(), tasksAfter, "no task may strand from the raced import")
}

func (s *BoardSuite) TestGuard_1948_ImportPlanMintsDraftEpicGreen() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	out, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "fresh epic", Outcome: "ship",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "A", Description: "charter", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	s.Require().NoError(err, "import_plan into an active project stays green")
	require.Equal(s.T(), board.EpicDraft, s.epicStatus(out.EpicID),
		"import_plan's minted epic lands draft — a terminal TARGET epic is structurally unreachable on this path")
}

func (s *BoardSuite) TestGuard_1948_RefusalNeverTurnsAReplayIntoAnError() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// The original create lands while the epic is open.
	original, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "landed before terminal", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "req-1948-replay")
	s.Require().NoError(err)

	// The epic goes terminal AFTER the write (the #1747 auto-complete
	// shape): the replayed requestId must render the ORIGINAL outcome
	// (replay-first, then validate — projectLifecycle's discipline), not
	// the guard.
	_, err = s.pool.Exec(ctx, `UPDATE epics SET status=$1 WHERE id=$2`, board.EpicCompleted, e.ID)
	s.Require().NoError(err)
	replayed, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "landed before terminal", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "req-1948-replay")
	s.Require().NoError(err, "a replay renders the original outcome, never the terminal-target guard")
	require.Equal(s.T(), original.ID, replayed.ID)
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	require.Equal(s.T(), 1, n, "the replay created nothing")

	// A FRESH request into the now-terminal epic still refuses loud.
	_, err = s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "after terminal", Description: "charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err, "only replays bypass the guard — fresh creates still refuse")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardEpicState, ge.Code)
}

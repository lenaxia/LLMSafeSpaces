// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// plan_attach_integration_test.go — design 0070 §3: import_plan's
// EXISTING-EPIC arm (epicId). The unified plan document attaches 1..N
// tasks to a resolved epic in the same all-or-nothing transaction that
// the create arm uses — the epic-create step is skipped, and the
// ownership/terminal-state guards mirror the create-task path's.
// Pins: the happy path attaches; both epic arms refuse 400; neither
// refuses 400; a bad task in the array leaves the existing epic
// UNTOUCHED (no partial attach); a terminal epic refuses the whole
// plan (the #1948 starvation guard's attach arm).

func (s *BoardSuite) TestPlan_0069_AttachToExistingEpic() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	out, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "t1", Title: "T1", Description: "d1",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}}},
			{Key: "t2", Title: "T2", Description: "d2", DependsOn: []string{"t1"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c2"}}},
		},
	}, false, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), e.ID, out.EpicID, "the attach arm targets the existing epic")
	assert.Equal(s.T(), e.ShortID, out.EpicShortID)
	require.Len(s.T(), out.TaskKeys, 2)

	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	assert.Equal(s.T(), 2, n, "both tasks attached to the existing epic")
	// The edge landed too (t2 depends on t1 by payload key).
	s.Require().NoError(s.pool.QueryRow(ctx, `
		SELECT count(*) FROM board_dependencies d
		JOIN tasks a ON a.id = d.task_id
		JOIN tasks b ON b.id = d.depends_on_task_id
		WHERE a.epic_id=$1 AND a.title='T2' AND b.title='T1'`, e.ID).Scan(&n))
	assert.Equal(s.T(), 1, n, "the dependsOn edge resolved through the payload keys")
	// Epic count unchanged — the attach arm creates NO epic.
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(&n))
	assert.Equal(s.T(), 1, n, "no new epic minted in attach mode")
}

func (s *BoardSuite) TestPlan_0069_BothEpicArmsRefuse() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	before := s.countEvents("import_plan")
	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		Epic:   &board.PlanEpicSpec{Title: "E"},
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "k", Title: "T", Description: "d",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	require.Error(s.T(), err)
	ge, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	require.Equal(s.T(), board.GuardInput, ge.Code)
	assert.Contains(s.T(), err.Error(), "not both")

	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	assert.Zero(s.T(), n, "nothing attached from a refused arm")
	assert.Equal(s.T(), before, s.countEvents("import_plan"), "no audit event")
}

func (s *BoardSuite) TestPlan_0069_NeitherEpicArmRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		Tasks: []board.PlanTaskInput{
			{Key: "k", Title: "T", Description: "d",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	require.Error(s.T(), err)
	ge, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	require.Equal(s.T(), board.GuardInput, ge.Code)
	assert.Contains(s.T(), err.Error(), "neither is present")
}

func (s *BoardSuite) TestPlan_0069_AttachAtomicity() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	seeded := s.seedTask(e, "existing", nil, false)

	// A bad task mid-array (dependsOn names an unknown payload key)
	// refuses the WHOLE attach — the epic keeps exactly its prior task.
	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "ok", Title: "OK", Description: "d",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
			{Key: "bad", Title: "Bad", Description: "d", DependsOn: []string{"missing-key"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	require.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "missing-key")

	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&n))
	assert.Equal(s.T(), 1, n, "all-or-nothing: the earlier good node left no partial row")
	var seededTitle string
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT title FROM tasks WHERE id=$1`, seeded.ID).Scan(&seededTitle))
	assert.Equal(s.T(), "existing", seededTitle, "the existing epic's tasks are untouched")
}

func (s *BoardSuite) TestPlan_0069_AttachToTerminalEpicRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicCompleted)

	_, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "k", Title: "starved", Description: "would sit unfed",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	require.Error(s.T(), err, "attaching to a terminal epic must refuse loud (#1948's attach arm)")
	ge, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	require.Equal(s.T(), board.GuardEpicState, ge.Code)

	// The dry run refuses the same way — the approval moment never
	// blesses a starved attach.
	_, err = s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "k", Title: "starved", Description: "would sit unfed",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, true, "")
	require.Error(s.T(), err)
}

func (s *BoardSuite) TestPlan_0069_AttachForeignProjectEpicRefuses() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	other := s.seedProject(ws, 2)
	e := s.seedEpic(other, board.EpicActive)

	_, err := s.svc.ImportPlan(context.Background(), s.owner(), p.ID, "", board.PlanInput{
		EpicID: e.ID,
		Tasks: []board.PlanTaskInput{
			{Key: "k", Title: "T", Description: "d",
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}},
		},
	}, false, "")
	require.Error(s.T(), err, "an epic from another project cannot host this project's plan")
	ge, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	require.Equal(s.T(), board.GuardInput, ge.Code)
	assert.Contains(s.T(), err.Error(), "another project")
}

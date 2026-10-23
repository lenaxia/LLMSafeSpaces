// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1849 items 1+6 (integration tier): the seed guard through the REAL
// service write path — a create without a charter or without criteria
// refuses loudly; a placeholder-class charter refuses; a complete
// create stores. Plan dry-run refuses a seedless plan task the same
// way (import_plan materializes tasks; the approval moment must never
// bless a seedless node).

func (s *BoardSuite) TestSeed_1849_CreateRequiresCharterAndCriteria() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)

	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "no charter"}, "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "description")

	_, err = s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "no criteria", Description: "charter"}, "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "criterion")

	_, err = s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "placeholder charter", Description: "  PLACEHOLDER ",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c"}}}, "")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "placeholder")

	t, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "complete", Description: "a real charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "done"}}}, "")
	s.Require().NoError(err)
	require.Equal(s.T(), board.StageBacklog, t.Stage)
}

func (s *BoardSuite) TestSeed_1849_PlanRefusesSeedlessNodes() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	out, err := s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "seedless plan", Outcome: "ship",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "no description"},
		},
	}, true, "")
	s.Require().NoError(err, "dry-run renders the refusal as an outcome")
	require.False(s.T(), out.Valid)
	require.Contains(s.T(), out.Error, "description")

	out, err = s.svc.ImportPlan(ctx, s.owner(), p.ID, "", board.PlanInput{
		EpicTitle: "criteria-less plan", Outcome: "ship",
		Tasks: []board.PlanTaskInput{
			{Key: "a", Title: "T", Description: "charter"},
		},
	}, true, "")
	s.Require().NoError(err)
	require.False(s.T(), out.Valid)
	require.Contains(s.T(), out.Error, "criterion")
}

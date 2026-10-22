// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1837 P2b (integration tier): setCriteriaMet safety through the REAL
// service write path (PG harness; skips without TEST_DATABASE_URL).
// The pure decode/bounds matrix lives in criteria_check_test.go.

func (s *BoardSuite) TestCriteria_1837_UncheckViaObjectForm() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)
	t := s.seedTask(e, "uncheck", nil, true) // both criteria met

	// Uncheck criterion 0.
	out, err := s.svc.UpdateTask(ctx, s.owner(), "", t.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: 0, Met: false}}, "")
	s.Require().NoError(err)
	require.False(s.T(), out.AcceptanceCriteria[0].Met, "{index, met:false} UNCHECKS")
	require.True(s.T(), out.AcceptanceCriteria[1].Met, "the sibling flag is untouched")

	// The legacy integer form still checks off.
	out, err = s.svc.UpdateTask(ctx, s.owner(), "", t.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: 0, Met: true}}, "")
	s.Require().NoError(err)
	require.True(s.T(), out.AcceptanceCriteria[0].Met)
}

func (s *BoardSuite) TestCriteria_1837_CombinedUseRefused() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)
	t := s.seedTask(e, "combined", nil, false)

	before, err := s.svc.GetTask(ctx, "", t.ShortID)
	s.Require().NoError(err)

	_, err = s.svc.UpdateTask(ctx, s.owner(), "", t.ShortID,
		board.TaskInput{
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "replacement"}},
			SetCriteriaMet:     &board.CriteriaCheck{Index: 0, Met: true},
		}, "")
	require.Error(s.T(), err, "a replace + a check-off in one payload is refused")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardInput, ge.Code)

	after, err := s.svc.GetTask(ctx, "", t.ShortID)
	s.Require().NoError(err)
	require.Equal(s.T(), before.AcceptanceCriteria, after.AcceptanceCriteria, "nothing stored on refusal")
}

func (s *BoardSuite) TestCriteria_1837_OutOfBoundsRefused() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)
	t := s.seedTask(e, "bounds", nil, false) // 2 criteria

	_, err := s.svc.UpdateTask(ctx, s.owner(), "", t.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: 5, Met: true}}, "")
	require.Error(s.T(), err, "out-of-bounds is a loud error, never a silent no-op")
	require.Contains(s.T(), err.Error(), "bounds")

	_, err = s.svc.UpdateTask(ctx, s.owner(), "", t.ShortID,
		board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: -1, Met: true}}, "")
	require.Error(s.T(), err)
}

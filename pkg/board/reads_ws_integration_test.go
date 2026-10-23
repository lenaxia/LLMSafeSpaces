// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1837 P2a (integration tier): the workspace-scoped agent reads
// against real Postgres — ListTasksInWorkspace / BoardHealthInWorkspace
// are ~190 lines of new SQL that the fake-backed handler tests cannot
// execute (#1864 review round 1).

func (s *BoardSuite) TestReadsWS_1837_ListTasksInWorkspace() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "ws-list-1", nil, false)
	t2 := s.seedTask(e, "ws-list-2", nil, false)
	_ = t2

	tasks, proj, err := s.svc.ListTasksInWorkspace(ctx, ws, p.ShortID, "", "", "")
	s.Require().NoError(err)
	require.Equal(s.T(), p.ID, proj.ID)
	require.Len(s.T(), tasks, 2, "every task in one call")
	require.Equal(s.T(), t1.ShortID, tasks[0].ShortID)

	// Stage filter rides through.
	tasks, _, err = s.svc.ListTasksInWorkspace(ctx, ws, p.ShortID, "", board.StageInProgress, "")
	s.Require().NoError(err)
	require.Empty(s.T(), tasks)

	// A foreign workspace resolves a plain 404 — no disclosure.
	foreign := s.seedWorkspace()
	_, _, err = s.svc.ListTasksInWorkspace(ctx, foreign, p.ShortID, "", "", "")
	require.Error(s.T(), err)
	var nf *board.NotFoundError
	require.ErrorAs(s.T(), err, &nf)
}

func (s *BoardSuite) TestReadsWS_1837_BoardHealthInWorkspace() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "ws-health", nil, false)

	c := s.claimOrFail(ws)
	require.Equal(s.T(), board.StageInProgress, c.Task.Stage)

	h, err := s.svc.BoardHealthInWorkspace(ctx, ws)
	s.Require().NoError(err)
	require.NotEmpty(s.T(), h.ActiveLeases, "the claimed row's lease renders with an expiry")
	found := false
	for _, l := range h.ActiveLeases {
		if l.ShortID == c.ShortID {
			found = true
			require.False(s.T(), l.LeaseExpiresAt.IsZero())
			require.NotEmpty(s.T(), l.AssigneeSession)
		}
	}
	require.True(s.T(), found)
	require.NotEmpty(s.T(), h.ProjectSlots, "per-project slots render")

	// Workspace scope: a sibling workspace's boards are invisible.
	other := s.seedWorkspace()
	h2, err := s.svc.BoardHealthInWorkspace(ctx, other)
	s.Require().NoError(err)
	require.Empty(s.T(), h2.ActiveLeases, "no cross-workspace disclosure")
	require.Empty(s.T(), h2.ProjectSlots)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"strings"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1837 P1 (integration tier): the emission-corruption defenses against
// the REAL service (PG harness; skips without TEST_DATABASE_URL). The
// pure detector matrix lives in textguard_test.go; these pins prove the
// service WRITE PATHS enforce them and that a >4KB charter stores
// byte-identical (the file-backed path's guarantee at the platform tier).

func (s *BoardSuite) TestTextGuard_1837_ServiceRejectsDegenerateRepetition() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)

	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "T", Description: strings.Repeat("Retry the loop step and continue. ", 50)}, "")
	require.Error(s.T(), err, "a 50-fold repeated fragment never reaches storage")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardTextRejected, ge.Code)
	require.Contains(s.T(), err.Error(), "repetition")
}

func (s *BoardSuite) TestTextGuard_1837_ServiceRejectsSpliceArtifact() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)

	desc := "Fix the thing and note the artifact \",\"title\":\"PLACEHOLDER\"} in the row."
	_, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "", board.TaskInput{Title: "T", Description: desc}, "")
	require.Error(s.T(), err, "raw JSON structural splices are never stored silently")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge)
	require.Equal(s.T(), board.GuardTextRejected, ge.Code)
}

func (s *BoardSuite) TestTextGuard_1837_LongLegitCharterRoundTripsByteIdentical() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicDraft)

	// >4KB of legitimate prose — what description_file carries.
	var b strings.Builder
	for i := 0; i < 150; i++ {
		b.WriteString("### Charter section ")
		b.WriteString(strings.Repeat("alpha ", i%5+1))
		b.WriteString("\nThe worker performs the distinct numbered step ")
		b.WriteString(strings.Repeat("beta", i%3+1))
		b.WriteString(" and verifies the outcome.\n")
	}
	charter := b.String()
	require.Greater(s.T(), len(charter), 4096)

	created, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "charter round-trip", Description: charter}, "")
	s.Require().NoError(err)

	stored, err := s.svc.GetTask(ctx, "", created.ShortID)
	s.Require().NoError(err)
	require.Equal(s.T(), charter, stored.Description, "byte-identical round-trip")
}

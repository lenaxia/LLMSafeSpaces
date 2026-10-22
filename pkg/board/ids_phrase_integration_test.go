// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// #1831 phrase-id pins — the DB-backed legs of the board vocabulary
// flip that need the service's real read paths: fresh mints are
// phrase-form, legacy random rows keep resolving through the
// dual-accept window and render short_id_deprecated, and no mint ever
// duplicates a phrase the owner already holds. Runs on the BoardSuite
// harness (TEST_DATABASE_URL; skips when unreachable).
package board_test

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestPhrase_FreshMintsArePhraseForm pins the emission flip through the
// real creation paths: every project/epic/task the service mints carries
// the prefix+phrase shape, and within one owner's fleet no two rows of
// the same kind ever hold the same short id (the owner-wide check).
func (s *BoardSuite) TestPhrase_FreshMintsArePhraseForm() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	s.Require().True(board.IsPhraseIDForm(p.ShortID), "project mint is phrase-form (got %s)", p.ShortID)

	e := s.seedEpic(p, board.EpicActive)
	s.Require().True(board.IsPhraseIDForm(e.ShortID), "epic mint is phrase-form (got %s)", e.ShortID)

	seen := map[string]bool{e.ShortID: true}
	for i := 0; i < 30; i++ {
		tk, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
			board.TaskInput{Title: "t", Description: "d"}, "")
		s.Require().NoError(err)
		s.Require().True(board.IsPhraseIDForm(tk.ShortID), "task mint is phrase-form (got %s)", tk.ShortID)
		s.Require().False(board.IsDeprecatedBoardID(tk.ShortID), "a fresh mint is never deprecated-form")
		if seen[tk.ShortID] {
			s.FailNowf("owner-wide mint uniqueness violated", "%s minted twice in one owner's fleet", tk.ShortID)
		}
		seen[tk.ShortID] = true
	}
}

// TestPhrase_DeprecationMarkingOnReads pins the dual-accept window's
// row marking: a legacy random-form short_id keeps resolving on every
// surface AND renders short_id_deprecated=true through the service
// reads; a phrase-form row renders no flag.
func (s *BoardSuite) TestPhrase_DeprecationMarkingOnReads() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	t1, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "fresh", Description: "d"}, "")
	s.Require().NoError(err)
	s.Require().True(board.IsPhraseIDForm(t1.ShortID))

	// A legacy-fleet row (the pre-#1831 vocabulary) addressed as-is.
	const legacy = "tsk-vlfuvcil"
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tasks (id, project_id, epic_id, short_id, title, description, project_name, epic_name, tags, acceptance_criteria)
		VALUES ($1, $2, $3, $4, 'legacy row', 'd', $5, $6, '{}', '[]')`,
		"lega-0001-"+e.ID[:8], p.ID, e.ID, legacy, p.Name, e.Title)
	s.Require().NoError(err)

	// GetTask by the legacy id: resolves (dual-accept) and the row is
	// marked deprecated.
	got, err := s.svc.GetTask(ctx, "", legacy)
	s.Require().NoError(err, "legacy random ids keep resolving through the window")
	s.Require().True(got.ShortIDDeprecated, "legacy-form row renders short_id_deprecated through GetTask")

	// Case-insensitive resolution still holds for the legacy form.
	gotUpper, err := s.svc.GetTask(ctx, "", "TSK-VLFUVCIL")
	s.Require().NoError(err)
	s.Require().True(gotUpper.ShortIDDeprecated)

	// The phrase-form row carries no flag.
	gotPhrase, err := s.svc.GetTask(ctx, "", t1.ShortID)
	s.Require().NoError(err)
	s.Require().False(gotPhrase.ShortIDDeprecated, "phrase-form row is not deprecated")

	// The list projection marks legacy rows too (the flag is set at
	// scan time — every read path carries it).
	tasks, err := s.svc.ListTasks(ctx, p.ID, "", "")
	s.Require().NoError(err)
	var sawLegacy, sawPhrase bool
	for _, tk := range tasks {
		switch tk.ShortID {
		case legacy:
			sawLegacy = true
			s.Require().True(tk.ShortIDDeprecated, "ListTasks marks the legacy row")
		case t1.ShortID:
			sawPhrase = true
			s.Require().False(tk.ShortIDDeprecated)
		}
	}
	s.Require().True(sawLegacy && sawPhrase, "both fleet rows present in the list")

	// The did-you-mean ranker serves the mixed fleet: a near-miss of
	// the legacy id suggests it (character ranker); a near-miss of the
	// phrase id suggests it (word ranker) — pinned at unit tier; here
	// the resolution miss itself carries suggestions in-scope.
	_, err = s.svc.GetTask(ctx, "", "tsk-meek-pony-rides")
	s.Require().Error(err)
	var nf *board.NotFoundError
	s.Require().ErrorAs(err, &nf)
}

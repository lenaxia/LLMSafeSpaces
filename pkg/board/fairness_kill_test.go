// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Fairness / starvation kill-tests (issue #1698, design 0067 §4.4 DRR +
// §6 r5/r6 pins). Deficit round robin is mechanized in SQL — these tests
// kill any impl that lets one epic or project lock-step starve another, or
// that loops unboundedly on round rollover.
package board_test

import (
	"context"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// finishClaim runs a claimed task to done (checking off its criteria if
// the fixture did not pre-met them, evidence supplied) so the next claim
// sees fresh candidates.
func (s *BoardSuite) finishClaim(c *board.Claim) {
	ctx := context.Background()
	t := s.reloadTask(c.TaskID)
	crit := t.AcceptanceCriteria
	for i := range crit {
		crit[i].Met = true
	}
	if len(crit) == 0 {
		crit = []board.AcceptanceCriterion{{Text: "done", Met: true}}
	}
	_, err := s.svc.UpdateTask(ctx, s.owner(), c.TaskID, "",
		board.TaskInput{Title: t.Title, AcceptanceCriteria: crit}, "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(ctx, s.agent("sess", c.ClaimToken), c.TaskID, "",
		board.StageDone, "https://evidence.example/"+c.ShortID, "")
	s.Require().NoError(err)
}

// KILL-TEST 12 (engine pin "DRR starvation"): two epics in one project, one
// aggressive (many tasks) — across K grants NEITHER starves: quantum=1
// forces strict alternation (vft stays within one of each other).
func (s *BoardSuite) TestKill_DRRStarvationTwoEpics() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 8)
	aggressive := s.seedEpic(p, board.EpicActive)
	quiet := s.seedEpic(p, board.EpicActive)
	for i := 0; i < 8; i++ {
		s.seedTask(aggressive, fmt.Sprintf("a%d", i), nil, true)
	}
	for i := 0; i < 6; i++ {
		s.seedTask(quiet, fmt.Sprintf("q%d", i), nil, true)
	}

	for i := 0; i < 6; i++ {
		c := s.claimOrFail(ws)
		s.finishClaim(c)
	}
	aVFT, aDef, _ := s.epicDRR(aggressive.ID)
	qVFT, qDef, _ := s.epicDRR(quiet.ID)
	s.Equal(int64(3), aVFT, "aggressive epic served 3 of 6 grants")
	s.Equal(int64(3), qVFT, "quiet epic served 3 of 6 grants — no starvation")
	// After an EVEN number of grants the round is mid-flight: each epic
	// holds its consumed quantum until the next rollover.
	s.Equal(int64(1), aDef)
	s.Equal(int64(1), qDef)
}

// KILL-TEST 13 (engine pin "DRR quantum-skip"): an epic at deficit >=
// quantum is skipped until the round completes.
func (s *BoardSuite) TestKill_QuantumSkip() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e1 := s.seedEpic(p, board.EpicActive)
	e2 := s.seedEpic(p, board.EpicActive)
	s.seedTask(e1, "one", nil, false)
	s.seedTask(e2, "two", nil, false)

	c1 := s.claimOrFail(ws)
	s.Equal(e1.ID, c1.Task.EpicID, "priority tie → least vft → created_at")
	// e1 is now at deficit 1 = quantum 1: the next claim must skip e1.
	c2 := s.claimOrFail(ws)
	s.Equal(e2.ID, c2.Task.EpicID, "epic at quota is skipped until round rollover")
	_, d1, _ := s.epicDRR(e1.ID)
	s.Equal(int64(1), d1, "e1's deficit holds at quota while in flight")
}

// KILL-TEST 14 (engine pin "two-level rollover"): (a) project-level — a
// claimer that locked the project but found no candidate resets THAT
// project's deficits and retries once; (b) dispatcher-level — an empty p
// CTE while at-quota epics with candidates exist resets every deficit in
// scope and retries once. Bounded: one reset per empty claim, never a loop.
func (s *BoardSuite) TestKill_TwoLevelRollover() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "only", nil, true)

	// (a) project-level emptiness: take the only candidate; the next
	// claimer reports a clean empty with NO spurious reset (at-quota
	// epics WITHOUT claimable candidates never trigger the dispatcher
	// reset — bounded: one reset per empty claim, never a loop).
	c1 := s.claimOrFail(ws)
	s.Equal(t1.ID, c1.TaskID)
	s.expectNoCandidate(ws)
	_, def, _ := s.epicDRR(e.ID)
	s.Equal(int64(1), def, "no reset without claimable candidates — the reset is bounded by the at-quota precondition")
	s.finishClaim(c1)

	// (b) dispatcher-level: two projects, every epic at quota (deficit=
	// quantum), but a claimable candidate exists → the empty p CTE
	// triggers the scope-wide reset and the retry grants.
	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 4)
	e2 := s.seedEpic(p2, board.EpicActive)
	t2 := s.seedTask(e2, "second-project", nil, false)

	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM epics WHERE project_id=$1`, p.ID).Scan(new(int)))
	_, err := s.pool.Exec(context.Background(),
		`UPDATE epics SET deficit=quantum, vft=vft+10 WHERE id=$1`, e.ID)
	s.Require().NoError(err) // e (project p) at quota, candidate t1 done — no candidates
	_, err = s.pool.Exec(context.Background(),
		`UPDATE epics SET deficit=quantum WHERE id=$1`, e2.ID)
	s.Require().NoError(err) // e2 at quota WITH a claimable candidate
	_ = t2

	c3 := s.claimOrFail(ws2)
	s.Equal(t2.ID, c3.TaskID, "dispatcher-level rollover resets at-quota epics and grants on the retry")
	_, def2, _ := s.epicDRR(e2.ID)
	s.Equal(int64(1), def2, "the granting claim consumed one unit after the reset")
}

// KILL-TEST 15 (#1747 final ruling — the FIFO pin): claim order is
// deterministic FIFO (creation order) over the ready set — the ONLY
// ordering rule, implemented in exactly ONE place (pkg/board
// candidateSQL). No priority field exists; a future ordering lever
// changes that ORDER BY alone.
func (s *BoardSuite) TestKill_ClaimOrderFIFO() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	first := s.seedTask(e, "first", nil, false)
	second := s.seedTask(e, "second", nil, false)
	third := s.seedTask(e, "third", nil, false)

	c := s.claimOrFail(ws)
	s.Equal(first.ID, c.TaskID, "creation order wins — dependency-only FIFO (#1747)")
	s.finishClaim(c)
	c2 := s.claimOrFail(ws)
	s.Equal(second.ID, c2.TaskID)
	s.finishClaim(c2)
	c3 := s.claimOrFail(ws)
	s.Equal(third.ID, c3.TaskID)
}

// KILL-TEST 16 (engine pin "cooldown exclusion"): task-level cooldown
// excludes the row; project-level slot_cooldown pauses the project — while
// a sibling project with work keeps serving.
func (s *BoardSuite) TestKill_CooldownExclusion() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	hot := s.seedTask(e, "hot", nil, true)
	cold := s.seedTask(e, "cold", nil, true)
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET cooldown_until = now() + interval '1 hour' WHERE id=$1`, cold.ID)
	s.Require().NoError(err)

	c := s.claimOrFail(ws)
	s.Equal(hot.ID, c.TaskID, "the cooled task is invisible to the scan")
	s.finishClaim(c)

	// Project slot cooldown: p pauses entirely.
	_, err = s.pool.Exec(context.Background(),
		`UPDATE projects SET slot_cooldown_until = now() + interval '1 hour' WHERE id=$1`, p.ID)
	s.Require().NoError(err)
	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 4)
	e2 := s.seedEpic(p2, board.EpicActive)
	other := s.seedTask(e2, "other-project", nil, false)
	s.expectNoCandidate(ws)

	// A worker in ws2 still gets served (cross-project isolation of the
	// pause): ws2's own claim path is unaffected.
	c3 := s.claimOrFail(ws2)
	s.Equal(other.ID, c3.TaskID)
}

// KILL-TEST 17 (§6 r5/r6 pins "candidate-less least-served"): a project
// with the lowest vft but ZERO claimable candidates cannot win the lock and
// starve others (the p CTE's EXISTS); the candidate-less-EPIC-anchor variant
// — the lowest-vft eligible epic holds no candidates — must not wedge the
// in-project ordering either.
func (s *BoardSuite) TestKill_CandidateLessLeastServedSkipped() {
	ws := s.seedWorkspace()

	// Project-level variant: pStarved has the LOWEST vft but zero claimable
	// candidates — its blocked task is blocked, its anchor is cooled.
	pStarved := s.seedProject(ws, 4)
	eStarved := s.seedEpic(pStarved, board.EpicActive)
	hole := s.seedTask(eStarved, "starved-hole", nil, false)
	anchor := s.seedTask(eStarved, "starved-anchor", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), hole.ID, "", anchor.ID, ""))
	_, err := s.pool.Exec(context.Background(),
		`UPDATE epics SET vft = -100 WHERE id=$1`, eStarved.ID)
	s.Require().NoError(err)
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET cooldown_until = now() + interval '1 hour' WHERE id=$1`, anchor.ID)
	s.Require().NoError(err)

	// Project-level served project: plain claimable work.
	pWork := s.seedProject(ws, 4)
	eWork := s.seedEpic(pWork, board.EpicActive)
	work := s.seedTask(eWork, "served-work", nil, false)

	c := s.claimOrFail(ws)
	s.Equal(work.ID, c.TaskID, "the candidate-less least-served project is skipped (EXISTS guard) — it cannot starve pWork")
	s.finishClaim(c)

	// Epic-anchor variant: within pAnchor, the lowest-vft eligible epic's
	// blocked task is blocked and its anchor is cooled (zero candidates);
	// the sibling epic holds the candidate.
	pAnchor := s.seedProject(ws, 4)
	eLow := s.seedEpic(pAnchor, board.EpicActive)
	eNext := s.seedEpic(pAnchor, board.EpicActive)
	lowHole := s.seedTask(eLow, "low-hole", nil, false)
	lowAnchor := s.seedTask(eLow, "low-anchor", nil, false)
	next := s.seedTask(eNext, "next-work", nil, false)
	s.Require().NoError(s.svc.AddDependency(context.Background(), s.owner(), lowHole.ID, "", lowAnchor.ID, ""))
	_, err = s.pool.Exec(context.Background(),
		`UPDATE epics SET vft = -50 WHERE id=$1`, eLow.ID)
	s.Require().NoError(err)
	_, err = s.pool.Exec(context.Background(),
		`UPDATE tasks SET cooldown_until = now() + interval '1 hour' WHERE id=$1`, lowAnchor.ID)
	s.Require().NoError(err)

	c2 := s.claimOrFail(ws)
	s.Equal(next.ID, c2.TaskID, "the candidate-less lowest-vft epic is skipped; service flows to the candidate-bearing epic")
}

// KILL-TEST 18 (§6 r5 pin "cross-project starvation"): two projects in one
// workspace, alternating work — the least-served ordering serves B the
// moment A's vft rises above B's; a busy A cannot lock-step starve B.
func (s *BoardSuite) TestKill_CrossProjectFairness() {
	ws := s.seedWorkspace()
	pA := s.seedProject(ws, 8)
	pB := s.seedProject(ws, 8)
	eA := s.seedEpic(pA, board.EpicActive)
	eB := s.seedEpic(pB, board.EpicActive)
	for i := 0; i < 6; i++ {
		s.seedTask(eA, fmt.Sprintf("a%d", i), nil, true)
		s.seedTask(eB, fmt.Sprintf("b%d", i), nil, true)
	}

	var servedA, servedB int
	for i := 0; i < 6; i++ {
		c := s.claimOrFail(ws)
		if c.Task.ProjectID == pA.ID {
			servedA++
		} else {
			servedB++
		}
		s.finishClaim(c)
	}
	s.Equal(3, servedA)
	s.Equal(3, servedB, "cross-project DRR: B's vft falls below A's the moment A is served")
}

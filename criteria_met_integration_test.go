// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// criteria_met_integration_test.go — the #1946 scoped grant (owner
// ruling 2026-10-25): reviewer-persona claims record criteria-met on
// their OWN claimed row; task-worker self-certification stays
// forbidden. PG harness; skips without TEST_DATABASE_URL.

// personaClaimRow reads the row's durable claim provenance (000050).
func (s *BoardSuite) claimPersona(id string) (token, persona *string) {
	err := s.pool.QueryRow(context.Background(),
		`SELECT claim_token::text, claim_persona FROM tasks WHERE id=$1`, id).Scan(&token, &persona)
	s.Require().NoError(err)
	return token, persona
}

// criteriaMetEvent returns the task's criteria_met audit payloads (the
// per-criterion #1946 trail).
func (s *BoardSuite) criteriaMetEvents(projectID string) []map[string]any {
	rows, err := s.pool.Query(context.Background(),
		`SELECT payload FROM board_events WHERE project_id=$1 AND action='criteria_met' ORDER BY created_at ASC, id ASC`, projectID)
	s.Require().NoError(err)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		s.Require().NoError(rows.Scan(&raw))
		var m map[string]any
		s.Require().NoError(json.Unmarshal(raw, &m))
		out = append(out, m)
	}
	return out
}

// TestCriteriaMet_1946_ReviewerGrantEndToEnd — the probe-1 scenario
// (tsk-thematic-octave-tints), now self-completing: the implementer
// moves the row in_review (the persona mint claims it), the reviewer
// persona verifies and records BOTH criterion flags through the scoped
// grant, then moves the row done. Zero manager-surface touches.
func (s *BoardSuite) TestCriteriaMet_1946_ReviewerGrantEndToEnd() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "e2e", nil, false) // criteria UNMET

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "https://example.org/evidence", "")
	s.Require().NoError(err)

	// The persona claim's durable provenance — the #1946 seam.
	token, persona := s.claimPersona(task.ID)
	s.Require().NotNil(token)
	require.NotNil(s.T(), persona, "the mint stamps its persona provenance on the row")
	s.Equal("reviewer", *persona)
	s.Require().Len(spawner.claims, 2)
	s.Equal(*token, spawner.claims[1].ClaimToken, "the reviewer's charter carries the minted token")

	// The reviewer records its per-criterion verdicts — worker surface,
	// its charter token, its OWN row.
	rev := s.agent("rev-session", *token)
	out, err := svc.SetCriteriaMet(ctx, rev, "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	s.Require().NoError(err)
	s.True(out.AcceptanceCriteria[0].Met)
	_, err = svc.SetCriteriaMet(ctx, rev, "", c.ShortID, board.CriteriaCheck{Index: 1, Met: true}, "")
	s.Require().NoError(err)

	// The per-criterion audit trail carries the persona provenance.
	evs := s.criteriaMetEvents(p.ID)
	s.Require().Len(evs, 2)
	s.Equal("reviewer", evs[0]["persona"])
	s.Equal(float64(0), evs[0]["index"])
	s.Equal(true, evs[0]["met"])
	s.Equal("c1", evs[0]["criterion"])

	// Done now passes the R2 guard — the reviewer completes the loop.
	done, err := svc.MoveStage(ctx, rev, "", c.ShortID, board.StageDone, "https://example.org/evidence", "")
	s.Require().NoError(err, "the probe-1 loop self-completes: flags recorded by the reviewer, done by the reviewer")
	s.Equal(board.StageDone, done.Stage)

	// The done exit cleared the persona provenance with the token.
	token, persona = s.claimPersona(task.ID)
	s.Require().Nil(token)
	s.Require().Nil(persona)
}

// TestCriteriaMet_1946_TaskWorkerSelfCertificationRefused — the
// back-compat pin: the implementer's in_progress claim (and a
// token-less caller) never gains the grant; the refusal keeps the
// manager_surface class and the persona-tier text (the #1747 §5
// manager gate this once shared is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates — the grant itself stays
// reviewer-persona-only).
func (s *BoardSuite) TestCriteriaMet_1946_TaskWorkerSelfCertificationRefused() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	s.seedTask(e, "self-cert", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{}) // opt-out: plain worker claim

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)

	_, err = svc.SetCriteriaMet(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err, "task-worker self-certification remains forbidden")
	guard, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	s.Equal(board.GuardManagerSurface, guard.Code)
	s.Contains(guard.Detail, "no persona claim, no grant")
	s.Contains(guard.Detail, "task-worker self-certification")

	// Token-less callers hold nothing either.
	_, err = svc.SetCriteriaMet(ctx, s.agent("rev-session", ""), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err)
	s.Equal(0, len(s.criteriaMetEvents(p.ID)), "no criteria_met audit rows for refused writes")
}

// TestCriteriaMet_1946_GuardMatrix — the grant's remaining axes: the
// token must be the row's LIVE persona-claim token (stale/foreign
// refuses), indices are loud, and the reviewer can walk a flag BOTH
// ways.
func (s *BoardSuite) TestCriteriaMet_1946_GuardMatrix() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "matrix", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	token, _ := s.claimPersona(task.ID)
	s.Require().NotNil(token)
	rev := s.agent("rev-session", *token)

	// A stale token (the implementer's dead one) refuses.
	_, err = svc.SetCriteriaMet(ctx, s.agent("rev-session", c.ClaimToken), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err)
	guard, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	s.Equal(board.GuardClaimToken, guard.Code)

	// A FOREIGN persona claim (another row's token) refuses — the grant
	// is scoped to the caller's OWN claimed row by token arbitration.
	_, err = svc.SetCriteriaMet(ctx, rev, "", "tsk-does-not-exist", board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err)

	// Out-of-bounds is loud (#1837 P2b parity).
	_, err = svc.SetCriteriaMet(ctx, rev, "", c.ShortID, board.CriteriaCheck{Index: 9, Met: true}, "")
	require.Error(s.T(), err)
	guard, ok = err.(*board.GuardError)
	require.True(s.T(), ok)
	s.Equal(board.GuardInput, guard.Code)
	s.Contains(guard.Detail, "out of bounds")

	// Walk BOTH ways: check, then uncheck, then check again.
	for _, met := range []bool{true, false, true} {
		out, err := svc.SetCriteriaMet(ctx, rev, "", c.ShortID, board.CriteriaCheck{Index: 0, Met: met}, "")
		s.Require().NoError(err)
		s.Equal(met, out.AcceptanceCriteria[0].Met)
	}
	s.Len(s.criteriaMetEvents(p.ID), 3, "every flag write is audited per-criterion")
}

// TestCriteriaMet_1946_PausedRowRefuses — the #1747 pause-must-pause
// axis of the grant (the review's coverage gap): a persona-claimed
// in_review row paused mid-review refuses the flag write.
func (s *BoardSuite) TestCriteriaMet_1946_PausedRowRefuses() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "paused", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	token, persona := s.claimPersona(task.ID)
	s.Require().NotNil(token)
	require.NotNil(s.T(), persona)

	_, err = svc.PauseTask(ctx, s.owner(), task.ID, "", "")
	s.Require().NoError(err)

	_, err = svc.SetCriteriaMet(ctx, s.agent("rev-session", *token), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err, "a paused row halts its workers — persona criteria writes refuse like the mint does")
	guard, ok := err.(*board.GuardError)
	require.True(s.T(), ok)
	s.Equal(board.GuardPaused, guard.Code)
	s.Equal(0, len(s.criteriaMetEvents(p.ID)), "no audit rows for the refused write")

	// Resume re-arms the grant — the reviewer continues its loop.
	_, err = svc.ResumeTask(ctx, s.owner(), task.ID, "", "")
	s.Require().NoError(err)
	out, err := svc.SetCriteriaMet(ctx, s.agent("rev-session", *token), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	s.Require().NoError(err)
	s.True(out.AcceptanceCriteria[0].Met)
}

// TestCriteriaMet_1946_ClosedUnmergedDriftClearsProvenance — the RC2
// drift pin (the review's second coverage ask, mirroring the bounce
// pin): both closed-unmerged landings — the recorded-identity
// re-claim-class mint (→in_progress) and the no-identity backlog
// landing — drop the persona provenance (claim_persona rides ONLY a
// live persona claim; the drift paths are non-persona mints).
func (s *BoardSuite) TestCriteriaMet_1946_ClosedUnmergedDriftClearsProvenance() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)

	seedPersonaClaimed := func(title string) (*board.Task, string) {
		task := s.seedTask(e, title, []string{"pr:7"}, false)
		spawner := &claimCapture{}
		svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
		s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})
		c, err := svc.ClaimNext(context.Background(), "w-"+ws, ws)
		s.Require().NoError(err)
		_, err = svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
		s.Require().NoError(err)
		token, persona := s.claimPersona(task.ID)
		s.Require().NotNil(token)
		s.Require().NotNil(persona, "fixture: the row holds a persona claim pre-drift")
		return task, c.ShortID
	}

	// Branch 1 — recorded identity: the re-claim-class drift mint
	// (→in_progress) is a worker-class claim.
	task1, short1 := seedPersonaClaimed("drift-reclaim")
	s.drift.set(7, board.PRState{Closed: true, URL: "https://git.example/pr/7"})
	respawning := board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithPRDriftSource(s.drift),
		board.WithFailpoint(s.fps.fp))
	res, err := respawning.Sweep(context.Background())
	s.Require().NoError(err)
	s.Require().Equal(1, res.DriftReentry)
	token, persona := s.claimPersona(task1.ID)
	s.Require().NotNil(token, "the drift re-claim mints a fresh worker token")
	s.Require().Nil(persona, "the drift mint is worker-class: no persona provenance")

	// And that drifted worker cannot self-certify with its token.
	_, err = s.svc.SetCriteriaMet(context.Background(), s.agent("impl", *token), "", short1, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err)

	// Branch 2 — no recorded identity: the backlog landing keeps the
	// (pre-existing) token but drops the provenance.
	task2, _ := seedPersonaClaimed("drift-backlog")
	_, err = s.pool.Exec(context.Background(), `UPDATE tasks SET assignee_session=NULL WHERE id=$1`, task2.ID)
	s.Require().NoError(err)
	s.drift.set(7, board.PRState{Closed: true, URL: "https://git.example/pr/7"})
	res, err = respawning.Sweep(context.Background())
	s.Require().NoError(err)
	s.Require().Equal(1, res.DriftReentry)
	token, persona = s.claimPersona(task2.ID)
	s.Equal(board.StageBacklog, s.rawTask(task2.ID).Stage)
	s.Require().Nil(persona, "the no-identity landing drops the persona provenance (the token's pre-existing retention is untouched)")
	_ = token
}

// TestCriteriaMet_1946_BounceClearsProvenance — hygiene pin: the bounce
// re-claim (in_review → in_progress) mints an IMPLEMENTER-class claim;
// the persona provenance must not survive it (a bounced row's next
// claim is a task worker, never a self-certifier).
func (s *BoardSuite) TestCriteriaMet_1946_BounceClearsProvenance() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "bounce-clear", nil, false)

	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg, board.WithWorkerSpawner(spawner.Spawn))
	s.setStagePersonas(svc, p, map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}})

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	_, persona := s.claimPersona(task.ID)
	require.NotNil(s.T(), persona)

	// The bounce (manager-grade move; the fail-verdict shape).
	_, err = svc.MoveStage(ctx, s.owner(), "", c.ShortID, board.StageInProgress, "", "")
	s.Require().NoError(err)
	token, persona := s.claimPersona(task.ID)
	s.Require().NotNil(token, "the bounce re-claims the row")
	s.Require().Nil(persona, "the bounced claim is implementer-class: no persona provenance")

	// And the bounced implementer cannot self-certify with it.
	row := s.rawTask(task.ID)
	_, err = svc.SetCriteriaMet(ctx, s.agent("impl", *row.ClaimToken), "", c.ShortID, board.CriteriaCheck{Index: 0, Met: true}, "")
	require.Error(s.T(), err)
}

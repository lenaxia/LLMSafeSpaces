// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Service-layer kill-tests (issue #1698, design 0067 §4.1/§4.2 + §6):
// the stage machine, the Done guard, the six-provenance pending_input
// exits, idempotent replay convergence, dependency guards, and the
// short-ID/did-you-mean discipline.
package board_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// KILL-TEST 28 (§6 stage-machine pins): exactly the enumerated transitions
// exist; everything else is a GuardError. No backward move exists in the
// machine except in_review → in_progress.
func (s *BoardSuite) TestKill_StageMachineGuards() {
	type fixture struct {
		name    string
		from    string // how the row is parked
		to      string
		actor   func(a board.Actor, token string) board.Actor
		wantErr string // "" = allowed
	}

	withToken := func(from, to string) fixture {
		return fixture{from + "→" + to + " (worker)", from, to, func(_ board.Actor, tok string) board.Actor { return s.agent("sess", tok) }, ""}
	}

	cases := []fixture{
		withToken(board.StageInProgress, board.StageInReview),
		withToken(board.StageInProgress, board.StagePendingInput),
		{"in_progress→in_review (manager, no token): an in-flight drag snaps back",
			board.StageInProgress, board.StageInReview, func(a board.Actor, _ string) board.Actor { return a }, board.GuardClaimToken},
		withToken(board.StageInReview, board.StagePendingInput),
		{"backlog→in_progress via move_stage: NO token-less in_progress row is ever created",
			board.StageBacklog, board.StageInProgress, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"done→backlog: done has no enumerated exit",
			board.StageDone, board.StageBacklog, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"done→in_progress", board.StageDone, board.StageInProgress, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"in_progress→backlog: no such backward move (#1747: no terminal stage exists)",
			board.StageInProgress, board.StageBacklog, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"backlog→done skips the machine", board.StageBacklog, board.StageDone, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"backlog→pending_input", board.StageBacklog, board.StagePendingInput, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
		{"in_review→backlog: no such backward move", board.StageInReview, board.StageBacklog, func(a board.Actor, _ string) board.Actor { return a }, board.GuardStage},
	}

	for _, tc := range cases {
		s.Run(tc.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, "machine", nil, true)
			s.parkAt(task, tc.from, ws)

			var token string
			if row := s.rawTask(task.ID); row.ClaimToken != nil {
				token = *row.ClaimToken
			}
			actor := tc.actor(s.owner(), token)
			_, err := s.svc.MoveStage(context.Background(), actor, task.ID, "", tc.to, "https://evidence.example/x", "")
			if tc.wantErr == "" {
				s.Require().NoError(err)
				s.Equal(tc.to, s.rawTask(task.ID).Stage)
				s.Equal(1, s.countTransitions(task.ID, tc.to), "every transition writes exactly one board_transitions row")
				return
			}
			s.Require().Error(err)
			var ge *board.GuardError
			s.Require().ErrorAs(err, &ge, "move_stage guard class")
			s.Equal(tc.wantErr, ge.Code)
			s.Equal(tc.from, s.rawTask(task.ID).Stage, "a refused drag changes nothing")
		})
	}
}

// parkAt drives a fixture task into the requested stage by legal means.
// Each subtest owns exactly one task in a fresh project, so claiming
// resolves to the fixture.
func (s *BoardSuite) parkAt(t *board.Task, stage, wsID string) {
	ctx := context.Background()
	switch stage {
	case board.StageBacklog:
		return
	case board.StageInProgress:
		c := s.claimOrFail(wsID)
		s.Require().Equal(t.ID, c.TaskID, "park helper must claim the fixture task")
	case board.StageInReview:
		c := s.claimOrFail(wsID)
		s.Require().Equal(t.ID, c.TaskID)
		_, err := s.svc.MoveStage(ctx, s.agent("sess", c.ClaimToken), t.ID, "", board.StageInReview, "", "")
		s.Require().NoError(err)
	case board.StageDone:
		c := s.claimOrFail(wsID)
		s.Require().Equal(t.ID, c.TaskID)
		_, err := s.svc.MoveStage(ctx, s.agent("sess", c.ClaimToken), t.ID, "", board.StageInReview, "", "")
		s.Require().NoError(err)
		s.doneDirect(t)
	case board.StagePendingInput:
		c := s.claimOrFail(wsID)
		s.Require().Equal(t.ID, c.TaskID)
		_, err := s.svc.MoveStage(ctx, s.agent("sess", c.ClaimToken), t.ID, "", board.StagePendingInput, "", "")
		s.Require().NoError(err)
	default:
		s.Failf("unknown park stage %q", stage)
	}
}

// KILL-TEST 29 (ruling R2 Done guard): done requires ALL acceptance
// criteria met + evidence_url; the reviewer checks criteria off via
// update_task; the transition row carries the evidence.
func (s *BoardSuite) TestKill_DoneGuard() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "done-guard", nil, false) // criteria NOT met
	c := s.claimOrFail(ws)
	s.Require().Equal(task.ID, c.TaskID)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), task.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)

	// Criteria unmet → guard fires.
	_, err = s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageDone, "https://evidence.example/a", "")
	s.Require().Error(err)
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardDone, ge.Code)

	// Check off one criterion (of two) → still unmet.
	_, err = s.svc.UpdateTask(context.Background(), s.owner(), task.ID, "",
		board.TaskInput{Title: task.Title, AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}, {Text: "c2", Met: false}}}, "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageDone, "https://evidence.example/a", "")
	s.Require().Error(err, "ALL criteria must be met")

	// All met but no evidence → still fires.
	_, err = s.svc.UpdateTask(context.Background(), s.owner(), task.ID, "",
		board.TaskInput{Title: task.Title, AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}, {Text: "c2", Met: true}}}, "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageDone, "", "")
	s.Require().Error(err)

	// Both → done, evidence recorded.
	got, err := s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageDone, "https://evidence.example/a", "")
	s.Require().NoError(err)
	s.Equal(board.StageDone, got.Stage)
	var ev string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT evidence_url FROM board_transitions WHERE task_id=$1 AND to_stage='done'`, task.ID).Scan(&ev))
	s.Equal("https://evidence.example/a", ev)
}

// KILL-TEST 30 (§6 r6 "stale-assignee exit pin, exhaustive by provenance"):
// six fixture cases across the four pending_origin values. Identity-holding
// rows take the re-claim branch (fresh token+lease, attempts reset, nudge);
// escalated + abandoned-from-backlog/lapsed take the backlog branch (the
// dead worker is never re-nudged). The branch keys on identity presence,
// never token presence.
func (s *BoardSuite) TestKill_PendingExitsSixProvenances() {
	type want struct {
		branch   string // "reclaim" | "backlog"
		origin   string
		preToken bool // row holds a live token before the exit
		preIdent bool // row holds a recorded assignee before the exit
	}
	cases := []struct {
		name string
		want want
		set  func(t *board.Task, ws string)
	}{
		{"worker-origin (dead session)", want{"reclaim", board.PendingOriginWorker, true, true}, func(t *board.Task, ws string) {
			c := s.claimOrFail(ws)
			s.Require().Equal(t.ID, c.TaskID)
			_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), t.ID, "", board.StagePendingInput, "", "")
			s.Require().NoError(err)
		}},
		{"escalated", want{"backlog", board.PendingOriginEscalated, false, false}, func(t *board.Task, ws string) {
			// Drive attempts to cap, then let the claim escalate.
			for i := 0; i < s.cfg.AttemptsCap; i++ {
				c := s.claimOrFail(ws)
				s.Require().Equal(t.ID, c.TaskID)
				s.expireLease(t.ID)
				s.sweep()
			}
			c, err := s.svc.ClaimNext(context.Background(), "w", ws)
			s.Require().NoError(err)
			s.True(c.Escalated)
		}},
		// #1747 round 3: the dependency_abandoned origin's only creator
		// (cancel's dependent cascade) is ELIMINATED — deleting a
		// prerequisite removes the edge, so dependents UNBLOCK instead of
		// staging pending. The origin remains in the vocabulary for
		// historical rows only; no live provenance constructs it.
		{"drift-review", want{"reclaim", board.PendingOriginDriftReview, false, true}, func(t *board.Task, ws string) {
			c := s.claimOrFail(ws)
			s.Require().Equal(t.ID, c.TaskID)
			_, err := s.svc.MoveStage(context.Background(), s.agent("sess", c.ClaimToken), t.ID, "", board.StageInReview, "", "")
			s.Require().NoError(err)
		}},
	}

	for i, tc := range cases {
		s.Run(tc.name, func() {
			ws := s.seedWorkspace()
			p := s.seedProject(ws, 4)
			e := s.seedEpic(p, board.EpicActive)
			task := s.seedTask(e, fmt.Sprintf("exit%d", i), nil, false)

			// Drift-review fixture needs unmet criteria + a merged PR.
			if tc.want.origin == board.PendingOriginDriftReview {
				_, err := s.pool.Exec(context.Background(),
					`UPDATE tasks SET tags = '{pr:900}' WHERE id=$1`, task.ID)
				s.Require().NoError(err)
			}
			tc.set(task, ws)
			if tc.want.origin == board.PendingOriginDriftReview {
				s.drift.set(900, board.PRState{Merged: true, URL: "https://git.example/900"})
				s.sweep()
			}

			row := s.rawTask(task.ID)
			s.Equal(board.StagePendingInput, row.Stage, "fixture parked")
			s.Equal(tc.want.origin, *row.PendingOrigin)
			s.Equal(tc.want.preToken, row.ClaimToken != nil, "fixture token state (branch keys on identity, never token)")
			s.Equal(tc.want.preIdent, row.Assignee != nil, "fixture identity state")
			spawnsBefore := s.spawner.count()

			// The exit: manager-surface human move_stage.
			out, err := s.svc.MoveStage(context.Background(), s.owner(), task.ID, "", board.StageInProgress, "", "")
			s.Require().NoError(err)

			after := s.rawTask(task.ID)
			s.Equal(0, after.Attempts, "the exit is the ONE write that resets attempts")
			if tc.want.branch == "reclaim" {
				s.Equal(board.StageInProgress, out.Stage)
				s.NotNil(after.ClaimToken, "re-claim branch mints a fresh token")
				s.NotNil(after.LeaseExp)
				s.False(after.ClaimIncr, "exit re-claim mints with flag=false")
				// Re-spawn semantics (merged doc §4.1): the fresh worker
				// receives the token in its charter — no nudges to dead
				// sessions anywhere. The sync fake spawner stamps the
				// identity immediately.
				s.Equal(spawnsBefore+1, s.spawner.count(), "the token is delivered by a FRESH spawn (R9)")
				s.NotNil(after.Assignee, "the guarded spawn-stamp landed (sync spawner)")
			} else {
				s.Equal(board.StageBacklog, out.Stage, "no-identity rows refuse in_progress and land backlog — never a token-less in_progress row")
				s.Nil(after.ClaimToken)
				s.Nil(after.Assignee)
				s.Nil(after.LeaseExp)
				s.Equal(spawnsBefore, s.spawner.count(), "the dead worker is never re-spawned from the backlog branch")
			}
			s.Nil(after.PendingOrigin, "origin cleared on exit")
		})
	}
}

// KILL-TEST 31 (§6 r4 pin): the pending_input exit is manager-surface-only
// — a delegated worker (agent surface) cannot self-exit, even holding the
// token; the drift_review exit follows the re-claim-or-backlog rule (no
// token-less in_progress row is ever created).
func (s *BoardSuite) TestKill_PendingExitManagerSurfaceOnly() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "self-exit", nil, false)

	c := s.claimOrFail(ws)
	token := c.ClaimToken
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess", token), task.ID, "", board.StagePendingInput, "", "")
	s.Require().NoError(err)

	_, err = s.svc.MoveStage(context.Background(), s.agent("sess", token), task.ID, "", board.StageBacklog, "", "")
	s.Require().Error(err, "a delegated worker is not the human the exit exists for")
	var ge *board.GuardError
	s.Require().ErrorAs(err, &ge)
	s.Equal(board.GuardManagerSurface, ge.Code)
	s.Equal(board.StagePendingInput, s.rawTask(task.ID).Stage, "no self-exit")
}

// KILL-TEST 32 (§6 invariant 4): crash between action and effect →
// idempotent replay converges. Every mutating action re-invoked with the
// same requestId after a simulated mid-transaction death returns
// 200-with-original (DuplicateError), board state converges, exactly one
// board_events row per requestId.
func (s *BoardSuite) TestKill_IdempotentReplayConverges() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "replay", nil, true)

	// Park the task in a claimed in_progress state (the Done transition's
	// legal source), then fault between effect and event write: the tx
	// aborts — nothing lands.
	parked := s.claimOrFail(ws)
	s.Require().Equal(task.ID, parked.TaskID)
	s.fps.armAt("move_stage.effect", fmt.Errorf("crash between action and effect"))
	_, err := s.svc.MoveStage(ctx, s.agent("sess", parked.ClaimToken), task.ID, "", board.StageDone, "https://evidence.example/r", "")
	s.Require().Error(err)
	s.fps.disarm("move_stage.effect")
	s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage, "the aborted tx left nothing")
	s.Equal(0, s.countEvents("move_stage"))

	// Replay with the SAME requestId: succeeds (first effective commit).
	_, err = s.svc.MoveStage(ctx, s.agent("sess", parked.ClaimToken), task.ID, "", board.StageDone, "https://evidence.example/r", "req-1")
	s.Require().NoError(err)
	s.Equal(board.StageDone, s.rawTask(task.ID).Stage)

	// Replay again: 200-with-original, never an error; no double effect.
	_, err = s.svc.MoveStage(ctx, s.agent("sess", parked.ClaimToken), task.ID, "", board.StageDone, "https://evidence.example/r", "req-1")
	s.Require().NoError(err, "duplicate requestId returns the original outcome")
	s.Equal(1, s.countTransitions(task.ID, board.StageDone), "no double transition")
	s.Equal(1, s.countEvents("move_stage"))

	// Plain duplicate (no crash involved).
	_, err = s.svc.MoveStage(ctx, s.agent("sess", parked.ClaimToken), task.ID, "", board.StageDone, "https://evidence.example/r", "req-1")
	s.Require().NoError(err)
	s.Equal(1, s.countTransitions(task.ID, board.StageDone))

	// create_task replay: one event, one task.
	created, err := s.svc.CreateTask(ctx, s.owner(), e.ID, "", board.TaskInput{Title: "dup-create"}, "req-2")
	s.Require().NoError(err)
	_, err = s.svc.CreateTask(ctx, s.owner(), e.ID, "", board.TaskInput{Title: "dup-create"}, "req-2")
	s.Require().NoError(err)
	var n int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE title='dup-create'`).Scan(&n))
	s.Equal(1, n, "one task per requestId")

	// delete is idempotent-by-absence (#1747): the first delete removes
	// the row (its event embeds the final-state snapshot); the replay
	// resolves a gone row and 404s.
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), task.ID, "", created.ID, ""))
	out, err := s.svc.DeleteTask(ctx, s.owner(), created.ID, "", false, "")
	s.Require().NoError(err)
	s.Equal(created.ShortID, out.ShortID)
	s.Contains(out.OrphanedDependents, task.ShortID, "the dependent is listed (affected-party visibility)")
	_, err = s.svc.DeleteTask(ctx, s.owner(), created.ID, "", false, "")
	s.Require().Error(err, "a deleted task never resolves again")
}

// KILL-TEST 33 (ruling R4a): dependency guards — cycle rejection at write
// (409 + did-you-mean naming the near-miss tasks), self-edge, cross-project
// rejection.
func (s *BoardSuite) TestKill_DependencyGuards() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	a := s.seedTask(e, "dep-a", nil, false)
	b := s.seedTask(e, "dep-b", nil, false)
	cc := s.seedTask(e, "dep-c", nil, false)

	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), b.ID, "", a.ID, ""))  // b→a
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), cc.ID, "", b.ID, "")) // c→b

	// Cycle: a→c closes the loop.
	err := s.svc.AddDependency(ctx, s.owner(), a.ID, "", cc.ID, "")
	s.Require().Error(err)
	var de *board.DependencyError
	s.Require().ErrorAs(err, &de)
	s.Equal("cycle", de.Code)

	// Self-edge.
	err = s.svc.AddDependency(ctx, s.owner(), a.ID, "", a.ID, "")
	s.Require().Error(err)
	s.Require().ErrorAs(err, &de)
	s.Equal("self", de.Code)

	// Cross-project: project2's task cannot depend on project1's.
	ws2 := s.seedWorkspace()
	p2 := s.seedProject(ws2, 4)
	e2 := s.seedEpic(p2, board.EpicActive)
	foreign := s.seedTask(e2, "foreign", nil, false)
	err = s.svc.AddDependency(ctx, s.owner(), foreign.ID, "", a.ID, "")
	s.Require().Error(err)
	s.Require().ErrorAs(err, &de)
	s.Equal("cross_workspace", de.Code, "#1747: edges live within a workspace, never across")

	// Duplicate edge: inert (PK).
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), b.ID, "", a.ID, ""))

	// Resolution suggestions fire on the near-miss shape: a typo'd
	// dependency target carries suggestions naming the near-miss tasks.
	err = s.svc.AddDependency(ctx, s.owner(), a.ID, "", mutateShortIDSuffix(b.ShortID), "")
	s.Require().Error(err)
	nf, ok := err.(*board.NotFoundError)
	s.Require().True(ok, "a near-miss short id resolves as 404+suggestions, got %T", err)
	s.Require().NotEmpty(nf.Suggestions)
}

// KILL-TEST 34 (design 0066 discipline): short IDs mint with the closed
// prefix table + shape; resolution is case-insensitive; unknown ids return
// 404 + top-3 suggestions bounded at Levenshtein <= 2.
func (s *BoardSuite) TestKill_ShortIDResolution() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	wsForeign := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "alpha", nil, false)
	s.Regexp(`^tsk-[0-9a-z]{8}$`, t1.ShortID)
	s.Regexp(`^prj-[0-9a-z]{8}$`, p.ShortID)
	s.Regexp(`^epc-[0-9a-z]{8}$`, e.ShortID)

	// Case-insensitive resolution through the workspace-scoped reader
	// (the agent seam's shape, r1-S1).
	got, err := s.svc.GetTaskInWorkspace(ctx, ws, "", upper(t1.ShortID))
	s.Require().NoError(err)
	s.Equal(t1.ID, got.ID)

	// Unknown → 404 with bounded suggestions (in-workspace only).
	_, err = s.svc.GetTaskInWorkspace(ctx, ws, "", "tsk-zzzzzzzz")
	s.Require().Error(err)
	_, ok := err.(*board.NotFoundError)
	s.Require().True(ok)

	// A one-char mutation resolves with the original suggested first.
	mutated := mutateShortIDSuffix(t1.ShortID)
	_, err = s.svc.GetTaskInWorkspace(ctx, ws, "", mutated)
	s.Require().Error(err)
	nf, ok := err.(*board.NotFoundError)
	s.Require().True(ok)
	s.Require().NotEmpty(nf.Suggestions)
	s.Equal(t1.ShortID, nf.Suggestions[0], "best-first: the near-miss original")

	// r1-S1: a foreign workspace's reader gets a 404 with NO row and NO
	// suggestions — resolution and did-you-mean never cross tenants.
	foreign, err := s.svc.GetTaskInWorkspace(ctx, wsForeign, "", t1.ShortID)
	s.Require().Error(err)
	s.Require().Nil(foreign)
	nf2, ok := err.(*board.NotFoundError)
	s.Require().True(ok)
	s.Require().Empty(nf2.Suggestions, "no cross-tenant suggestions")
}

// mutateShortIDSuffix swaps the final char for one GUARANTEED to differ
// (the r3 flake: appending "q" is a no-op whenever the random mint already
// ends in q — 1/36 per fresh draw, deterministic-in-cause, random-in-
// trigger; it reddened Secrets Integration on 4a70bfa5). Same length, so
// the mutation stays a Levenshtein-1 near miss.
func mutateShortIDSuffix(id string) string {
	last := id[len(id)-1]
	sub := byte('q')
	if last == 'q' {
		sub = 'r'
	}
	return id[:len(id)-1] + string(sub)
}

func upper(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 32
		}
	}
	return string(out)
}

// KILL-TEST 35 (§4.7 + §6 "ownership-agnostic engine audit pin" +
// "derived-epic-progress arity pin", grep-level): (a) no scheduler/lease/
// fairness/reconciler code path reads user_id; (b) no stored progress or
// story-points column exists on epics (ruling R2: derived, never stored).
func TestKill_SourceLevelOwnershipPins(t *testing.T) {
	for _, f := range []string{"engine.go", "reconciler.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "user_id") {
			t.Errorf("%s reads user_id — the engine/reconciler are ownership-agnostic by construction (design §4.7 forward-compat note)", f)
		}
	}
}

// KILL-TEST 36 (§6 "derived-epic-progress arity pin", schema-level): no
// stored progress or story-points column exists on epics — progress is
// derived by board_read only (ruling R2), and story points were cut.
func (s *BoardSuite) TestKill_NoStoredEpicProgressColumn() {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name='epics'
		  AND (column_name ILIKE '%progress%' OR column_name ILIKE '%story%' OR column_name ILIKE '%point%')
	`).Scan(&n))
	s.Equal(0, n, "epic progress is derived, never stored; story points do not exist (ruling R2)")
}

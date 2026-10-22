// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// reads_events_test.go — #1832 item 4 red-first pins for the task
// audit-timeline read seam (Service.TaskEvents): the FULL timeline of
// board_events rows scoped to one task — stage transitions, claims
// (who/when via the payload), dependency-graph changes, orphan/unblock
// events — in chronological order with a newest-N window. RED until
// the seam exists.

import (
	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// actionsOf renders the timeline's action sequence (assertion helper).
func actionsOf(evts []board.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.Action)
	}
	return out
}

// TestReads_TaskEvents exercises the REAL write path (create → claim →
// stage moves → dependency add/remove → delete-of-a-dependency) and
// pins that TaskEvents returns exactly those rows, oldest first.
func (s *BoardSuite) TestReads_TaskEvents() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e1 := s.seedEpic(p, board.EpicActive)

	dep := s.seedTask(e1, "the dependency", nil, true)
	t := s.seedTask(e1, "the watched task", nil, true)
	// Claims ride the ENGINE's FIFO path (claimOnce → action "claim",
	// payload {task_id, worker, attempts} — the timeline's who/when).
	// FIFO by creation: the older dep grants first, then the watched task.
	depClaim := s.claimOrFail(ws)
	s.Require().Equal(dep.ID, depClaim.TaskID)
	claim := s.claimOrFail(ws)
	s.Require().Equal(t.ID, claim.TaskID)
	_, err := s.svc.MoveStage(ctx, s.agent("sess-"+claim.ShortID, claim.ClaimToken), t.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(ctx, s.owner(), t.ID, "", board.StageDone, "https://example.com/proof", "")
	s.Require().NoError(err)

	// Dependency-graph change on the watched task: add then remove.
	s.Require().NoError(s.svc.AddDependency(ctx, s.owner(), t.ID, "", dep.ID, ""))

	// Delete the dependency: the watched task's timeline must carry the
	// orphan/unblock event (unblocked_by_delete rides the ORPHAN's row).
	_, err = s.svc.DeleteTask(ctx, s.owner(), dep.ID, "", false, "")
	s.Require().NoError(err)

	evts, err := s.svc.TaskEvents(ctx, "", t.ShortID, 0)
	s.Require().NoError(err)
	actions := actionsOf(evts)
	s.T().Logf("timeline actions: %v", actions)

	// The four #1832 item-4 timeline classes over this history:
	require.Contains(s.T(), actions, "create_task")
	require.Contains(s.T(), actions, "claim")      // claims: who/when
	require.Contains(s.T(), actions, "move_stage") // stage transitions
	require.Contains(s.T(), actions, "add_dependency")
	require.Contains(s.T(), actions, "unblocked_by_delete")

	// Chronological ascending: the first row is the task's creation.
	require.NotEmpty(s.T(), evts)
	assert.Equal(s.T(), "create_task", evts[0].Action)

	// Every row is scoped to THIS task (item_kind task, item_id = uuid).
	for _, e := range evts {
		assert.Equal(s.T(), "task", e.ItemKind)
		assert.Equal(s.T(), t.ID, e.ItemID)
		assert.Equal(s.T(), p.ID, e.ProjectID)
	}

	// The move_stage rows carry from/to in the payload (the drawer's
	// transition rendering is payload-driven).
	var sawTransition bool
	for _, e := range evts {
		if e.Action == "move_stage" {
			assert.Contains(s.T(), string(e.Payload), "from_stage")
			assert.Contains(s.T(), string(e.Payload), "resulting_stage")
			sawTransition = true
		}
	}
	assert.True(s.T(), sawTransition, "at least one move_stage payload")
}

// TestReads_TaskEventsWindow pins the newest-N window: limit=N returns
// the N newest rows STILL in chronological order (the window slides,
// the order never flips).
func (s *BoardSuite) TestReads_TaskEventsWindow() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	e1 := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e1, "windowed", nil, true)

	full, err := s.svc.TaskEvents(ctx, "", t.ShortID, 0)
	s.Require().NoError(err)
	s.Require().Len(full, 1, "just the create_task row")

	// A second task keeps its OWN timeline (scoping, no cross-task bleed).
	other := s.seedTask(e1, "unrelated", nil, true)
	otherEvts, err := s.svc.TaskEvents(ctx, "", other.ShortID, 0)
	s.Require().NoError(err)
	require.Len(s.T(), otherEvts, 1)
	assert.Equal(s.T(), other.ID, otherEvts[0].ItemID, "no cross-task bleed")

	// Window: limit caps at the newest rows, ascending order preserved.
	// FIFO by creation: the older seeded task (t) grants first.
	claim := s.claimOrFail(ws)
	s.Require().Equal(t.ID, claim.TaskID)
	evts, err := s.svc.TaskEvents(ctx, "", t.ShortID, 1)
	s.Require().NoError(err)
	require.Len(s.T(), evts, 1)
	assert.Equal(s.T(), "claim", evts[0].Action, "limit=1 keeps the NEWEST row")
}

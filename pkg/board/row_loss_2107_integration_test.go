// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// row_loss_2107_integration_test.go — the pins for the 2026-11 silent
// row-loss class (issue #2107). Two findings, two pin families:
//
//  1. The epic-auto-complete-purge theory is REFUTED at the seam it
//     would have to live on: every completion path (the →done
//     transition's in-transaction seam, the reconciler's RC4 net) is
//     status-only — an epic with open siblings REFUSES to complete,
//     and an epic that completes keeps every story row. The purge
//     theory's "correlation" was the causal arrow reversed: epics
//     complete BECAUSE open rows vanish (the drain predicate demands
//     zero non-done), never the other way. These pins hold that line.
//  2. The audit trail was unreadable exactly when it was needed: the
//     delete contract preserves history (delete events embed the row
//     snapshot), yet TaskEvents 404'd once the row left the table —
//     sealing WHO-deleted-WHAT from every investigator. The tombstone
//     read (#2107 fix) anchors the trail through board_events; the
//     never-existed/below-store fork stays an honest 404.
package board_test

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestRowLoss_2107_EpicCompletionNeverPurgesOpenSiblings is the
// charter's repro, pinned: one done + two open siblings (backlog AND
// in_progress), every completion path fired — the doneCascade seam, the
// RC4 sweep net — and the open rows must survive verbatim while the
// epic refuses to complete (the drain predicate holds it). A purge, a
// cascade, or a wrong-stage-filter bug anywhere on these paths fails
// this test.
func (s *BoardSuite) TestRowLoss_2107_EpicCompletionNeverPurgesOpenSiblings() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)

	// The fixture mirrors the incident shape: dones landing in an epic
	// that still holds OPEN siblings. All four rows exist BEFORE any
	// →done fires (a lone story's done would legitimately drain the
	// epic — the #1948 create guard is the belt on the far side).
	s1 := s.seedTask(e, "s1-done", nil, true)
	s2 := s.seedTask(e, "s2-open-backlog", nil, false)
	s3 := s.seedTask(e, "s3-open-claimed", nil, false)
	s4 := s.seedTask(e, "s4-second-done", nil, true)
	// s2 paused OFF the feed so claims walk s1→s3→s4 (the pause lever
	// steers FIFO; the row itself never leaves the board).
	_, err := s.svc.PauseTask(ctx, s.owner(), s2.ID, "", "")
	require.NoError(t, err)

	// The drain-trigger attempts: TWO →done transitions through the
	// real path (each runs wrapUpAwareEpicCompletion in-transaction
	// with open siblings on the board) + one claimed row left open.
	c1, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	require.NoError(t, err)
	require.Equal(t, s1.ShortID, c1.ShortID, "FIFO takes s1 first")
	_, err = s.svc.MoveStage(ctx, s.agent(c1.Assignee, c1.ClaimToken), "", s1.ShortID,
		board.StageDone, "https://ev.test/"+s1.ShortID, "")
	require.NoError(t, err)
	c3, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	require.NoError(t, err)
	require.Equal(t, s3.ShortID, c3.ShortID, "s3 claims and stays open (in_progress)")
	c4, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	require.NoError(t, err)
	require.Equal(t, s4.ShortID, c4.ShortID, "the open siblings never block the feed")
	_, err = s.svc.MoveStage(ctx, s.agent(c4.Assignee, c4.ClaimToken), "", s4.ShortID,
		board.StageDone, "https://ev.test/"+s4.ShortID, "")
	require.NoError(t, err)

	// The RC4 sweep net (missed-terminal completion) over the same epic.
	_, err = s.recon.Sweep(ctx)
	require.NoError(t, err)

	// The line the purge theory would break: the epic REFUSES to
	// complete while open siblings exist, and the rows are intact.
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID),
		"an epic with open siblings never auto-completes (drain predicate)")
	for _, id := range []string{s2.ID, s3.ID} {
		row, rerr := s.svc.GetTask(ctx, id, "")
		require.NoError(t, rerr, "open sibling %s survives every completion path", row.ShortID)
	}
	require.Equal(t, board.StageBacklog, s.rawTask(s2.ID).Stage)
	require.Equal(t, board.StageInProgress, s.rawTask(s3.ID).Stage)
	require.Equal(t, 0, s.countEvents("delete_task"), "no delete fired on any completion path")

	// The complementary shape: once the siblings DO complete, the epic
	// completes — and still deletes nothing. s2 resumes onto the feed;
	// s3 completes on its LIVE claim (never paused, token from above).
	// Both get their criteria checked off first (the Done guard, R2).
	for _, id := range []string{s2.ID, s3.ID} {
		for idx := 0; idx < 2; idx++ {
			_, err = s.svc.UpdateTask(ctx, s.owner(), id, "",
				board.TaskInput{SetCriteriaMet: &board.CriteriaCheck{Index: idx, Met: true}}, "")
			require.NoError(t, err)
		}
	}
	_, err = s.svc.ResumeTask(ctx, s.owner(), s2.ID, "", "")
	require.NoError(t, err)
	c2, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	require.NoError(t, err)
	require.Equal(t, s2.ShortID, c2.ShortID)
	_, err = s.svc.MoveStage(ctx, s.agent(c2.Assignee, c2.ClaimToken), "", s2.ShortID,
		board.StageDone, "https://ev.test/"+s2.ShortID, "")
	require.NoError(t, err)
	_, err = s.svc.MoveStage(ctx, s.agent(c3.Assignee, c3.ClaimToken), "", s3.ShortID,
		board.StageDone, "https://ev.test/"+s3.ShortID, "")
	require.NoError(t, err)

	require.Equal(t, board.EpicCompleted, s.epicStatus(e.ID),
		"all-done completes the epic")
	for _, id := range []string{s1.ID, s2.ID, s3.ID, s4.ID} {
		row, rerr := s.svc.GetTask(ctx, id, "")
		require.NoError(t, rerr, "completed epic keeps its story rows: %s", row.ShortID)
		require.Equal(t, board.StageDone, row.Stage)
	}
	require.Equal(t, 0, s.countEvents("delete_task"),
		"epic completion is status-only — no story row is ever purged")
}

// TestRowLoss_2107_TaskEventsSurviveDelete pins the tombstone read: a
// deleted task's audit timeline stays readable (create + delete events,
// the delete payload's embedded snapshot), addressed by BOTH the short
// id and the UUID — born-red against the pre-fix 404.
func (s *BoardSuite) TestRowLoss_2107_TaskEventsSurviveDelete() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	victim := s.seedTask(e, "victim", nil, false)
	c, err := s.svc.ClaimNext(ctx, "worker-"+ws, ws)
	require.NoError(t, err)
	require.Equal(t, victim.ShortID, c.ShortID)

	_, err = s.svc.DeleteTask(ctx, s.owner(), victim.ID, "", false, "")
	require.NoError(t, err)

	// The row is gone — the pre-fix read sealed here.
	_, err = s.svc.GetTask(ctx, victim.ID, "")
	require.Error(t, err, "the row itself is deleted")

	// The tombstone: addressed by short id.
	events, err := s.svc.TaskEvents(ctx, "", victim.ShortID, 0)
	require.NoError(t, err, "the audit trail outlives the row (short id)")
	actions := make(map[string]bool)
	var deletePayload map[string]any
	for _, ev := range events {
		actions[ev.Action] = true
		if ev.Action == "delete_task" {
			require.NoError(t, json.Unmarshal(ev.Payload, &deletePayload))
		}
	}
	require.True(t, actions["create_task"], "the trail carries the create event")
	require.True(t, actions["delete_task"], "the trail carries the delete event")
	require.Equal(t, victim.ShortID, deletePayload["short_id"],
		"the delete event names the row")
	snap, ok := deletePayload["snapshot"].(map[string]any)
	require.True(t, ok, "the delete event embeds the row snapshot (content preservation)")
	require.Equal(t, victim.ShortID, snap["short_id"], "the snapshot names the row")

	// The tombstone: addressed by UUID (the item_id anchor).
	eventsByUUID, err := s.svc.TaskEvents(ctx, victim.ID, "", 0)
	require.NoError(t, err, "the audit trail outlives the row (UUID anchor)")
	require.Len(t, eventsByUUID, len(events), "both anchors resolve the same trail")

	// The adapter arm: the tombstone names the project — the ownership
	// perimeter's anchor when there is no row.
	_, proj, err := s.svc.TaskEventsTombstone(ctx, victim.ShortID, 0)
	require.NoError(t, err)
	require.Equal(t, p.ID, proj.ID, "the trail's project is the perimeter anchor")
}

// TestRowLoss_2107_TombstoneKeepsTheHonest404 pins the fork's other
// arm: an id with NO trail anywhere returns the 404 (never-existed or
// a below-store loss that left no events) — the tombstone never
// fabricates history, and the did-you-mean contract stays the caller's.
func (s *BoardSuite) TestRowLoss_2107_TombstoneKeepsTheHonest404() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	live := s.seedTask(e, "live", nil, false)

	// Sanity both directions: the live row reads through the ordinary
	// path; the phantom id stays a NotFoundError on every arm.
	_, err := s.svc.TaskEvents(ctx, "", live.ShortID, 0)
	require.NoError(t, err)
	_, err = s.svc.TaskEvents(ctx, "", "tsk-never-minted-anywhere", 0)
	require.Error(t, err)
	var nf *board.NotFoundError
	require.ErrorAs(t, err, &nf, "no trail keeps the 404 class (the below-store fork)")
	_, _, err = s.svc.TaskEventsTombstone(ctx, "tsk-never-minted-anywhere", 0)
	require.ErrorAs(t, err, &nf, "the adapter arm keeps the same 404")
}

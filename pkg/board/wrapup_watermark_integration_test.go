// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// wrapup_watermark_integration_test.go — the wrapup-watermark-diff story's
// born-red pins (epic wrap-up review, low-level design §4-5; high-level D4,
// D5): the delta story list's correctness (first run covers ALL done
// stories ever via '-infinity'; evidence fallback chain; re-done rows at
// their LATEST done; stories completing mid-review fall in the NEXT window
// — advance-to-mint-time) and the charter's frozen-at-mint contract.
//
// Born-red mapping (README-LLM rule 0):
//
//   - [1] first run covers ALL done stories ever — a legacy row whose
//     latest →done transition PREDATES the epic's created_at is still
//     covered ('-infinity', §5 — RED before: the engine's minimal query
//     bounded at epic.created_at and skipped it).
//   - [1] evidence fallback chain — transition evidence_url → latest
//     verdict's evidenceUrl → "(none recorded)" (RED before: no verdict
//     fallback; the verdict event carried no evidenceUrl at all).
//   - [1] advance-to-mint-time — stories completing while a wrap-up is in
//     flight fall in the NEXT window (D4): no double-cover, no skip; the
//     next charter's window-from is the previous row's MINT time.
//   - [2] the charter is frozen at mint: later story activity never
//     rewrites a minted description (regression pin; the renderer's
//     determinism pins live in wrapup_charter_unit_test.go).
package board_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// rawDoneAt force-writes a done story's ONLY (latest) done transition at an
// arbitrary time — the legacy-row fixture: rows whose append-only evidence
// predates the epic itself (imported boards, clock skew). board_transitions
// is raw evidence; `at` is insertable by design (DEFAULT now()).
func (s *BoardSuite) rawDoneAt(taskID string, at time.Time) {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `UPDATE tasks SET stage='done' WHERE id=$1`, taskID)
	s.Require().NoError(err)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO board_transitions (task_id, from_stage, to_stage, evidence_url, actor, at)
		VALUES ($1, 'backlog', 'done', NULL, '{"kind":"user"}'::jsonb, $2)`, taskID, at)
	s.Require().NoError(err)
}

// nullLatestDoneEvidence erases the evidence_url on a story's latest →done
// transition (the legacy/edge shape the verdict fallback exists for).
func (s *BoardSuite) nullLatestDoneEvidence(taskID string) {
	_, err := s.pool.Exec(context.Background(), `
		UPDATE board_transitions SET evidence_url=NULL
		WHERE id = (SELECT id FROM board_transitions
		            WHERE task_id=$1 AND to_stage='done'
		            ORDER BY at DESC, id DESC LIMIT 1)`, taskID)
	s.Require().NoError(err)
}

// settleWrapUpRaw arranges the POST-verdict epic state (advanceWatermarkTx's
// settle, standing in for the wrapup-gap-recursion story's RecordWrapUpVerdict
// — the only production caller): watermark at the closing row's MINT time,
// ledger +1, in-flight pointer cleared.
func (s *BoardSuite) settleWrapUpRaw(epicID, wrapupTaskID string, mintAt time.Time) {
	_, err := s.pool.Exec(context.Background(), `
		UPDATE epics SET last_wrapped_at=$2, wrapup_count=wrapup_count+1, wrapup_task_id=NULL
		WHERE id=$1 AND wrapup_task_id=$3`, epicID, mintAt, wrapupTaskID)
	s.Require().NoError(err)
}

// charterLine finds the charter's story-list line naming shortID.
func charterLine(t *testing.T, desc, shortID string) string {
	t.Helper()
	for _, line := range strings.Split(desc, "\n") {
		if strings.Contains(line, shortID) {
			return line
		}
	}
	t.Fatalf("charter line for %s not found in description:\n%s", shortID, desc)
	return ""
}

// managerCloseWrapup force-mets a wrap-up row's engine-seeded criteria and
// closes it via the manager →done (the documented mid-sequence posture —
// legal, and its doneCascade runs the wrap-up seam).
func (s *BoardSuite) managerCloseWrapup(svc *board.Service, w *board.Task) {
	_, err := s.pool.Exec(context.Background(), `
		UPDATE tasks SET acceptance_criteria='[{"text":"c1","met":true},{"text":"c2","met":true},{"text":"c3","met":true}]'::jsonb
		WHERE id=$1`, w.ID)
	s.Require().NoError(err)
	_, err = svc.MoveStage(context.Background(), s.owner(), "", w.ShortID,
		board.StageDone, "https://ev.test/close-"+w.ShortID, "")
	s.Require().NoError(err)
}

// --- [1] First run covers ALL done stories ever ('-infinity', §5) -----------

func (s *BoardSuite) TestWrapUpDelta_FirstRunCoversAllDoneStories() {
	t := s.T()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	// A legacy row: done BEFORE the epic's own created_at (imported board /
	// backdated evidence). First-run coverage must include it — the §5
	// predicate is coalesce(last_wrapped_at, '-infinity'), NOT
	// epic.created_at. All stories are seeded BEFORE any completion: the
	// drain fires at the last existing done, and the fixture must be in
	// place by then.
	sOld := s.seedTask(e, "legacy", nil, true)
	epoch := s.reloadEpic(e.ID).CreatedAt
	s.rawDoneAt(sOld.ID, epoch.Add(-2*time.Hour))

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID) // the drain: mints here

	w := s.soleWrapupTask(svc)
	require.Contains(t, w.Description, "Stories in this window (3)",
		"first run covers ALL done stories ever — the legacy row included")
	for _, id := range []string{sOld.ShortID, s1.ShortID, s2.ShortID} {
		require.Contains(t, w.Description, id, "story %s listed", id)
	}

	// The rendered window pair (D5): [coalesce(last_wrapped_at,
	// epic.created_at), the wrap-up row's mint moment] — RFC 3339 both.
	require.Contains(t, w.Description, "  from "+epoch.Format(time.RFC3339),
		"window-from renders epic.created_at on the first run (NULL watermark)")
	require.Contains(t, w.Description, "  to   "+w.CreatedAt.Format(time.RFC3339),
		"window-to renders the wrap-up row's own created_at (the mint moment)")

	// The covered story carries its evidence: s1/s2 completed with evidence
	// URLs; the legacy row has none → "(none recorded)".
	require.Contains(t, charterLine(t, w.Description, s1.ShortID), "evidence: https://ev.test/"+s1.ShortID)
	require.Contains(t, charterLine(t, w.Description, sOld.ShortID), "evidence: (none recorded)")
}

// --- [1] Evidence fallback chain: transition → verdict → (none) -------------

func (s *BoardSuite) TestWrapUpDelta_EvidenceFallbackChain() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	// All four stories seeded up front (the drain fires at the last
	// existing done; the evidence fixtures must be in place by then —
	// the charter freezes at mint).
	sA := s.seedTask(e, "sA", nil, true) // transition evidence (the worker's →done URL)
	sB := s.seedTask(e, "sB", nil, true) // verdict evidence (transition evidence lost)
	sC := s.seedTask(e, "sC", nil, true) // nothing recorded anywhere
	sD := s.seedTask(e, "sD", nil, true) // the drain trigger (plain evidence)

	// sA: the plain worker path — done with a transition evidence URL.
	s.claimAndComplete(svc, ws, sA.ShortID)

	// sB: approved through the verdict channel with an evidenceUrl, then
	// the transition's evidence is lost (the legacy shape the fallback
	// exists for) — the charter must recover the VERDICT's evidenceUrl.
	cB, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(sB.ID, cB.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(cB.Assignee, cB.ClaimToken), "", sB.ShortID,
		board.StageInReview, "", "")
	s.Require().NoError(err)
	_, _, err = svc.RecordVerdict(ctx, s.owner(), sB.ID, sB.ShortID,
		board.VerdictInput{Verdict: board.VerdictApprove, Reviewer: "rev-fallback",
			EvidenceURL: "https://verdict.test/" + sB.ShortID}, "")
	s.Require().NoError(err)
	s.nullLatestDoneEvidence(sB.ID)

	// sC: done with evidence, then the evidence is lost and NO verdict
	// exists — the honest "(none recorded)".
	s.claimAndComplete(svc, ws, sC.ShortID)
	s.nullLatestDoneEvidence(sC.ID)

	// sD completes the drain — the charter mints with the chain as-is.
	s.claimAndComplete(svc, ws, sD.ShortID)

	w := s.soleWrapupTask(svc)
	require.Contains(t, w.Description, "Stories in this window (4)")
	require.Contains(t, charterLine(t, w.Description, sA.ShortID),
		"evidence: https://ev.test/"+sA.ShortID, "chain link 1: the done transition's evidence_url")
	require.Contains(t, charterLine(t, w.Description, sB.ShortID),
		"evidence: https://verdict.test/"+sB.ShortID,
		"chain link 2: the latest verdict's evidenceUrl when the transition carries none")
	require.Contains(t, charterLine(t, w.Description, sC.ShortID),
		"evidence: (none recorded)", "chain link 3: nothing recorded anywhere")
}

// --- [1] Advance-to-mint-time: mid-review completions fall in the NEXT window

func (s *BoardSuite) TestWrapUpDelta_MidReviewCompletionFallsInNextWindow() {
	t := s.T()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvc(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)

	// s1 completes first (no drain — s2 still open), then s2's done drains
	// the epic: wrap-up #1 mints (window: epic start → W1's mint moment).
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w1 := s.soleWrapupTask(svc)
	require.Contains(t, w1.Description, "Stories in this window (2)",
		"W1 covers both stories of the first window")
	desc1 := w1.Description

	// A story completes WHILE the review is in flight: its done transition
	// falls AFTER W1's mint snapshot → NEXT window (no double-cover of the
	// already-covered, no skip of the newcomer).
	s.reopenStoryToInProgress(s1.ID)
	s.managerCompleteUnclaimed(svc, s1.ShortID)

	// The verdict close (the settle's state, raw here — the verdict verb is
	// the wrapup-gap-recursion story's; advanceWatermarkTx is pinned
	// directly in wrapup_settle_internal_test.go): watermark at W1's MINT
	// time, ledger +1, pointer cleared.
	s.settleWrapUpRaw(e.ID, w1.ID, w1.CreatedAt)

	// Closing W1 runs the seam in the same transaction: drained, pointer
	// clear, under the cap → W2 mints scoped to the delta.
	s.managerCloseWrapup(svc, w1)
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE kind='wrapup'`).Scan(&n))
	require.Equal(t, 2, n, "the delta wrap-up minted")

	var w2ID string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT id FROM tasks WHERE kind='wrapup' AND id <> $1`, w1.ID).Scan(&w2ID))
	w2, err := svc.GetTask(context.Background(), w2ID, "")
	s.Require().NoError(err)
	require.Contains(t, w2.Title, "(delta 2)", "the second wrap-up is explicitly a delta")

	require.Contains(t, w2.Description, "Stories in this window (1)",
		"the delta covers ONLY the mid-review completion")
	require.Contains(t, w2.Description, s1.ShortID, "the story done mid-review is covered — no skip")
	require.NotContains(t, w2.Description, s2.ShortID,
		"the story already covered by W1 is NOT re-listed — no double-cover")

	// D4 end-to-end: the next window STARTS at the previous wrap-up's MINT
	// time (its created_at — captured at mint, NOT its close time).
	require.Contains(t, w2.Description, "  from "+w1.CreatedAt.Format(time.RFC3339),
		"advance-to-mint-time: window-from is W1's mint moment")

	// The frozen charter: W1's description never re-renders — byte-identical
	// after all later activity ([2]'s convergence pin at the row level).
	w1After, err := svc.GetTask(context.Background(), w1.ID, "")
	s.Require().NoError(err)
	require.Equal(t, desc1, w1After.Description,
		"the charter is frozen on the row at mint — later activity never rewrites it")

	// The epic's watermark itself sits at W1's mint moment.
	var wm *time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT last_wrapped_at FROM epics WHERE id=$1`, e.ID).Scan(&wm))
	require.NotNil(t, wm)
	require.WithinDuration(t, w1.CreatedAt, *wm, time.Second,
		"the settle placed the watermark at the wrap-up row's mint time")
}

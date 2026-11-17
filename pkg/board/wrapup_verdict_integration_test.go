// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// wrapup_verdict_integration_test.go — the wrapup-gap-recursion story's
// born-red pins (epic wrap-up review, low-level design §§4, 8-9; high-level
// D6, D7): the close verb (RecordWrapUpVerdict), the gap discipline, the
// cap/ack state machine, and the manager-override posture.
//
// Born-red mapping (README-LLM rule 0 — these pins land BEFORE the
// implementation and redden against the pre-verdict engine, where a
// wrap-up row can only be closed by the manager raw-done with the pointer
// parking):
//
//   - [0] PASS completes the epic + advances the watermark ATOMICALLY and
//     is a TERMINUS: no fresh mint, no ack-wait, at the cap or below; the
//     verdict path never re-runs the seam; a story reopened under PASS
//     (0-row completion) leaves the epic open with no mint. GAPS closes
//     the row done, validates gapRefs (wrong-epic / untagged / pre-mint /
//     empty-on-gaps all refuse), and holds the epic open via the ordinary
//     drain predicate — no bounce, no implementer re-dispatch.
//   - [1] verdicts are auditable board_events with deterministic ids
//     (replay converges); the claim-token guard refuses wrong/absent
//     tokens; Done-guard parity (criteria + reportUrl) enforced
//     engine-side regardless of reviewer role.
//   - [2] the recursion cap: at 3 consecutive closes the next DRAIN arms
//     ack-wait (event + epic state + health surface, NO mint; never from
//     a verdict); board ack resets the ledger, clears the pointer, and
//     re-drains (an acked drained epic mints immediately).
//   - [3] gap completion triggers the next delta-scoped wrap-up (the
//     watermark this story's verb advances); a manager raw-done on a
//     wrap-up row parks the pointer until ack (fail-visible, never
//     wedged).
package board_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// wrapSvcFaultable is wrapSvc + the suite's failpoint seam (the verdict
// transaction's mid-settle fault pin needs the armed hook).
func (s *BoardSuite) wrapSvcFaultable(sp *fakeSpawner) *board.Service {
	if sp == nil {
		sp = &fakeSpawner{}
	}
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(sp.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithRoleDirectory(wrapupDir()),
		board.WithFailpoint(s.fps.fp))
}

// reviewerActor is the wrap-up reviewer's agent actor holding the row's
// LIVE claim token (the in-flight guard from its charter envelope).
func (s *BoardSuite) reviewerActor(w *board.Task) board.Actor {
	return s.agent("wrapup-reviewer", w.ClaimToken)
}

// checkWrapupCriteria walks the engine-seeded criteria through the #1946
// reviewer grant (the row's own persona claim + live token) — the
// Done-guard parity the verdict requires.
func (s *BoardSuite) checkWrapupCriteria(svc *board.Service, w *board.Task) {
	for i := range w.AcceptanceCriteria {
		_, err := svc.SetCriteriaMet(context.Background(), s.reviewerActor(w), w.ID, "",
			board.CriteriaCheck{Index: i, Met: true}, "")
		s.Require().NoError(err)
	}
}

// wrapupMintAt reads the wrap-up row's MINT timestamp (created_at — the
// D4 snapshot the watermark must take, never close time).
func (s *BoardSuite) wrapupMintAt(id string) time.Time {
	var at time.Time
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT created_at FROM tasks WHERE id=$1`, id).Scan(&at))
	return at
}

// wrapupMintText renders the wrap-up row's mint timestamp as text (the
// same ::text domain epicWrapup renders last_wrapped_at in — exact
// equality across the two columns is the D4 pin).
func (s *BoardSuite) wrapupMintText(id string) string {
	var txt string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT created_at::text FROM tasks WHERE id=$1`, id).Scan(&txt))
	return txt
}

// countVerdictRowsFor counts the CANONICAL verdict rows for one wrap-up
// row (the deterministic wrapup:verdict:<taskID> id — one per row, ever;
// the requestID-keyed eventWriter row shares the action but never the id).
func (s *BoardSuite) countVerdictRowsFor(taskID string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE id=$1`,
		"wrapup:verdict:"+taskID).Scan(&n))
	return n
}

// liveWrapupTask resolves the epic's CURRENT in-flight wrap-up row (the
// pointer target) — the multi-cycle shape, where done wrap-up rows from
// earlier cycles accumulate in the board.
func (s *BoardSuite) liveWrapupTask(svc *board.Service, epicID string) *board.Task {
	var id string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT wrapup_task_id::text FROM epics WHERE id=$1`, epicID).Scan(&id))
	s.Require().NotEmpty(id, "the epic carries an in-flight wrap-up pointer")
	t, err := svc.GetTask(context.Background(), id, "")
	s.Require().NoError(err)
	s.Require().Equal(board.StageInReview, t.Stage, "the pointer names a live review")
	return t
}

// verdictEventPayload decodes the deterministic wrapup:verdict:<taskID>
// event row's payload (the one-verdict-per-row canonical record).
func (s *BoardSuite) verdictEventPayload(taskID string) map[string]any {
	var payload []byte
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT payload FROM board_events WHERE id=$1`,
		"wrapup:verdict:"+taskID).Scan(&payload))
	var m map[string]any
	s.Require().NoError(json.Unmarshal(payload, &m))
	return m
}

// drainToMint seeds n stories, completes them all through the real engine
// path, and returns the minted wrap-up row (exactly one).
func (s *BoardSuite) drainToMint(svc *board.Service, ws string, e *board.Epic, n int) *board.Task {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("s%d", i+1)
	}
	for _, name := range names {
		st := s.seedTask(e, name, nil, true)
		s.claimAndComplete(svc, ws, st.ShortID)
	}
	return s.soleWrapupTask(svc)
}

// passVerdict is the happy PASS close (criteria checked, live token).
func (s *BoardSuite) passVerdict(svc *board.Service, w *board.Task, requestID string) (*board.Task, *board.Epic) {
	s.checkWrapupCriteria(svc, w)
	t, e, err := svc.RecordWrapUpVerdict(context.Background(), s.reviewerActor(w), w.ID, "",
		board.WrapUpVerdictInput{
			Verdict:   board.WrapUpVerdictPass,
			ReportURL: "https://ev.test/wrapup-report-" + w.ShortID,
			Reviewer:  "wrapup-reviewer",
		}, requestID)
	s.Require().NoError(err)
	return t, e
}

// --- [0]+[1] PASS: the atomic, terminal close --------------------------------

func (s *BoardSuite) TestWrapUp_PassVerdictCompletesEpicAtomically() {
	t := s.T()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	sp := &fakeSpawner{}
	svc := s.wrapSvcFaultable(sp)
	e := s.seedWrapEpic(svc, p, true)

	w := s.drainToMint(svc, ws, e, 2)

	task, epic := s.passVerdict(svc, w, "rq-pass-1")

	require.Equal(t, board.StageDone, task.Stage, "the verdict closes the wrap-up row done")
	require.Equal(t, board.EpicCompleted, epic.Status, "PASS completes the epic (same transaction)")

	st := s.epicWrapup(e.ID)
	require.Nil(t, st.TaskID, "the in-flight pointer settled")
	require.Equal(t, 1, st.Count, "the consecutive-close ledger advanced")
	require.NotNil(t, st.LastWrapped, "the watermark advanced")
	require.Equal(t, s.wrapupMintText(w.ID), *st.LastWrapped,
		"D4: the watermark takes the row's MINT time, never close time")

	// TERMINUS pins: no fresh mint, no ack-wait, epic_complete exactly
	// once with the deterministic id. (A seam re-run here would mint an
	// empty-delta wrap-up — the drained epic has all stories done.)
	require.Equal(t, 1, s.countWrapupRows(), "PASS is a terminus: NO fresh mint")
	require.Equal(t, 1, s.countEvents("wrapup_mint"), "no second mint event")
	require.False(t, st.AckPending, "the cap does not gate the terminus")
	require.Equal(t, 0, s.countEvents("wrapup_ack_required"), "ack-wait arms ONLY on drain-mint paths")

	require.Equal(t, 1, s.countVerdictRowsFor(w.ID), "the canonical verdict event")
	payload := s.verdictEventPayload(w.ID)
	require.Equal(t, "pass", payload["verdict"])
	require.NotEmpty(t, payload["report_url"], "the report URL rides the audit payload")
	require.Equal(t, 1, s.countEvents("wrapup_epic_complete"), "the completion event rides the verdict transaction")

	// The →done transition carries the report as evidence.
	var evidence string
	s.Require().NoError(s.pool.QueryRow(context.Background(), `
		SELECT evidence_url FROM board_transitions
		WHERE task_id=$1 AND to_stage='done'
		ORDER BY at DESC, id DESC LIMIT 1`, w.ID).Scan(&evidence))
	require.Equal(t, "https://ev.test/wrapup-report-"+w.ShortID, evidence)

	// Replay convergence: the same call again (the row is done) is a
	// stale replay — 200-with-original, nothing double-lands.
	task2, epic2, err := svc.RecordWrapUpVerdict(context.Background(), s.reviewerActor(w), w.ID, "",
		board.WrapUpVerdictInput{
			Verdict:   board.WrapUpVerdictPass,
			ReportURL: "https://ev.test/wrapup-report-" + w.ShortID,
			Reviewer:  "wrapup-reviewer",
		}, "rq-pass-1-replay")
	s.Require().NoError(err, "a stale replay is 200-with-original, never an error")
	require.Equal(t, board.StageDone, task2.Stage)
	require.Equal(t, board.EpicCompleted, epic2.Status)
	require.Equal(t, 1, s.countVerdictRowsFor(w.ID), "one verdict per row, ever")
	require.Equal(t, 1, s.epicWrapup(e.ID).Count, "no double ledger advance")
	require.Equal(t, 1, s.countWrapupRows(), "still no mint")
}

// TestWrapUp_PassAtExactlyTheCapStillCompletes pins the loop-bound: the
// cap bounds the MINT loop (a pedantic gap-filer), never the terminus.
func (s *BoardSuite) TestWrapUp_PassAtExactlyTheCapStillCompletes() {
	t := s.T()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	w := s.drainToMint(svc, ws, e, 1)

	// The ledger stands at exactly the cap (two gap cycles closed before
	// this one — simulated raw; the verdict owns the real increment).
	_, err := s.pool.Exec(context.Background(),
		`UPDATE epics SET wrapup_count=$2 WHERE id=$1`, e.ID, svc.Config().WrapUpConsecutiveCap)
	s.Require().NoError(err)

	task, epic := s.passVerdict(svc, w, "")
	require.Equal(t, board.StageDone, task.Stage)
	require.Equal(t, board.EpicCompleted, epic.Status,
		"PASS at exactly the cap ALSO completes — the terminus is not the loop")
	st := s.epicWrapup(e.ID)
	require.False(t, st.AckPending, "a verdict NEVER arms ack-wait")
	require.Equal(t, 0, s.countEvents("wrapup_ack_required"))
	require.Equal(t, 1, s.countWrapupRows(), "no mint")
}

// TestWrapUp_PassUnderReopenedStory pins the 0-row completion shape: a
// story reopened under the PASS verdict leaves the epic open (the honest
// outcome), with no mint — the drain seam re-arms on its own schedule.
func (s *BoardSuite) TestWrapUp_PassUnderReopenedStory() {
	t := s.T()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w := s.soleWrapupTask(svc)

	// The story reopens BETWEEN mint and verdict (the manager reopen).
	s.reopenStoryToInProgress(s2.ID)

	task, epic := s.passVerdict(svc, w, "")
	require.Equal(t, board.StageDone, task.Stage, "the verdict still closes the row")
	require.Equal(t, board.EpicActive, epic.Status,
		"0-row completion: the reopened story holds the epic open")
	st := s.epicWrapup(e.ID)
	require.Equal(t, 1, st.Count, "the close still advanced the ledger + watermark")
	require.NotNil(t, st.LastWrapped)
	require.Equal(t, 1, s.countWrapupRows(), "no mint — the verdict path never runs the seam")
	require.Equal(t, 0, s.countEvents("wrapup_epic_complete"), "no completion event rode the tx")

	// The epic re-arms on its own schedule: completing s2 again runs the
	// DRAIN seam (the only mint path) — a fresh, delta-scoped wrap-up.
	s.managerCompleteUnclaimed(svc, s2.ShortID)
	require.Equal(t, 2, s.countWrapupRows(), "the ordinary drain minted the next wrap-up")
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID), "the new review defers completion")
}

// --- [0] GAPS: ref validation + the ordinary holding --------------------------

func (s *BoardSuite) TestWrapUp_GapsVerdictRefValidation() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w := s.soleWrapupTask(svc)
	s.checkWrapupCriteria(svc, w)

	// A same-epic gap story filed AFTER mint through the NORMAL create
	// path (tag 'gap') — the charter discipline the engine validates.
	g1, err := svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "gap-1", Description: "Seam gap filed by the wrap-up reviewer: the integration seam between the two stories is unpinned.",
			Tags:               []string{"gap"},
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "fix the seam"}}}, "")
	s.Require().NoError(err)

	// A pre-mint row (the epic's own s1) tagged gap — filed BEFORE the
	// wrap-up mint: refuses.
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET tags=array_append(tags,'gap') WHERE id=$1`, s1.ID)
	s.Require().NoError(err)

	// A wrong-epic gap row (a sibling epic in the same project).
	e2 := s.seedWrapEpic(svc, p, false)
	foreign, err := svc.CreateTask(ctx, s.owner(), e2.ID, "",
		board.TaskInput{Title: "gap-foreign", Description: "A gap-tagged story in a DIFFERENT epic (the wrong-epic refusal fixture).",
			Tags:               []string{"gap"},
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "x"}}}, "")
	s.Require().NoError(err)

	// An untagged same-epic row filed after mint.
	plain, err := svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "plain", Description: "An untagged same-epic row filed after mint (the untagged refusal fixture).",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "x"}}}, "")
	s.Require().NoError(err)

	gapsInput := func(refs ...string) board.WrapUpVerdictInput {
		return board.WrapUpVerdictInput{
			Verdict:   board.WrapUpVerdictGaps,
			ReportURL: "https://ev.test/gaps-report",
			GapRefs:   refs,
			Reviewer:  "wrapup-reviewer",
		}
	}
	for name, ref := range map[string]string{
		"wrong-epic ref":  foreign.ShortID,
		"untagged ref":    plain.ShortID,
		"pre-mint ref":    s1.ShortID,
		"nonexistent ref": "tsk-no-such-gap-row",
		"uuid of a story": s1.ID,
	} {
		_, _, err := svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "", gapsInput(ref), "")
		s.Require().Error(err, name)
		require.Contains(t, err.Error(), "gap", name+": the refusal names the gap discipline")
		require.Equal(t, board.StageInReview, s.reloadTask(w.ID).Stage, name+": the row stays open")
	}

	// Empty-on-gaps refuses at validation (the iff rule).
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "", gapsInput(), "")
	s.Require().Error(err, "a gaps verdict requires at least one gapRef")

	// The healthy GAPS close: the row lands done, the epic stays open via
	// the ordinary drain predicate (the non-done gap rows are the
	// holders), NO bounce — the wrap-up row is done, not re-dispatched.
	task, epic, err := svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "",
		gapsInput(g1.ShortID), "rq-gaps-1")
	s.Require().NoError(err)
	require.Equal(t, board.StageDone, task.Stage, "GAPS closes the row done (the report is evidence)")
	require.Equal(t, board.EpicActive, epic.Status, "the gap rows hold the epic open")
	st := s.epicWrapup(e.ID)
	require.Equal(t, 1, st.Count)
	require.Nil(t, st.TaskID, "the pointer settled")
	require.Equal(t, 1, s.countWrapupRows(), "no mint from a verdict")

	payload := s.verdictEventPayload(w.ID)
	require.Equal(t, "gaps", payload["verdict"])
	require.Contains(t, fmt.Sprint(payload["gap_refs"]), g1.ShortID, "the refs ride the audit payload")

	// No bounce: the wrap-up row never returned to in_progress, no fresh
	// claim was minted on it (its claim fields cleared with the close).
	row := s.rawTask(w.ID)
	require.Equal(t, board.StageDone, row.Stage)
	require.Empty(t, row.ClaimToken, "the close cleared the claim — no re-dispatch")

	// The plain backlog row (untagged, filed mid-review) is simply part
	// of the next delta: it also holds the epic by the same predicate.
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID))
}

// TestWrapUp_GapCompletionTriggersDeltaScopedMint pins the loop's engine
// side (§2 of the high-level lifecycle): gap stories complete → the drain
// seam mints the NEXT wrap-up scoped to the delta since the watermark THIS
// verb advanced.
func (s *BoardSuite) TestWrapUp_GapCompletionTriggersDeltaScopedMint() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s2 := s.seedTask(e, "s2", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w1 := s.soleWrapupTask(svc)
	w1Mint := s.wrapupMintAt(w1.ID)

	g1, err := svc.CreateTask(ctx, s.owner(), e.ID, "",
		board.TaskInput{Title: "gap-1", Description: "Gap story: the delta-scoped re-review's covered row.",
			Tags:               []string{"gap"},
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "fix", Met: true}}}, "")
	s.Require().NoError(err)

	s.checkWrapupCriteria(svc, w1)
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w1), w1.ID, "",
		board.WrapUpVerdictInput{
			Verdict:   board.WrapUpVerdictGaps,
			ReportURL: "https://ev.test/gaps-1",
			GapRefs:   []string{g1.ShortID},
			Reviewer:  "wrapup-reviewer",
		}, "")
	s.Require().NoError(err)
	require.Equal(t, s.wrapupMintText(w1.ID), *s.epicWrapup(e.ID).LastWrapped,
		"the verdict's settle advanced the watermark to W1's mint")

	// The gap story completes through the real engine path: the drain
	// seam mints W2 — delta-scoped (window-from == W1's mint; the
	// covered list is the gap row alone, not the whole epic again).
	c, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(g1.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
		board.StageDone, "https://ev.test/gap-1-done", "")
	s.Require().NoError(err)

	require.Equal(t, 2, s.countWrapupRows(), "the gap completion minted the next wrap-up")
	w2 := s.reloadEpic(e.ID)
	require.NotNil(t, w2.WrapUpTaskID, "W2 is the in-flight review")

	// W2's charter window starts at the watermark (delta-scoped).
	var w2ID string
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT id FROM tasks WHERE kind='wrapup' AND stage='in_review'`).Scan(&w2ID))
	w2row, err := svc.GetTask(ctx, w2ID, "")
	s.Require().NoError(err)
	require.Contains(t, w2row.Description, "from "+w1Mint.UTC().Format(time.RFC3339),
		"the next window's from-bound is the advanced watermark (the delta this story's verb carved)")
	require.Contains(t, w2row.Description, g1.ShortID, "the delta list covers the gap story")
	require.NotContains(t, w2row.Description, s1.ShortID,
		"the already-covered stories do not re-enter the delta")
}

// --- [1] The guard chain ------------------------------------------------------

func (s *BoardSuite) TestWrapUp_VerdictGuardChain() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)
	w := s.soleWrapupTask(svc)

	in := board.WrapUpVerdictInput{
		Verdict:   board.WrapUpVerdictPass,
		ReportURL: "https://ev.test/report",
		Reviewer:  "wrapup-reviewer",
	}

	// Kind guard: a story row is not a wrap-up row (wrong object, loud).
	_, _, err := svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), s1.ID, "", in, "")
	s.Require().Error(err, "a story row refuses the wrap-up verdict")
	require.Contains(t, err.Error(), "wrap-up")

	// Claim-token guard: wrong token, absent token.
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.agent("impostor", "wrong-token"), w.ID, "", in, "")
	s.Require().Error(err, "a wrong token refuses")
	require.Contains(t, err.Error(), board.GuardClaimToken)
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.agent("tokenless", ""), w.ID, "", in, "")
	s.Require().Error(err, "an absent token refuses")
	require.Contains(t, err.Error(), board.GuardClaimToken)
	// The owner's own token-less call refuses too — the schema is
	// engine-owned regardless of reviewer role.
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.owner(), w.ID, "", in, "")
	s.Require().Error(err, "even the owner carries the live token or refuses")
	require.Contains(t, err.Error(), board.GuardClaimToken)

	// Done-guard parity: criteria unmet (engine-seeded, born unmet).
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "", in, "")
	s.Require().Error(err, "unmet criteria refuse (Done-guard parity)")
	require.Contains(t, err.Error(), board.GuardDone)

	// Criteria met but no report URL: still refuses (the schema layer —
	// the report is the close's evidence on both verdicts).
	s.checkWrapupCriteria(svc, w)
	noReport := in
	noReport.ReportURL = ""
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "", noReport, "")
	s.Require().Error(err, "a missing reportUrl refuses (both verdicts require it)")
	require.Contains(t, err.Error(), "reportUrl")

	// Enum discipline: an unknown verdict value refuses at validation.
	bogus := board.WrapUpVerdictInput{Verdict: "approve", ReportURL: "https://ev.test/x", Reviewer: "r"}
	_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "", bogus, "")
	s.Require().Error(err, "the verdict enum is closed (pass|gaps)")

	// Nothing above moved the row.
	require.Equal(t, board.StageInReview, s.reloadTask(w.ID).Stage)
	require.Equal(t, 0, s.epicWrapup(e.ID).Count, "no guard refusal advanced anything")
}

// TestWrapUp_VerdictFailpointAtomicity pins the single-transaction
// contract (AC[0]): a fault between the row's →done and the epic settle
// aborts BOTH — no half-closed wrap-up ever observes.
func (s *BoardSuite) TestWrapUp_VerdictFailpointAtomicity() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	w := s.drainToMint(svc, ws, e, 1)
	s.checkWrapupCriteria(svc, w)

	s.fps.armAt("wrapup_verdict.settle", fmt.Errorf("injected crash mid-settle"))
	_, _, err := svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "",
		board.WrapUpVerdictInput{Verdict: board.WrapUpVerdictPass,
			ReportURL: "https://ev.test/fp", Reviewer: "wrapup-reviewer"}, "")
	s.Require().Error(err, "the fault surfaces")
	require.Equal(t, 1, s.fps.hitsFor("wrapup_verdict.settle"), "the fault path actually ran")

	st := s.epicWrapup(e.ID)
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID), "the epic did not complete")
	require.Equal(t, 0, st.Count, "no ledger advance")
	require.Nil(t, st.LastWrapped, "no watermark advance")
	require.NotNil(t, st.TaskID, "the pointer still names the row")
	row := s.reloadTask(w.ID)
	require.Equal(t, board.StageInReview, row.Stage, "the row is NOT half-closed")
	require.NotEmpty(t, row.ClaimToken, "the reviewer's claim survived — the retry is live")

	// The retry (fault cleared) converges to the healthy close.
	s.fps.disarm("wrapup_verdict.settle")
	_, epic, err := svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "",
		board.WrapUpVerdictInput{Verdict: board.WrapUpVerdictPass,
			ReportURL: "https://ev.test/fp", Reviewer: "wrapup-reviewer"}, "")
	s.Require().NoError(err)
	require.Equal(t, board.EpicCompleted, epic.Status, "the retry completes")
	require.Equal(t, 1, s.countVerdictRowsFor(w.ID), "exactly one canonical verdict row (converged)")
}

// --- [2] The cap/ack state machine --------------------------------------------

// TestWrapUp_AckResetsLedgerAndRedrains walks the FULL loop: three
// consecutive GAPS closes drive the ledger to the cap; the next drain
// arms ack-wait (no 4th mint — the engine story's seam pin, here through
// the REAL verdict increments); the human ack resets the ledger, clears
// the pointer, and re-drains — an acked drained epic mints immediately.
func (s *BoardSuite) TestWrapUp_AckResetsLedgerAndRedrains() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	sp := &fakeSpawner{}
	svc := s.wrapSvcFaultable(sp)
	e := s.seedWrapEpic(svc, p, true)

	capN := svc.Config().WrapUpConsecutiveCap
	require.Equal(t, 3, capN, "the design's pinned cap (default 3)")

	s1 := s.seedTask(e, "s1", nil, true)
	s.claimAndComplete(svc, ws, s1.ShortID)

	// capN consecutive GAPS cycles, each through the REAL verdict close
	// (the ledger increments only through advanceWatermarkTx).
	for i := 0; i < capN; i++ {
		w := s.liveWrapupTask(svc, e.ID)
		s.Require().Equal(board.StageInReview, w.Stage, "cycle %d: the epic's live wrap-up", i)
		g, err := svc.CreateTask(ctx, s.owner(), e.ID, "",
			board.TaskInput{Title: fmt.Sprintf("gap-%d", i), Description: fmt.Sprintf("Cycle %d's gap story: pin the delta the wrap-up flagged.", i),
				Tags:               []string{"gap"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "fix", Met: true}}}, "")
		s.Require().NoError(err)
		s.checkWrapupCriteria(svc, w)
		_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "",
			board.WrapUpVerdictInput{Verdict: board.WrapUpVerdictGaps,
				ReportURL: fmt.Sprintf("https://ev.test/gaps-%d", i),
				GapRefs:   []string{g.ShortID}, Reviewer: "wrapup-reviewer"}, "")
		s.Require().NoError(err, "cycle %d: the gaps close", i)

		// The gap completes → the drain mints the next wrap-up — EXCEPT
		// after the cap-th close, where the drain arms ack-wait instead.
		c, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
		s.Require().NoError(err)
		_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
			board.StageDone, fmt.Sprintf("https://ev.test/gap-%d-done", i), "")
		s.Require().NoError(err)
	}
	require.Equal(t, capN, s.epicWrapup(e.ID).Count, "the ledger stands at the cap (real increments)")

	// The drain at the cap armed ack-wait: no 4th mint, the flag set,
	// the epic parked visibly — including on the health surface.
	require.Equal(t, capN, s.countWrapupRows(), "no wrap-up beyond the cap-th row")
	require.True(t, s.epicWrapup(e.ID).AckPending, "wrapup_ack_pending armed")
	require.Equal(t, 1, s.countEvents("wrapup_ack_required"), "the loud arm event")
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID))

	health, err := svc.BoardHealth(ctx, testUser)
	s.Require().NoError(err)
	require.Len(t, health.AckWaits, 1, "board_health carries the ack_waits section")
	aw := health.AckWaits[0]
	require.Equal(t, e.ID, aw.EpicID)
	require.Equal(t, capN, aw.WrapUpCount)
	require.Equal(t, capN, aw.Cap)
	require.True(t, aw.AckPending)

	// The human ack (manager verb): resets the ledger, clears the flag,
	// and re-drains — the still-drained epic mints IMMEDIATELY.
	acked, err := svc.AckWrapUp(ctx, s.owner(), e.ID, "", "rq-ack-1")
	s.Require().NoError(err)
	require.Equal(t, board.EpicActive, acked.Status, "the fresh review defers completion again")
	st := s.epicWrapup(e.ID)
	require.Equal(t, 0, st.Count, "the ack reset the ledger (fresh, like rearm)")
	require.False(t, st.AckPending)
	require.Equal(t, capN+1, s.countWrapupRows(), "the acked drain minted the next wrap-up immediately")
	require.Equal(t, 1, s.countEvents("wrapup_ack"), "the wrapup_ack event")

	// The epic read carries the wrapup surface (count, cap, ack state).
	got := s.reloadEpic(e.ID)
	require.Equal(t, svc.Config().WrapUpConsecutiveCap, got.WrapUpCap, "epic reads carry the cap")
	require.False(t, got.WrapUpAckPending)

	// Acking again (nothing left to ack) is the original echo: 200, no
	// error, no second event, and the in-flight W5 survives untouched.
	wLive := s.epicWrapup(e.ID)
	require.NotNil(t, wLive.TaskID)
	_, err = svc.AckWrapUp(ctx, s.owner(), e.ID, "", "rq-ack-2")
	s.Require().NoError(err, "0 rows = original echo, never an error")
	require.Equal(t, 1, s.countEvents("wrapup_ack"), "no no-op event spam")
	require.Equal(t, *wLive.TaskID, *s.epicWrapup(e.ID).TaskID,
		"the echo never touched the live in-flight pointer")

	// --- The SECOND cap cycle: the ack-reset loop runs again, and the
	// arm must fire a SECOND wrapup_ack_required event. The arm event's
	// deterministic id is cycle-discriminated (the epic's total minted
	// wrap-up rows rides the id): every post-ack arm fires at
	// count==cap again, so the ledger value alone would collide and ON
	// CONFLICT DO NOTHING would silently swallow this cycle's audit row
	// + SSE event — the collision pin.
	for i := capN; i < 2*capN; i++ {
		w := s.liveWrapupTask(svc, e.ID)
		s.Require().Equal(board.StageInReview, w.Stage, "cycle %d: the epic's live wrap-up", i)
		g, err := svc.CreateTask(ctx, s.owner(), e.ID, "",
			board.TaskInput{Title: fmt.Sprintf("gap-%d", i), Description: fmt.Sprintf("Cycle %d's gap story: pin the delta the wrap-up flagged.", i),
				Tags:               []string{"gap"},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "fix", Met: true}}}, "")
		s.Require().NoError(err)
		s.checkWrapupCriteria(svc, w)
		_, _, err = svc.RecordWrapUpVerdict(ctx, s.reviewerActor(w), w.ID, "",
			board.WrapUpVerdictInput{Verdict: board.WrapUpVerdictGaps,
				ReportURL: fmt.Sprintf("https://ev.test/gaps-%d", i),
				GapRefs:   []string{g.ShortID}, Reviewer: "wrapup-reviewer"}, "")
		s.Require().NoError(err, "cycle %d: the gaps close", i)

		c, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
		s.Require().NoError(err)
		_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID,
			board.StageDone, fmt.Sprintf("https://ev.test/gap-%d-done", i), "")
		s.Require().NoError(err)
	}
	require.Equal(t, capN, s.epicWrapup(e.ID).Count, "cycle 2's ledger stands at the cap again")
	require.True(t, s.epicWrapup(e.ID).AckPending, "cycle 2 re-armed ack-wait")
	require.Equal(t, 2*capN, s.countWrapupRows(), "cycle 2 minted its own cap-1 wrap-ups plus the ack's immediate row")
	require.Equal(t, 2, s.countEvents("wrapup_ack_required"),
		"the SECOND arm cycle's event — the cycle-discriminated id, not swallowed by ON CONFLICT")

	// The second human ack lands its own event too (requestID-keyed ids
	// never collide) and the loop keeps going — the cap bounds, the
	// human decides, indefinitely.
	_, err = svc.AckWrapUp(ctx, s.owner(), e.ID, "", "rq-ack-3")
	s.Require().NoError(err)
	require.Equal(t, 2, s.countEvents("wrapup_ack"), "each real ack carries its own event")
	require.Equal(t, 0, s.epicWrapup(e.ID).Count, "cycle 2's ack reset the ledger again")
}

// TestWrapUp_AckClearsParkedManagerOverridePointer pins the §4 tail: a
// raw manager →done on a wrap-up row runs NO settle — the pointer parks
// (fail-visible) until the ack clears it; the acked drained epic then
// mints fresh.
func (s *BoardSuite) TestWrapUp_AckClearsParkedManagerOverridePointer() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	w := s.drainToMint(svc, ws, e, 1)
	s.managerCloseWrapup(svc, w)

	st := s.epicWrapup(e.ID)
	require.NotNil(t, st.TaskID, "the pointer parked (no verdict settle ran)")
	require.Equal(t, 0, st.Count, "the raw-done advanced nothing")
	require.Equal(t, board.EpicActive, s.epicStatus(e.ID), "never wedged: the epic waits, visibly")

	// Fail-visible: the parked pointer surfaces on the health read.
	health, err := svc.BoardHealth(ctx, testUser)
	s.Require().NoError(err)
	require.Len(t, health.AckWaits, 1, "the parked pointer rides the ack_waits surface")
	require.NotEmpty(t, health.AckWaits[0].InFlightShortID, "the parked row is named")

	// The ack clears the parked pointer and re-drains (fresh mint): the
	// pointer after ack names the FRESH wrap-up, never the parked row.
	_, err = svc.AckWrapUp(ctx, s.owner(), e.ID, "", "")
	s.Require().NoError(err)
	fresh := s.epicWrapup(e.ID).TaskID
	require.NotNil(t, fresh, "the acked, still-drained epic minted immediately")
	require.NotEqual(t, w.ID, *fresh, "the PARKED pointer was cleared — this is the fresh review")
	require.Equal(t, 2, s.countWrapupRows(), "the acked drain re-minted a fresh wrap-up")
}

// TestWrapUp_ReopenedEpicRecompleteFiresSecondCompletionEvent pins the
// epic_complete id's cycle discrimination: a completed epic REOPENED
// (EpicActivate completed→active, r13's exempt branch) and drained again
// completes through a LATER wrap-up row — and the second completion's
// audit event must LAND. The epic-id-only deterministic id would collide
// with the first completion's and ON CONFLICT DO NOTHING would silently
// swallow the audit row + SSE event — the collision pin (the id now
// carries the closing wrap-up row's task id).
func (s *BoardSuite) TestWrapUp_ReopenedEpicRecompleteFiresSecondCompletionEvent() {
	t := s.T()
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	svc := s.wrapSvcFaultable(nil)
	e := s.seedWrapEpic(svc, p, true)

	// First life: drain → W1 → PASS → completed (completion event 1).
	w1 := s.drainToMint(svc, ws, e, 1)
	_, epic := s.passVerdict(svc, w1, "rq-pass-reopen-1")
	require.Equal(t, board.EpicCompleted, epic.Status)
	require.Equal(t, 1, s.countEvents("wrapup_epic_complete"), "first completion's event")

	// The reopen (manager verb; the completed→active branch is exempt
	// from the activation re-check — stays active until work lands).
	reopened, err := svc.EpicActivate(ctx, s.owner(), e.ID, "", false, "rq-reopen-1")
	s.Require().NoError(err)
	require.Equal(t, board.EpicActive, reopened.Status, "the completed epic reopened")

	// Second life: follow-up work lands and drains → a FRESH wrap-up
	// (the ledger never reset — count=1 < cap) → PASS → completed again.
	s2 := s.seedTask(e, "followup", nil, true)
	s.claimAndComplete(svc, ws, s2.ShortID)
	w2 := s.liveWrapupTask(svc, e.ID)
	require.NotEqual(t, w1.ID, w2.ID, "the second completion rides a LATER wrap-up row")
	_, epic2 := s.passVerdict(svc, w2, "rq-pass-reopen-2")
	require.Equal(t, board.EpicCompleted, epic2.Status, "the reopened epic completed again")
	require.Equal(t, 2, s.countEvents("wrapup_epic_complete"),
		"the SECOND completion's event — the row-discriminated id, not swallowed by ON CONFLICT")
}

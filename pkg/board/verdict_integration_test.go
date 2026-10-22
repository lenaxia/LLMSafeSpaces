//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 item 5 (1/2) — RecordVerdict semantics against the real stage
// machine (PG harness; skips without TEST_DATABASE_URL):
//   - approve does NOT bypass the Done guard; the refusal is recorded
//     (applied=false) and the task stays in review;
//   - a fail verdict rides the bounce — fresh claim minted, attempts
//     UNCHANGED (the amended #1832 semantics: a bounce is an iteration
//     in the verdict chain, never a failure-ledger entry);
//   - the chain is ordered and readable (B2's envelope input).

func (s *BoardSuite) TestVerdict_1832_ApproveGuardHolds_RecordPersists() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "verdict-guard", nil, false) // criteria UNMET

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	_, err := s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	// Approve with unmet criteria + no evidence: recorded, refused, unchanged.
	out, v, err := s.svc.RecordVerdict(ctx, s.owner(), "", c.ShortID,
		board.VerdictInput{Verdict: board.VerdictApprove, Reviewer: "rev-1"}, "")
	s.Require().NoError(err, "a refused transition is an outcome, not an error")
	require.False(s.T(), v.Applied)
	require.Contains(s.T(), v.Transition, "refused:")
	require.Equal(s.T(), board.StageInReview, out.Stage, "the guard held")

	// Mark criteria met + supply evidence: approve lands Done.
	_, err = s.svc.UpdateTask(ctx, s.owner(), "", c.ShortID,
		board.TaskInput{AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}, {Text: "c2", Met: true}}}, "")
	s.Require().NoError(err)
	out, v, err = s.svc.RecordVerdict(ctx, s.owner(), "", c.ShortID,
		board.VerdictInput{Verdict: board.VerdictApprove, Reviewer: "rev-1", EvidenceURL: "https://example/review"}, "")
	s.Require().NoError(err)
	require.True(s.T(), v.Applied)
	require.Equal(s.T(), board.StageDone, out.Stage)

	chain, err := s.svc.TaskVerdicts(ctx, s.owner(), "", c.ShortID)
	s.Require().NoError(err)
	require.Len(s.T(), chain, 2, "both verdicts retained, ordered")
	require.False(s.T(), chain[0].Applied)
	require.True(s.T(), chain[1].Applied)
}

func (s *BoardSuite) TestVerdict_1832_FailBounceAttemptsUnchanged() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t := s.seedTask(e, "verdict-bounce", nil, false)

	c := s.claimOrFail(ws)
	require.Equal(s.T(), t.ID, c.TaskID)
	before := s.rawTask(t.ID)
	_, err := s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	out, v, err := s.svc.RecordVerdict(ctx, s.owner(), "", c.ShortID,
		board.VerdictInput{
			Verdict:  board.VerdictFail,
			Reviewer: "rev-2",
			Findings: []board.VerdictFinding{{Severity: board.SeverityBlocker, Pointer: "criterion 0: not verified"}},
		}, "")
	s.Require().NoError(err)
	require.True(s.T(), v.Applied)
	require.Equal(s.T(), board.StageInProgress, out.Stage)

	after := s.rawTask(t.ID)
	require.Equal(s.T(), before.Attempts, after.Attempts,
		"the amended #1832 semantics: a review bounce NEVER increments attempts — it is an iteration inside the verdict chain")
	require.NotEmpty(s.T(), after.ClaimToken, "the bounce re-arms a fresh claim")
	require.NotEqual(s.T(), c.ClaimToken, after.ClaimToken, "a NEW token, not the review-era one")

	chain, err := s.svc.TaskVerdicts(ctx, s.owner(), "", c.ShortID)
	s.Require().NoError(err)
	require.Len(s.T(), chain, 1)
	require.Equal(s.T(), board.VerdictFail, chain[0].Verdict)
	require.Len(s.T(), chain[0].Findings, 1)
}

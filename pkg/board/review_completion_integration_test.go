// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 lane WS1 — delegated review tasks auto-complete when their
// SUBJECT reaches done (the tsk-zbdeufiw incident: a review task parked
// in_review with ALL criteria met could not reach done once its subject
// merged — no agent-visible path existed). The fix is engine-side:
//
//   - the review↔subject linkage is an explicit TAG anchor
//     `review-of:<subject-short-id>` (the pr:N discipline: the only
//     form the engine consumes; malformed anchors are inert tags,
//     never guessed);
//   - every path that lands a task in done (manager move, approve
//     verdict via moveStageTx, RC2 drift_done) settles the subject's
//     review tasks: ALL criteria met → complete (audited transition +
//     review_complete event); criteria UNMET → NEVER done — the
//     in_review row parks pending_input/drift_review with a
//     review_park event (the owner-guard ruling: stranded-in_review
//     rots invisibly; the pending_input column is owner-visible).
//   - the completion is engine-actor audited: a transition row plus a
//     `review_complete` board event naming subject and evidence.

// completeSubjectFlow walks a task backlog→in_review so a done write is
// legal (criteria pre-met per seedTask(criteriaMet)).
func (s *BoardSuite) parkInReview(wsID string, t *board.Task) {
	c := s.claimOrFail(wsID)
	s.Require().Equal(t.ID, c.TaskID, "FIFO: seed order decides the claim")
	_, err := s.svc.MoveStage(context.Background(), s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
}

func (s *BoardSuite) TestReviewAuto_SubjectDoneCompletesReviewTask() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// Seed order = FIFO claim order: review first, subject second.
	review := s.seedTask(e, "review", []string{"review-of:PLACEHOLDER"}, true)
	subject := s.seedTask(e, "subject", nil, true)
	// The anchor addresses the subject by its minted short id.
	_, err := s.svc.UpdateTask(ctx, s.owner(), "", review.ShortID,
		board.TaskInput{Tags: []string{"review-of:" + subject.ShortID}}, "")
	s.Require().NoError(err)

	s.parkInReview(ws, review)
	c := s.claimOrFail(ws)
	s.Require().Equal(subject.ID, c.TaskID)
	_, err = s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	// Subject done (the manager move): the review task completes with it.
	_, err = s.svc.MoveStage(ctx, s.owner(), "", subject.ShortID, board.StageDone, "https://example/subject-pr", "")
	s.Require().NoError(err)

	done := s.reloadTask(review.ID)
	require.Equal(s.T(), board.StageDone, done.Stage,
		"auto-complete-on-subject-done: the engine completes the delegated review task")
	require.Equal(s.T(), 1, s.countTransitions(review.ID, board.StageDone), "an audited transition row")
	require.GreaterOrEqual(s.T(), s.countEvents("review_complete"), 1, "an engine audit event")
}

func (s *BoardSuite) TestReviewAuto_SubjectDoneByDriftCompletesReviewTask() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	// The RC2 path: subject anchored pr:42, PR merges externally.
	review := s.seedTask(e, "review", []string{"review-of:PLACEHOLDER"}, true)
	subject := s.seedTask(e, "subject", []string{"pr:42"}, true)
	_, err := s.svc.UpdateTask(ctx, s.owner(), "", review.ShortID,
		board.TaskInput{Tags: []string{"review-of:" + subject.ShortID}}, "")
	s.Require().NoError(err)

	s.parkInReview(ws, subject)
	// The incident's exact parked shape: the review task sits in
	// pending_input/drift_review (RC2 staged it when criteria were
	// unmet); its criteria are met NOW.
	_, err = s.pool.Exec(ctx,
		`UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='drift_review'
		 WHERE id=$1`, review.ID)
	s.Require().NoError(err)

	s.drift.set(42, board.PRState{Merged: true, URL: "https://example/pr/42"})
	res := s.sweep()
	require.Equal(s.T(), 1, res.DriftDone, "the subject completed by external truth")

	done := s.reloadTask(review.ID)
	require.Equal(s.T(), board.StageDone, done.Stage,
		"the drift_review-parked review task is rescued when its subject reaches done")
	require.Equal(s.T(), 1, s.countTransitions(review.ID, board.StageDone))
}

func (s *BoardSuite) TestReviewAuto_CriteriaUnmetParksForDecision() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	review := s.seedTask(e, "review-unmet", nil, false) // criteria UNMET
	subject := s.seedTask(e, "subject-unmet", nil, true)
	_, err := s.svc.UpdateTask(ctx, s.owner(), "", review.ShortID,
		board.TaskInput{Tags: []string{"review-of:" + subject.ShortID}}, "")
	s.Require().NoError(err)

	s.parkInReview(ws, review)
	c := s.claimOrFail(ws)
	s.Require().Equal(subject.ID, c.TaskID)
	_, err = s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(ctx, s.owner(), "", subject.ShortID, board.StageDone, "https://example/x", "")
	s.Require().NoError(err)

	parked := s.reloadTask(review.ID)
	require.Equal(s.T(), board.StagePendingInput, parked.Stage,
		"the owner-guard ruling: criteria-unmet review tasks NEVER auto-complete — they park owner-visibly")
	require.NotNil(s.T(), parked.PendingOrigin)
	require.Equal(s.T(), "drift_review", *parked.PendingOrigin)
	require.Equal(s.T(), 0, s.countEvents("review_complete"), "no completion ever fired")
	require.GreaterOrEqual(s.T(), s.countEvents("review_park"), 1, "the park is audited")
	require.Equal(s.T(), 1, s.countTransitions(review.ID, board.StagePendingInput))
}

func (s *BoardSuite) TestReviewAuto_MalformedAnchorsInert() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)

	review := s.seedTask(e, "review-malformed", []string{"review-of:", "review-of:tsk-", "reviews-of:tsk-nope"}, true)
	subject := s.seedTask(e, "subject-malformed", nil, true)

	s.parkInReview(ws, review)
	c := s.claimOrFail(ws)
	s.Require().Equal(subject.ID, c.TaskID)
	_, err := s.svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)
	_, err = s.svc.MoveStage(ctx, s.owner(), "", subject.ShortID, board.StageDone, "https://example/y", "")
	s.Require().NoError(err)

	require.Equal(s.T(), board.StageInReview, s.reloadTask(review.ID).Stage,
		"malformed anchors never match anything — inert tags, never guessed")
	require.Equal(s.T(), 0, s.countEvents("review_complete"))
}

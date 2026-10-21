// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// 1813_kill_test.go — the failed-state attribute-overlay pins (#1813):
// a task whose worker attempt FAILED (lease lapse → reclaim) carries a
// first-class VISIBLE failure overlay — last_attempt_failed +
// failure_count + last_failure_at — set by the ENGINE, never a stage
// (the five-stage vocabulary is binding post-#1763). Auto-return-to-
// ready rides the existing cooldown gate (backoff doubles per
// consecutive failure); any successful turn (in_review / done entry)
// resets the overlay; exhaustion escalation marks it too; the
// pending_input human exit clears it with the attempts reset.

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// KILL-TEST 1813-a: a lease-lapse reclaim marks the overlay and parks
// the row behind backoff — the failure is VISIBLE, not a silent
// redispatch to ordinary backlog.
func (s *BoardSuite) TestKill_1813_ReclaimMarksFailureOverlay() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "doomed", nil, false)

	c := s.claimOrFail(ws)
	s.Equal(t1.ID, c.TaskID)
	s.expireLease(t1.ID)

	res := s.sweep()
	s.GreaterOrEqual(res.Reclaims, 1, "the lapsed claim is reclaimed")

	after := s.reloadTask(t1.ID)
	s.Equal(board.StageBacklog, after.Stage, "reclaim returns the row to backlog (auto-return-to-ready)")
	s.True(after.LastAttemptFailed, "the overlay is set: last attempt failed")
	s.Equal(1, after.FailureCount, "one consecutive failure")
	s.Require().NotNil(after.LastFailureAt, "the failure timestamp is recorded")
	s.Require().NotNil(after.CooldownUntil, "backoff parks the row behind the cooldown gate")
	s.True(after.CooldownUntil.After(*after.LastFailureAt), "cooldown covers the failure moment")
}

// KILL-TEST 1813-b: backoff actually delays redispatch — the failed row
// is NOT claimable until the cooldown passes (then it is: return-to-
// ready is automatic, never a manager action).
func (s *BoardSuite) TestKill_1813_BackoffDelaysThenPermitsRedispatch() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "waiting", nil, false)

	s.claimOrFail(ws)
	s.expireLease(t1.ID)
	s.sweep()

	_, err := s.svc.ClaimNext(context.Background(), "w1", ws)
	s.ErrorIs(board.ErrNoCandidate, err, "the failed row waits out its backoff")

	s.clearCooldowns(p, t1.ID)
	c := s.claimOrFail(ws)
	s.Equal(t1.ID, c.TaskID, "after backoff the row re-enters dispatch automatically")
}

// KILL-TEST 1813-c: consecutive failures accumulate with doubling
// backoff, and a SUCCESSFUL turn (in_review entry) resets the overlay.
func (s *BoardSuite) TestKill_1813_ConsecutiveAccumulateAndResetOnSuccess() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "flappy", nil, false)

	// Cycle 1: fail once.
	s.claimOrFail(ws)
	s.expireLease(t1.ID)
	s.sweep()
	one := s.reloadTask(t1.ID)
	s.Equal(1, one.FailureCount)
	s.clearCooldowns(p, t1.ID)

	// Cycle 2: fail again — the count is CONSECUTIVE and the backoff
	// doubles (cooldown window strictly grows).
	c := s.claimOrFail(ws)
	s.expireLease(t1.ID)
	s.sweep()
	two := s.reloadTask(t1.ID)
	s.Equal(2, two.FailureCount, "consecutive failures accumulate")
	s.Require().NotNil(one.CooldownUntil)
	s.Require().NotNil(two.CooldownUntil)
	s.True(two.CooldownUntil.Sub(*two.LastFailureAt) > one.CooldownUntil.Sub(*one.LastFailureAt),
		"backoff doubles per consecutive failure")
	s.clearCooldowns(p, t1.ID)

	// Cycle 3: a SUCCESSFUL turn — worker drives to in_review — resets.
	c = s.claimOrFail(ws)
	_, err := s.svc.MoveStage(context.Background(), s.agent("sess-"+c.ShortID, c.ClaimToken), t1.ID, "", board.StageInReview, "", "")
	s.Require().NoError(err)
	done := s.reloadTask(t1.ID)
	s.False(done.LastAttemptFailed, "success clears the overlay")
	s.Equal(0, done.FailureCount)
}

// KILL-TEST 1813-d: exhaustion escalation (the attempts cap) marks the
// overlay on the pending_input row — the human sees WHY it escalated.
func (s *BoardSuite) TestKill_1813_EscalationMarksOverlay() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2) // cap-friendly
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "exhausted", nil, false)

	for i := 0; i < s.cfg.AttemptsCap; i++ {
		s.claimOrFail(ws)
		s.expireLease(t1.ID)
		s.sweep()
		s.clearCooldowns(p, t1.ID)
	}

	c, err := s.svc.ClaimNext(context.Background(), "w2", ws)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.True(c.Escalated, "at-cap claim escalates")

	after := s.reloadTask(t1.ID)
	s.Equal(board.StagePendingInput, after.Stage)
	s.True(after.LastAttemptFailed, "escalation carries the failure overlay")
	s.GreaterOrEqual(after.FailureCount, s.cfg.AttemptsCap, "the cap's failures are counted")
}

// KILL-TEST 1813-e: the pending_input human exit clears the overlay
// with the attempts reset — the re-claim starts clean.
func (s *BoardSuite) TestKill_1813_PendingResolutionClearsOverlay() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	t1 := s.seedTask(e, "resolved", nil, false)

	for i := 0; i < s.cfg.AttemptsCap; i++ {
		s.claimOrFail(ws)
		s.expireLease(t1.ID)
		s.sweep()
		s.clearCooldowns(p, t1.ID)
	}
	c, err := s.svc.ClaimNext(context.Background(), "w3", ws)
	s.Require().NoError(err)
	s.Require().True(c.Escalated)
	s.True(s.reloadTask(t1.ID).LastAttemptFailed, "precondition: escalated with overlay")

	// The human exit: pending_input → in_progress (the re-claim arm).
	_, err = s.svc.MoveStage(context.Background(), s.owner(), t1.ID, "", board.StageInProgress, "", "")
	s.Require().NoError(err)
	after := s.reloadTask(t1.ID)
	s.False(after.LastAttemptFailed, "the human exit clears the overlay with the attempts reset")
	s.Equal(0, after.FailureCount)
	s.Equal(0, after.Attempts, "attempts reset (the existing semantic)")
	require.True(s.T(), after.Stage == board.StageInProgress || after.Stage == board.StageBacklog)
}

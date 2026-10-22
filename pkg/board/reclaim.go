// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// reclaim.go — #1847 spec item 5: board_reclaim, the prior-claimant
// recovery path. A lapsed-then-alive claimant reclaims WITH CONTEXT:
// attempts unchanged (attempts counts context-loss events — a
// same-session reclaim lost nothing), and the #1813 failure overlay
// clears (the lapse's damage — the context loss — never materialized;
// this is the issue's treadmill-artifact-vs-real-failure distinction).
//
// Identity follows the claim token from the worker's charter envelope:
// the prior token's claim_assign history event (written at the
// spawn-stamp) names the context-holding session. No LLM-declared
// session id exists anywhere on the path — the #1847 trust rule
// (worker-declared identity is not input) holds here too.
//
// Guards: the prior token must appear in THIS task's assign history;
// no live claimant holds the row (backlog, or in_progress with a
// lapsed lease); pending_input is the human gate (its exits are
// manager-surface-only — reclaim cannot bypass it); terminal rows are
// immutable (RC5); paused rows refuse the mint (#1747, the bounce's
// belt). The mint re-stamps the historical session as assignee, clears
// the appeal/lapse columns, and writes a fresh claim_assign so a
// SECOND lapse can reclaim the same way.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReclaimTask is board_reclaim: the prior claimant's recovery mint.
// priorClaimToken is the token from the worker's charter envelope (the
// claim it holds context for — live or lapsed).
func (s *Service) ReclaimTask(ctx context.Context, actor Actor, taskID, shortIDOrID, priorClaimToken, requestID string) (*Claim, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	// The human gate first (§4.1: pending_input exits are human
	// decisions — a reclaim cannot route around the question).
	if t.Stage == StagePendingInput {
		return nil, &GuardError{Code: GuardManagerSurface, Detail: "pending_input is a human decision — resolve it on a manager surface; board_reclaim cannot bypass it"}
	}
	// RC5: terminal rows are immutable; in_review belongs to reviewers
	// (the bounce already re-claims for the recorded assignee).
	if t.Stage == StageDone || t.Stage == StageInReview {
		return nil, &GuardError{Code: GuardStage, Detail: "board_reclaim targets a backlog or lapsed in_progress row (got " + t.Stage + ")"}
	}
	// A live claim blocks: an unexpired lease means someone holds it.
	if t.Stage == StageInProgress && t.LeaseExpiresAt != nil && t.LeaseExpiresAt.After(time.Now().UTC()) {
		return nil, &GuardError{Code: GuardClaimToken, Detail: "the task holds a live claim — board_reclaim applies only after the lease lapsed (backlog or lapsed in_progress)"}
	}
	// The claim-history check: the prior token names the context-holding
	// session on THIS task's assign history.
	var session string
	err = s.q().QueryRow(ctx, `
		SELECT payload->>'session' FROM board_events
		WHERE item_kind='task' AND item_id=$1 AND action='claim_assign' AND payload->>'token'=$2
		ORDER BY created_at DESC LIMIT 1`, t.ID, priorClaimToken).Scan(&session)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, &GuardError{Code: GuardClaimToken, Detail: "the prior claim token is not in this task's assign history — board_reclaim is for prior claimants only"}
		}
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	token := uuidNew()
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "reclaim", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		// Effective pause (task/epic/project) — the bounce's belt.
		if paused, perr := s.effectivePausedTx(ctx, tx, t); perr != nil {
			return nil, perr
		} else if paused {
			return nil, &GuardError{Code: GuardPaused, Detail: "the task is paused — board_reclaim refuses until resume (resume the row first)"}
		}
		// The mint: fresh token+lease, the HISTORICAL session re-stamped,
		// attempts UNCHANGED (claim_incremented_attempts=false — a 429 on
		// this claim must not decrement what was never incremented), the
		// appeal/lapse columns cleared, and the #1813 overlay cleared (the
		// recovery repaired the lapse's damage).
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='in_progress', stage_entered_at=now(),
				claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8),
				last_progress_at=now(), assignee_session=$4,
				claim_incremented_attempts=false, pending_origin=NULL,
				cooldown_until=NULL, appeal_until=NULL, lapse_reason=NULL,
				failure_count=0, last_attempt_failed=false, last_failure_at=NULL
			WHERE id=$1 AND stage IN ('backlog','in_progress')
			  AND (lease_expires_at IS NULL OR lease_expires_at < now())
			  AND NOT paused`,
			t.ID, token, s.cfg.LeaseDuration.Seconds(), session)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardClaimToken, Detail: "the row moved or holds a live claim — server-arbitrated refusal"}
		}
		if err := writeTransition(ctx, txQ{tx}, t.ID, t.Stage, StageInProgress, "", actor); err != nil {
			return nil, err
		}
		// The fresh episode's claim history: a second lapse reclaims the
		// same way (with the FRESH charter token).
		if _, err := recordEventTx(ctx, tx, p.ID,
			"engine:assign:"+t.ID+":"+token, "claim_assign", "task", t.ID,
			Actor{Kind: ActorSystem, Surface: "engine"},
			map[string]any{"task_id": t.ID, "session": session, "token": token}); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": t.ID, "reclaimed_by": "prior_claimant",
			"session": session, "attempts_unchanged": true}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	out := s.mustTask(ctx, t.ID)
	if out == nil {
		return nil, &NotFoundError{Item: "task", Identifier: t.ID}
	}
	return &Claim{
		TaskID: t.ID, ShortID: out.ShortID, ProjectID: t.ProjectID, EpicID: t.EpicID,
		WorkspaceID: s.projectWorkspace(ctx, t.ProjectID),
		ClaimToken:  token, Assignee: session, Task: out,
	}, nil
}

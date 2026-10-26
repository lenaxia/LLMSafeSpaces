// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// rearm.go — #1832 WS3: the attempts-exhaustion re-pool path.
// Escalation (pre-increment attempts >= cap, both the sweep's claimOnce
// and TaskStart) parks a row at pending_input/escalated and the engine
// NEVER grants it again — before this action there was no path back.
// Re-arm is the explicit, audited manager judgment that the task
// deserves another round: attempts reset to 0 (the human-exit
// semantic — a fresh judgment), the PRE-RESET count preserved in the
// rearm event so the history survives, the failure overlay cleared
// (a re-arm is not a failure), and the row back in the claimable pool.

// RearmTask re-pools an attempts-exhausted (escalated) pending_input
// row: → backlog, attempts=0, overlay + cooldown cleared, one audited
// `rearm` event (idempotent per requestId). Only escalated rows — the
// other pending_input origins keep their enumerated exits.
func (s *Service) RearmTask(ctx context.Context, actor Actor, taskID, shortIDOrID, requestID string) (*Task, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	_, _, err = (&eventWriter{svc: s, projectID: p.ID, requestID: requestID,
		action: "rearm", itemKind: "task", itemID: t.ID, actor: actor}).run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		cur, err := s.store.taskForUpdate(ctx, tx, t.ID)
		if err != nil {
			return nil, err
		}
		if cur.Stage != StagePendingInput {
			return nil, &GuardError{Code: GuardStage, Detail: "re-arm targets attempts-exhausted rows (pending_input/escalated); other stages keep their enumerated transitions"}
		}
		if cur.PendingOrigin == nil || *cur.PendingOrigin != PendingOriginEscalated {
			return nil, &GuardError{Code: GuardStage, Detail: "re-arm targets attempts-exhausted rows (pending_input/escalated); worker-origin and drift-review rows keep their enumerated exits"}
		}
		pre := cur.Attempts
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='backlog', stage_entered_at=now(),
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
				claim_incremented_attempts=false, claim_persona=NULL, pending_origin=NULL,
				attempts=0, failure_count=0, last_attempt_failed=false, last_failure_at=NULL,
				cooldown_until=NULL
			WHERE id=$1 AND stage='pending_input' AND pending_origin='escalated'`, cur.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardStage, Detail: "row moved — server-arbitrated refusal"}
		}
		if err := writeTransition(ctx, txQ{tx}, cur.ID, StagePendingInput, StageBacklog, "", actor); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": cur.ID, "attempts_before": pre, "attempts_after": 0, "stage": StageBacklog}, nil
	})
	if err != nil {
		return nil, err
	}
	// The re-pooled row is claimable: feed the dispatch drive.
	s.pokeDrive(p.ID)
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	// #1922 (review round 2, finding 2): the tail reload renders the
	// row's CURRENT state — a replay after a re-claim+stamp (or a fast
	// dispatch drive winning the pokeDrive race) carries a live
	// assignee, so the echo translates at the read edge like every
	// other task-bearing response.
	s.translateTaskReads(ctx, out)
	return out, nil
}

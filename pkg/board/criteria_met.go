// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// criteria_met.go — the #1946 scoped grant (owner ruling 2026-10-25:
// "Why is set criteria met manager only. It should not be.").
//
// Ruling #1747 §5's blanket surface separation (ALL manager board tools
// denied to spawned worker sessions) plus the anti-self-certification
// rule for TASK workers caught the reviewer persona in the same net:
// the auto-review reviewer (the #1941 stage-persona claim-then-spawn
// under the #1939 flipped defaults) can VERIFY acceptance criteria but
// could never RECORD them met — setCriteriaMet was manager-surface-only,
// so the done guard (R2) refused the reviewer's →done and a
// user-collaborator had to hand-set the flags. Criteria verification IS
// the reviewer's job; the loop must self-complete.
//
// The grant, exactly as ruled — narrow on all three axes:
//   - WHO: only a REVIEWER-PERSONA claim. The #1941 mint's provenance,
//     made durable on the row (claim_persona, migration 000050), is the
//     seam: it is non-NULL only while a persona episode's token holds
//     the row, and in_review tokens are minted by NOTHING else. A task
//     worker's claim (worker grant, bounce re-claim, exit re-claim)
//     never carries it — self-certification stays refused with the
//     #1747 §5 error.
//   - WHAT: only the per-criterion met flag (setCriteriaMet's one
//     mutation — no title/description/tags/criteria-replace reach this
//     method; the surrounding seam refuses them loudly).
//   - WHERE: only the caller's OWN claimed row — the live claim token
//     is re-arbitrated against the locked row, same as every in-flight
//     worker write.
//
// Every flag write is audited per-criterion (action criteria_met: index,
// the written flag, the criterion text, and the persona whose claim
// made the grant) — the provenance the auto-review trail lacked.

// SetCriteriaMet records ONE criterion's met flag on the caller's own
// claimed row (#1946). The caller is the reviewer persona holding the
// row's live persona claim (claim token from its charter envelope);
// anything else — an implementer's in_progress claim, a stale or lapsed
// token, an unclaimed row — refuses. met may be written BOTH ways (the
// reviewer's judgment, #1837 P2b's walk-both-ways semantics).
func (s *Service) SetCriteriaMet(ctx context.Context, actor Actor, taskID, shortIDOrID string, check CriteriaCheck, requestID string) (*Task, error) {
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
	// Grant guard, tier 1 — the persona seam. A row that is not held
	// by a reviewer-persona claim has NO grant on this surface: the
	// caller falls back to the #1747 §5 rule verbatim (manager board
	// tools, user-collaborator sessions only). This is the error the
	// task worker self-certifying its own work keeps seeing.
	if t.Stage != StageInReview || t.ClaimPersona == "" {
		return nil, &GuardError{Code: GuardManagerSurface, Detail: "criteria-met recording is the reviewer-persona claim's scoped grant (issue #1946) — manager board tools attach to user-collaborator sessions only (ruling #1747 §5), and task-worker self-certification remains forbidden"}
	}
	// Grant guard, tier 2 — the live token (the charter envelope's
	// claim token; an empty or stale token never matches).
	if actor.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
		return nil, &GuardError{Code: GuardClaimToken, Detail: "criteria-met writes require the row's live persona-claim token (the in-flight guard from your charter envelope)"}
	}
	if err := validateCriteriaUpdate(TaskInput{SetCriteriaMet: &check}, t.AcceptanceCriteria); err != nil {
		return nil, err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "criteria_met", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		// Server arbitration under the row lock: the claim may have
		// lapsed, the row bounced, or the persona cleared since the
		// read above — the guards re-run against the fresh row.
		cur, err := s.store.taskForUpdate(ctx, tx, t.ID)
		if err != nil {
			return nil, err
		}
		if cur.Stage != StageInReview || cur.ClaimPersona == "" || actor.ClaimToken != cur.ClaimToken {
			return nil, &GuardError{Code: GuardClaimToken, Detail: "the claim lapsed or moved — server-arbitrated refusal"}
		}
		// #1747 pause-must-pause: a paused row halts its workers — the
		// persona claim refuses on pause exactly like its mint does.
		if paused, perr := s.effectivePausedTx(ctx, tx, cur); perr != nil {
			return nil, perr
		} else if paused {
			return nil, &GuardError{Code: GuardPaused, Detail: "the task is paused — persona criteria writes refuse until resume"}
		}
		if err := validateCriteriaUpdate(TaskInput{SetCriteriaMet: &check}, cur.AcceptanceCriteria); err != nil {
			return nil, err
		}
		cur.AcceptanceCriteria[check.Index].Met = check.Met
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET acceptance_criteria=$2
			WHERE id=$1 AND stage='in_review' AND claim_token=$3 AND claim_persona IS NOT NULL`,
			t.ID, criteriaJSON(cur.AcceptanceCriteria), actor.ClaimToken)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardClaimToken, Detail: "the claim lapsed or moved — server-arbitrated refusal"}
		}
		return map[string]any{
			"task_id": t.ID, "index": check.Index, "met": check.Met,
			"criterion": cur.AcceptanceCriteria[check.Index].Text,
			"persona":   cur.ClaimPersona,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup // 200-with-original: the reloaded row IS the original outcome
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	// #1922: the write echo rides the read-edge phrase translation.
	s.translateTaskReads(ctx, out)
	return out, nil
}

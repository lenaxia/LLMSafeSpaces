// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// review.go — #1832 WS1: delegated-review completion coupling (the
// tsk-zbdeufiw incident). A task that exists to review another task's
// artifact is anchored to its SUBJECT with an explicit tag
// `review-of:<subject-short-id>` (the pr:N discipline: the only form
// the engine consumes; malformed anchors are inert tags, never
// guessed). When the subject reaches done — by the manager move, an
// approve verdict, or RC2 external-truth drift — the engine settles
// the subject's review tasks in the SAME transaction:
//
//   - criteria ALL met → complete (the Done guard is never bypassed;
//     an empty criteria set is vacuously met, the guard's own rule);
//     eligible rows: in_review, or pending_input parked by RC2's
//     drift_review (the incident's exact shape: the review task was
//     staged when its criteria were unmet, then stranded when the
//     subject merged);
//   - criteria UNMET → NEVER done: the in_review row parks to
//     pending_input/drift_review with an audit event (the owner-guard
//     ruling — stranded-in_review rots invisibly; the pending_input
//     column is the owner-visible surface, and the manager exit
//     (WS2) makes it serviceable);
//   - every settle is engine-actor audited: a transition row plus a
//     deterministic review_complete / review_park event carrying
//     subject + evidence;
//   - a completed review task may itself be a subject (chained
//     delegation) — the walk recurses over a visited set, which bounds
//     it: every completion lands a row in done, and done rows never
//     re-enter candidacy (RC5). Parks do not recurse (a parked row is
//     not done — nothing downstream settles).

// reviewEvidenceFallback synthesizes an evidence pointer when the
// subject's own transition carried none (the Done guard's evidence
// half must hold for engine completions too).
func reviewEvidenceFallback(subject *Task) string {
	return "board://review/" + subject.ShortID
}

// completeReviewTasks settles the anchored review tasks of a task that
// just reached done, inside the caller's transaction. It returns the
// post-commit publishable events for the settles plus the project ids
// the settle touched (sibling projects of the subject — the dispatch
// drive feed).
func (s *Service) completeReviewTasks(ctx context.Context, tx pgx.Tx, subject *Task, evidence string, visited map[string]bool) ([]Event, []string, error) {
	if visited == nil {
		visited = map[string]bool{}
	}
	visited[subject.ID] = true
	if evidence == "" {
		evidence = reviewEvidenceFallback(subject)
	}
	candidates, err := s.reviewCandidates(ctx, tx, subject)
	if err != nil {
		return nil, nil, err
	}
	sys := Actor{Kind: ActorSystem, Surface: "engine"}
	var out []Event
	var pokes []string
	for _, cand := range candidates {
		if visited[cand.ID] {
			continue
		}
		if criteriaAllMet(cand) {
			evt, err := s.completeReviewTask(ctx, tx, cand, subject, evidence, sys)
			if err != nil {
				return out, pokes, err
			}
			if evt != nil {
				out = append(out, *evt)
				pokes = append(pokes, cand.ProjectID)
				visited[cand.ID] = true
				// Chained delegation: the completed review may anchor
				// its own reviewers. The visited set + the done
				// predicate bound the walk.
				more, morePokes, err := s.completeReviewTasks(ctx, tx, cand, evidence, visited)
				if err != nil {
					return out, pokes, err
				}
				out = append(out, more...)
				pokes = append(pokes, morePokes...)
			}
		} else {
			evt, err := s.parkReviewTask(ctx, tx, cand, subject, evidence, sys)
			if err != nil {
				return out, pokes, err
			}
			if evt != nil {
				out = append(out, *evt)
			}
		}
	}
	return out, pokes, nil
}

// completeReviewTask lands one eligible review task in done (guarded
// UPDATE; a raced row converged by its own writer) with transition +
// deterministic event, then the done-cascade effects (reverse-edge
// unblock + epic completion). Returns nil when the guard lost the row.
func (s *Service) completeReviewTask(ctx context.Context, tx pgx.Tx, cand, subject *Task, evidence string, sys Actor) (*Event, error) {
	from := cand.Stage
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='done', stage_entered_at=now(),
			claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false,
			failure_count=0, last_attempt_failed=false, last_failure_at=NULL, pending_origin=NULL
		WHERE id=$1 AND NOT paused AND (stage='in_review' OR (stage='pending_input' AND pending_origin='drift_review'))`,
		cand.ID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil // raced a concurrent writer: converged by them
	}
	if err := writeTransition(ctx, txQ{tx}, cand.ID, from, StageDone, evidence, sys); err != nil {
		return nil, err
	}
	evt, err := recordEventTx(ctx, tx, cand.ProjectID,
		"engine:review_complete:"+cand.ID+":"+subject.ID, "review_complete",
		"task", cand.ID, sys, map[string]any{
			"task_id": cand.ID, "subject_task_id": subject.ID,
			"subject_short_id": subject.ShortID, "from_stage": from, "evidence": evidence,
		})
	if err != nil {
		return nil, err
	}
	if _, err := unblockDependents(ctx, txQ{tx}, cand.ID, true); err != nil {
		return nil, err
	}
	if _, err := epicCompletionCheck(ctx, tx, cand.EpicID); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: review task completed with its subject",
		"task", cand.ShortID, "subject", subject.ShortID)
	if evt.ID == "" {
		return nil, nil // replayed deterministic id: inert, nothing to publish
	}
	return &evt, nil
}

// parkReviewTask is the owner-guard branch: an in_review review task
// whose criteria are UNMET when the subject completes parks to
// pending_input/drift_review (RC2's stranded-decision shape — the
// pending_input column is the owner-visible surface) with transition +
// deterministic review_park event. Already-parked rows are no-ops.
func (s *Service) parkReviewTask(ctx context.Context, tx pgx.Tx, cand, subject *Task, evidence string, sys Actor) (*Event, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='drift_review'
		WHERE id=$1 AND NOT paused AND stage='in_review'`, cand.ID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	if err := writeTransition(ctx, txQ{tx}, cand.ID, StageInReview, StagePendingInput, evidence, sys); err != nil {
		return nil, err
	}
	evt, err := recordEventTx(ctx, tx, cand.ProjectID,
		"engine:review_park:"+cand.ID+":"+subject.ID, "review_park",
		"task", cand.ID, sys, map[string]any{
			"task_id": cand.ID, "subject_task_id": subject.ID,
			"subject_short_id": subject.ShortID, "evidence": evidence,
			"reason": "subject reached done with review criteria unmet — owner decision required",
		})
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: review task parked for decision (criteria unmet at subject-done)",
		"task", cand.ShortID, "subject", subject.ShortID)
	if evt.ID == "" {
		return nil, nil
	}
	return &evt, nil
}

// reviewCandidates loads the subject's anchored review tasks that are
// structurally eligible for the settle walk: same workspace (the
// dependency-edge scoping rule — a foreign tenant's identically-tagged
// row is never touched; IS NOT DISTINCT FROM so two workspace-less
// projects still settle), non-terminal review stages, and not
// effectively paused (#1747: the engine does not force forward writes
// through the pause lever). FOR UPDATE locks each candidate row
// THROUGH the settle's branch decision (review r1, finding 1: the
// criteria verdict must read the locked row — a racing UpdateTask
// rewriting the criteria array blocks on the lock, never lands
// done-with-unmet-criteria through this path). Short-id anchors are
// per-project unique, so two same-named tasks in one workspace's
// projects both anchor — the UUID anchor form disambiguates.
func (s *Service) reviewCandidates(ctx context.Context, tx pgx.Tx, subject *Task) ([]*Task, error) {
	anchors := []string{"review-of:" + subject.ShortID, "review-of:" + subject.ID}
	rows, err := tx.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks t
		JOIN projects p ON p.id = t.project_id
		JOIN epics e ON e.id = t.epic_id
		WHERE t.tags && $1::text[]
		  AND (t.stage = 'in_review' OR (t.stage = 'pending_input' AND t.pending_origin = 'drift_review'))
		  AND NOT t.paused AND e.status <> 'paused' AND NOT p.paused
		  AND p.target_workspace_id IS NOT DISTINCT FROM (SELECT p2.target_workspace_id FROM projects p2 WHERE p2.id = $2)
		FOR UPDATE OF t`,
		anchors, subject.ProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("board: review candidate scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

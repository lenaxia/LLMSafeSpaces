// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// depose.go — S5, the DEPOSED directive: first-class salvage handoff
// for lapsed attempts (2026-10-30 incident remediation, epic "Liveness
// and observation"). When an attempt lapses holding pushed work,
// recovery used to be ad-hoc — the orchestrator read transcripts and
// hand-wrote a salvage note to the next worker (the glassy-shovel
// incident cost 4 worker sessions and 3 claims: one worker pivoted to
// salvage on its own initiative, another re-did pushed work).
//
// Depose is the manager lever in the HALT/RESUME family:
//
//   - The orchestrator (manager route — owner/agent-manager guards, the
//     taskProjectForActor house pattern) deposes a LAPSED claim.
//   - The row records the salvage pointer — branch, PR, last known
//     state (latest-only slot; the full trail is the audited 'depose'
//     events).
//   - The next claim's charter embeds the pointer automatically
//     (engine.go spawnAfter/spawnFor → boardCharterPrompt: inherit,
//     don't restart).
//   - The stale session receives the DEPOSED directive best-effort
//     post-commit (turn-boundary, like HALT — never an abort): a
//     still-running deposed worker learns its claim is gone and stops
//     writing (its next token-carrying write 409s regardless).
//
// GUARDS (pinned by depose_integration_test.go):
//
//   - A LIVE claim refuses deposition (GuardClaimLive): an unexpired
//     lease is flat refusal, and an expired lease with LIFE evidence
//     (observation-seam movement/busy/post-sentinel reaction, or a
//     heartbeat after the latest nudge) refuses too — a worker the
//     ladder is about to renew must never be deposed under it. The
//     write-side predicate re-arbitrates at UPDATE time in the DB
//     clock, so a renewal racing the check converges refused, never
//     deposed. This lever is only as trustworthy as the stamp/lapse
//     evidence underneath it (S1, stamp hardening) — which is why the
//     story depends on S1.
//   - Deposing is AUDIT-LOGGED: one 'depose' board event per call
//     (payload: the pointer, the deposed session, the from-stage),
//     requestId-idempotent like every eventWriter action.
//
// WHAT DEPOSE DOES TO THE ROW: an in_progress lapsed claim is requeued
// exactly as the rc1 reclaim would (failure overlay, consecutive-failure
// backoff cooldown, lease-episode slots cleared) plus the salvage
// pointer — depose ACCELERATES the reclaim the sweep would perform,
// with the handoff attached. A backlog row (the reclaim already ran —
// the incident's actual shape: the orchestrator notices post-reclaim)
// takes the pointer alone: no stage change, no second failure count,
// one audited event.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Salvage field caps (bytes; the textguard house discipline — free
// manager text rides the row and the charter, so it is size-capped and
// corruption-scanned like description/outcome). Branch/PR/Head are
// short identifiers; State/Note are the hand-written-note class.
const (
	MaxSalvageRefBytes   = 512
	MaxSalvageStateBytes = 32 << 10
)

// validateSalvagePointer enforces presence + caps + corruption scan on
// the depose payload (an all-empty pointer is GuardInput — the audit
// event must record SOMETHING for the handoff to exist).
func validateSalvagePointer(p SalvagePointer) error {
	if p.Empty() {
		return &GuardError{Code: GuardInput, Detail: "the salvage pointer is empty — record at least a branch, PR, or last-known state"}
	}
	for field, s := range map[string]string{
		"salvage.branch": p.Branch,
		"salvage.pr":     p.PR,
		"salvage.head":   p.Head,
	} {
		if err := validateTextField(field, s, MaxSalvageRefBytes); err != nil {
			return err
		}
	}
	if err := validateTextField("salvage.state", p.State, MaxSalvageStateBytes); err != nil {
		return err
	}
	return validateTextField("salvage.note", p.Note, MaxSalvageStateBytes)
}

// DeposeTask deposes a LAPSED claim, recording the salvage pointer on
// the row (manager route). See the file comment for the full contract.
func (s *Service) DeposeTask(ctx context.Context, actor Actor, taskID, shortIDOrID string, ptr SalvagePointer, requestID string) (*Task, error) {
	t, p, err := s.taskProjectForActor(ctx, actor, taskID, shortIDOrID)
	if err != nil {
		return nil, err
	}
	if err := validateSalvagePointer(ptr); err != nil {
		return nil, err
	}
	ptrJSON, err := json.Marshal(ptr)
	if err != nil {
		return nil, err
	}

	// deposedSession carries the pre-write assignee out of the
	// transaction for the post-commit DEPOSED directive (the UPDATE
	// nulls the row's copy).
	var deposedSession string
	var requeued bool

	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID,
		action: "depose", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		cur, err := s.store.taskForUpdate(ctx, tx, t.ID)
		if err != nil {
			return nil, err
		}
		deposedSession = cur.AssigneeSession
		switch cur.Stage {
		case StageDone:
			return nil, &GuardError{Code: GuardDone, Detail: "done is immutable — nothing to depose"}
		case StageFailed:
			return nil, &GuardError{Code: GuardStage, Detail: "failed rows take re-arm, not depose (attempts-exhaustion is the rearm lane)"}
		case StageInReview:
			return nil, &GuardError{Code: GuardStage, Detail: "depose targets the worker lane — a lapsed in_progress claim or a reclaimed backlog row; review-lapse recovery is the persona pass"}
		case StageInProgress:
			if cur.PendingInput {
				return nil, &GuardError{Code: GuardStage, Detail: "a pending_input-flagged row suspends the claim lifecycle — its exits are the pending_input transitions, not depose"}
			}
			// GUARD 1 — the live lease: an unexpired lease is flat
			// refusal. Go-clock read for the good error; the UPDATE's
			// predicate re-arbitrates in the DB clock below (a renewal
			// racing this check converges refused).
			if cur.LeaseExpiresAt != nil && cur.LeaseExpiresAt.After(time.Now().UTC()) {
				return nil, &GuardError{Code: GuardClaimLive, Detail: "a live claim refuses deposition — the lease has not lapsed (renewal/heartbeat may still land)"}
			}
			// GUARD 2 — life evidence on the expired lease: the
			// seamless-lease ladder's own alive signals, consulted
			// under the row lock (the rc1 pattern). A worker the next
			// sweep would RENEW (movement, mid-turn busy, post-sentinel
			// reaction, or a heartbeat after the latest nudge) must
			// never be deposed under. No-evidence (Nil source, degraded
			// adapter, no assignee) degrades honestly to the wall-clock
			// lease — the expired lease IS the lapse evidence (S1).
			if cur.AssigneeSession != "" {
				if s.deposeShowsLife(ctx, tx, cur) {
					return nil, &GuardError{Code: GuardClaimLive, Detail: "the expired claim shows life (movement, un-exhausted mid-turn busy, post-sentinel reaction, or a post-nudge heartbeat) — the ladder would renew it; deposition refused"}
				}
			}
			// The requeue: rc1's reclaim statement VERBATIM (same
			// columns, same overlay: failure counted consecutively,
			// backoff cooldown, episode slots cleared) plus the salvage
			// pointer — depose accelerates the reclaim the sweep would
			// perform. attempts stay untouched (the increment belongs
			// to the next claim).
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET stage='backlog', stage_entered_at=now(),
					claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
					claim_incremented_attempts=false, claim_persona=NULL,
					nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
					last_attempt_failed=true, failure_count=failure_count+1, last_failure_at=now(),
					cooldown_until=now()+make_interval(secs=>$2::float8),
					salvage=$3
				WHERE id=$1 AND stage='in_progress' AND NOT pending_input
				  AND (lease_expires_at IS NULL OR lease_expires_at < now())`,
				cur.ID, s.cfg.failureBackoff(cur.FailureCount+1).Seconds(), ptrJSON)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 0 {
				// The claim went live again (a renewal committed under
				// us) or the row moved: converged by the winner — the
				// refusal is the safe direction.
				return nil, &GuardError{Code: GuardClaimLive, Detail: "row moved — the claim re-armed (renewal or reclaim raced); server-arbitrated refusal"}
			}
			if err := writeTransition(ctx, txQ{tx}, cur.ID, StageInProgress, StageBacklog, "", actor); err != nil {
				return nil, err
			}
			requeued = true
		case StageBacklog:
			// The reclaim already ran (the incident's shape: the
			// orchestrator notices post-reclaim): record the pointer
			// alone — no stage change, no second failure count.
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET salvage=$2, updated_at=now()
				WHERE id=$1 AND stage='backlog'`, cur.ID, ptrJSON)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 0 {
				return nil, &GuardError{Code: GuardStage, Detail: "row moved — server-arbitrated refusal"}
			}
		default:
			// Defensive: the five-stage vocabulary is DB-CHECK-pinned,
			// but a future sixth stage must fall through to a LOUD
			// refusal here, never a no-op success payload (review r1's
			// latent fail-open finding).
			return nil, &GuardError{Code: GuardStage, Detail: "depose has no enumerated arm for stage " + cur.Stage + " — refusing loud"}
		}
		return map[string]any{
			"task_id":         cur.ID,
			"salvage":         ptr,
			"deposed_session": deposedSession,
			"from_stage":      cur.Stage,
			"requeued":        requeued,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	// S7: the deposition lands on the observation trail (post-commit,
	// fail-open — recordObservation's contract). A replayed requestId
	// (dup) skips the row: the original run already recorded it, and a
	// replay is not a second deposition. The salvage pointer rides the
	// detail — the trail's row is the pointer WITH its context (which
	// session, from which stage, requeued or pointer-alone).
	if dup == nil {
		s.recordObservation(ctx, ObservationEvent{
			Kind: ObservationDeposed, TaskID: t.ID, Session: deposedSession,
			Detail: map[string]any{
				"salvage": ptr, "from_stage": t.Stage, "requeued": requeued,
			},
		})
	}
	_ = dup
	if requeued {
		// The DEPOSED directive to the stale session: best-effort,
		// post-commit (the HALT/RESUME transport precedent — the
		// turn-boundary message, never an abort). The row no longer
		// names the assignee, so the captured pre-write identity is the
		// only address; a lost directive self-heals (the deposed
		// worker's next token-carrying write 409s and it re-reads).
		s.deliverDeposedDirective(ctx, deposedSession, t.ShortID)
		// The requeued row is claimable now (subject to its cooldown):
		// feed the dispatch drive.
		s.pokeDrive(p.ID)
	}
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, out, p)
	s.translateTaskReads(ctx, out)
	return out, nil
}

// deposeShowsLife answers GUARD 2: does the expired claim show any
// alive signal the seamless-lease ladder would renew on? Read under the
// row lock (the rc1 consult pattern): the row's own heartbeat-vs-nudge
// ordinal first (server-clock, one domain), then the observation seam's
// movement/busy/post-sentinel facts. Busy is honored under the SAME
// cap the ladder enforces (boardBusyRenewCap): past cap consecutive
// busy-only renewals the ladder treats Busy as EXHAUSTED and proceeds
// toward the wedge verdict — depose must not refuse on the very truth
// the ladder itself has stopped believing (review r1's lockout
// finding: the stuck busy-true zombie row would become UNDEPOSABLE,
// the exact class this directive exists to salvage).
func (s *Service) deposeShowsLife(ctx context.Context, q Queryer, cur *Task) bool {
	var nudgeID *string
	var nudgedAt, lastHeartbeat *time.Time
	var busyRenews int32
	if err := q.QueryRow(ctx,
		`SELECT nudge_id, nudged_at, last_heartbeat_at, busy_renews FROM tasks WHERE id=$1`, cur.ID).
		Scan(&nudgeID, &nudgedAt, &lastHeartbeat, &busyRenews); err != nil {
		// An unreadable row is the claimStillLive lesson (b1a73e79c):
		// the unanswerable question must not kill — treat as no
		// EXTRA life evidence and let the write-side predicate arbitrate.
		slog.WarnContext(ctx, "board: depose life-evidence read failed (deferring to the write predicate)",
			"taskId", cur.ID, "error", err.Error())
	}
	if nudgedAt != nil && lastHeartbeat != nil && lastHeartbeat.After(*nudgedAt) {
		return true // the worker answered the sentinel on the response channel
	}
	lives := s.lives
	if lives == nil {
		return false // no observation seam: the wall-clock lease is the evidence
	}
	res := lives.ObserveAssignees(ctx, SessionObservations{Refs: []AssigneeRef{{SessionID: cur.AssigneeSession, NudgeID: deref(nudgeID)}}})
	o, ok := res[cur.AssigneeSession]
	if !ok || !o.Observed {
		return false // no-evidence — never death, and never a refusal either
	}
	busyHonored := o.Busy && busyRenews < boardBusyRenewCap
	return o.MovedSinceLastSweep || busyHonored || o.EventsAfterNudge
}

// deliverDeposedDirective sends the DEPOSED directive to the deposed
// session over the messenger seam (fire-and-forget, like
// deliverToWorker — but address-direct: the row no longer carries the
// assignee after the requeue).
func (s *Service) deliverDeposedDirective(ctx context.Context, session, shortID string) {
	if s.messenger == nil || session == "" {
		return
	}
	_ = s.messenger(ctx, session, DirectiveDeposed, shortID)
}

// claimSalvage loads the row's salvage pointer for a claim's charter
// envelope (best-effort: a read failure degrades to a pointer-less
// charter — the spawn never fails on it, loudly warned).
func (s *Service) claimSalvage(ctx context.Context, taskID string) *SalvagePointer {
	var raw []byte
	if err := s.q().QueryRow(ctx, `SELECT salvage FROM tasks WHERE id=$1`, taskID).Scan(&raw); err != nil {
		slog.WarnContext(ctx, "board: claim salvage read failed (charter embeds no pointer)",
			"taskId", taskID, "error", err.Error())
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	var p SalvagePointer
	if err := json.Unmarshal(raw, &p); err != nil {
		slog.WarnContext(ctx, "board: claim salvage decode failed (charter embeds no pointer)",
			"taskId", taskID, "error", err.Error())
		return nil
	}
	if p.Empty() {
		return nil
	}
	return &p
}

// deref stringifies a nullable string.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RenderSalvage renders the charter envelope's salvage-handoff block —
// the SAME renderer shape as RenderVerdictChain so the fresh worker's
// charter, and any other embedding surface, render one voice. Empty
// pointer renders "" (the charter stays byte-identical to a
// pointer-less claim).
func RenderSalvage(p *SalvagePointer) string {
	if p == nil || p.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("SALVAGE HANDOFF — a prior attempt on this task was deposed holding pushed work.\n")
	b.WriteString("INHERIT this state (verify it yourself first); do NOT restart cold and do NOT redo\n")
	b.WriteString("what is already pushed:\n")
	if p.Branch != "" {
		fmt.Fprintf(&b, "  Branch: %s\n", p.Branch)
	}
	if p.PR != "" {
		fmt.Fprintf(&b, "  PR: %s\n", p.PR)
	}
	if p.Head != "" {
		fmt.Fprintf(&b, "  Last known head: %s\n", p.Head)
	}
	if p.State != "" {
		fmt.Fprintf(&b, "  Last known state: %s\n", p.State)
	}
	if p.Note != "" {
		fmt.Fprintf(&b, "  Note from the orchestrator: %s\n", p.Note)
	}
	b.WriteString("The pointer records what was known at deposition — treat it as the starting point\n")
	b.WriteString("for your verification, never as verified truth.\n\n")
	return b.String()
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"time"
)

// due_mark.go — #2061/#2065 story 2: the event-driven due-mark, the
// epic's entry-acceleration leg. A turn-end(no-advance) event for a
// claim-holder (emitted by story #2064 at the harness-attested settle
// fold, delivered on the ABI stream, folded by the API's usagestream
// consumer) marks the row's lease DUE — lease_expires_at=now() — so
// the NEXT 30s sweep processes it through the EXISTING quiet arms
// (nudge → ordinal → wedge). Nothing else: the sweep backstop (R5),
// every ladder arm, and the #2043 heartbeat-lapse arm (the one stale
// threshold — HeartbeatLapseThreshold, the audit ruling's ONE KNOB)
// are untouched. The measured incident shape (worker stops 19:51,
// first nudge 21:38 — 1h47m) closes to < one sweep.
//
// Authority surface (the audit rulings, 2026-11-15):
//   - DUE-MARKING authority only. The write never renews a lease,
//     never stamps last_progress_at / last_renewed_at / the liveness
//     hub's worker index (#2042 zero-liveness contract, R3), never
//     touches attempts/failure_count, never writes a transition or an
//     audit event (entry acceleration is not a decision — the ladder
//     decides, and it audits).
//   - advance=yes is TELEMETRY ONLY (board_turn_advances_total): the
//     lease is untouched (R2).
//   - Guards (R4): pending_input and paused rows are never due-marked
//     (their workers are waiting on a human / held by the owner, not
//     stalled); a row whose nudge slot is armed is never due-marked
//     (the sweep's quiet arms OWN the live episode — re-marking under
//     a positioned sentinel would restart the episode per turn-end);
//     a row already lease-due is a no-op (the sweep's episode —
//     idempotence by predicate, which is also the cross-replica
//     arbitration: two replicas folding the same event race one
//     guarded UPDATE, the loser's predicate sees lease already due,
//     zero rows, no double count — the freeze arm's discipline).
//   - Epoch mismatch drops, both sides: the CONSUMER (api-side) drops
//     an event whose epoch is not strictly newer than the last seen
//     for the session (stale replay / duplicate / at-least-once
//     redelivery — the resume fold replays); the ROW side drops an
//     event observed BEFORE this claim's mint (stage_entered_at —
//     every claim path writes it in the same statement as the mint),
//     so a straggler from a PREVIOUS claim of the same session (the
//     re-claim case) cannot due-mark the fresh epoch. The mint
//     comparison crosses process clocks by necessity (the observation
//     stamp is the API's); the tolerance is seconds and the failure
//     direction is one early nudge a live worker answers — the safe
//     side, never a lost renewal.
//
// Replica arbitration (#2060 open-question 1, verified): a single
// guarded UPDATE with row-level locking — the reconciler's own
// multi-replica pattern in its single-row shape (freezeLeaseSQL,
// suspend.go: guarded predicates + RowsAffected == 0 means converged
// by them). No SKIP LOCKED here: that arbitration belongs to the
// sweep's multi-row victim scan; one UPDATE on one row is its own
// lock.

// TurnEndDueMark is the board consumer's dispatch for one observed
// EVENT_TYPE_BOARD_TURN_END — the payload facts plus the fold's own
// anchors (workspace, both identity keys, the observation stamp).
type TurnEndDueMark struct {
	// WorkspaceID scopes the fold the event rode (the usagestream
	// gate's workspace) — the due-mark's project join.
	WorkspaceID string
	// SessionRefs are the identity keys the frame carried (#1817: the
	// wire id is the phrase, the harness id rides internal) — the
	// assignee match runs against every key, so a row stamped with
	// either form resolves.
	SessionRefs []string
	// Epoch is the emitter's per-session ordinal (story #2064). The
	// consumer-side watermark dedupe runs BEFORE dispatch; the value
	// rides for observability and future budget seams.
	Epoch uint64
	// Advanced is the turn's advance flag. true ⇒ telemetry only.
	// Under transport v2 the emitter carries false always and the
	// verdict derives STORE-SIDE (TurnStartedAt's window) below.
	Advanced bool
	// TurnStartedAt is the event's turn window start (the emitter's
	// busy-mark anchor; #2100 transport v2). Zero = absent (a pre-v2
	// emitter): the store-side advance derivation is SKIPPED and the
	// payload's flag is trusted (wire-compat one release train).
	TurnStartedAt time.Time
	// ObservedAt is the fold's observation stamp (the API clock — the
	// mint-guard anchor; zero means "resolve at write time").
	ObservedAt time.Time
}

// advanceObservedByStore answers, advisorially, whether an
// advance-class write by one of the refs landed inside the turn window
// (#2100 transport v2, design §2.2): a board_transitions row by the
// assignee, or a verdict board_event by the assignee — the facts the
// platform already owns end-to-end (timestamped, actor-attributed).
// ADVISORY ONLY: the guarded due-mark UPDATE below re-checks the same
// EXISTS at write time, so a race between check and write can miss a
// COUNT, never a due-mark (the house discipline — no
// lookup-then-write authority).
func (s *Service) advanceObservedByStore(ctx context.Context, workspaceID string, refs []string, windowStart time.Time) (bool, error) {
	var found bool
	err := s.q().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM tasks t
			JOIN projects p ON p.id = t.project_id
			WHERE p.target_workspace_id = $1
			  AND t.stage = 'in_progress' AND NOT t.pending_input AND NOT t.paused
			  AND t.claim_token IS NOT NULL AND t.assignee_session = ANY($2)
			  AND (EXISTS (SELECT 1 FROM board_transitions bt
			        WHERE bt.task_id = t.id AND bt.at > $3
			          AND bt.actor->>'session_id' = ANY($2))
			    OR EXISTS (SELECT 1 FROM board_events be
			        WHERE be.item_id = t.id AND be.action = 'verdict' AND be.created_at > $3
			          AND be.actor->>'session_id' = ANY($2)))
		)`, workspaceID, refs, windowStart).Scan(&found)
	return found, err
}

// storeAdvanceNotExistsSQL is the guarded due-mark's advance arm — the
// SAME derivation as advanceObservedByStore, inlined into the UPDATE's
// WHERE so the DECISION is the guarded statement's own predicate
// (re-checked at write time under the row lock; manager/reconciler
// moves carry no actor session_id and never match).
const storeAdvanceNotExistsSQL = `
		  AND NOT EXISTS (SELECT 1 FROM board_transitions bt
			WHERE bt.task_id = t.id AND bt.at > $4
			  AND bt.actor->>'session_id' = ANY($2))
		  AND NOT EXISTS (SELECT 1 FROM board_events be
			WHERE be.item_id = t.id AND be.action = 'verdict' AND be.created_at > $4
			  AND be.actor->>'session_id' = ANY($2))`

// ConsumeTurnEnd is the board engine's single due-marking entry point
// for turn-end events. It is the ONLY new write in the event path;
// every downstream behavior belongs to the unchanged sweep.
func (s *Service) ConsumeTurnEnd(ctx context.Context, e TurnEndDueMark) (bool, error) {
	if e.Advanced {
		// R2: the advancing leg records and leaves — the lease is
		// untouched, the due-mark counter is NOT incremented.
		turnAdvances.Inc()
		return false, nil
	}
	if e.WorkspaceID == "" || len(e.SessionRefs) == 0 {
		return false, nil // unattributable: never a due-mark (the user-session ignore falls out of the assignee match too)
	}
	observed := e.ObservedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	// Transport v2's store-side advance derivation (#2100): armed AND
	// the turn window present ⇒ the STORE answers "did this turn
	// advance" from the writes the platform already owns — a worker-
	// side or file-artifact-declared advance carries no authority once
	// v2 is armed (the frozen-file class never wins over store truth).
	// An observed advance is telemetry-only (same R2 posture as a v1
	// emitter's flag); the guarded UPDATE below re-checks the same
	// predicate at write time. Kill-switch off (or a pre-v2 emitter
	// carrying no window): the payload's declared flag is trusted —
	// today's path verbatim.
	sql := `
		UPDATE tasks t SET lease_expires_at=now(), updated_at=now()
		FROM projects p
		WHERE p.id = t.project_id
		  AND p.target_workspace_id = $1
		  AND t.stage = 'in_progress'
		  AND NOT t.pending_input
		  AND NOT t.paused
		  AND t.claim_token IS NOT NULL
		  AND t.assignee_session = ANY($2)
		  AND t.nudge_id IS NULL
		  AND t.lease_expires_at > now()
		  AND t.stage_entered_at <= $3`
	args := []any{e.WorkspaceID, e.SessionRefs, observed}
	if s.cfg.PresenceTransportV2 && !e.TurnStartedAt.IsZero() {
		if advanced, err := s.advanceObservedByStore(ctx, e.WorkspaceID, e.SessionRefs, e.TurnStartedAt); err == nil && advanced {
			turnAdvances.Inc()
			return false, nil // the store observed the advance — the row is not due (advisory short-circuit)
		}
		sql += storeAdvanceNotExistsSQL
		args = append(args, e.TurnStartedAt)
	}
	tag, err := s.q().Exec(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		// No live claim matched (user chat / other workspace / stale
		// epoch / guarded row / already due / store-observed advance):
		// converged, silently — the sweep owns the row either way.
		return false, nil
	}
	dueMarksByTrigger.WithLabelValues(DueMarkTriggerEvent).Inc()
	return true, nil
}

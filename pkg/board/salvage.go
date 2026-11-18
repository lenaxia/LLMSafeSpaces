// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// salvage.go — the salvage-prefer terminal's engine-side half
// (reports/specs/claim-wait-lifecycle-designs, §4/§5.2): when a
// WORKER-CLAIM attempts-exhaustion fires (the dispatch claim's escalate
// arm and the manual task_start escalate arm), the row lands
// stage='failed' even when the lapsed attempts left salvageable work
// behind — an open PR from the lane, unpushed lane commits, uncommitted
// worktree changes (the #2064 shape: a row holding a perfectly good
// open PR reads as a failure). The S5 depose/rearm/inherit machinery
// already exists for the manager depose lever; this file adds the SEAM
// the exhaustion sites read salvage facts through, plus the one shared
// policy write both sites ride.
//
// The engine stays repo-blind (the poverty line: pkg/board imports
// nothing new): everything it knows about a lane's leftover work
// arrives through SalvageSource, an injectable seam in the
// AssigneeLivenessSource house pattern — Nil = inert = today's
// behavior (the exhaustion terminal stays byte-identical; the inert
// contract is pinned). The production implementation lives at the app
// seam (api/internal/app/board_salvage.go), where the git/GitHub truth
// is reachable.
//
// The LOOP BOUND (§5.2): the salvage path requires facts that DIFFER
// from the row's recorded pointer (new head, new PR, or first
// sighting). A row whose signals are identical to its pointer reads as
// "no NEW salvage" and takes the ordinary failed terminal — one
// salvage round per real change, never an infinite depose loop.
//
// Scope discipline: exactly TWO sites consult the seam (claimOnce and
// TaskStart — the worker-claim exhaustion terminals). The reconciler's
// persona claim-time escalation (reviewer rows) carries no lane
// worktree and stays byte-identical, NEVER probing (pinned); the wedge
// path's own terminal is a liveness verdict, not exhaustion, and is
// untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// SalvageSource answers "did the lapsed attempts leave salvageable work
// on this task's lane?" for the exhaustion sites. Implementations must
// be cheap (~2s budget — the deposeShowsLife house pattern: the probe
// runs under the claim's row lock, and SKIP LOCKED keeps a slow probe
// from stalling the fleet). Failure to answer = nil (no signals),
// LOUDLY logged — a dead source degrades to today's terminal, never a
// failed claim; absence of salvageable work = nil too (the ordinary
// failed terminal, byte-identical).
type SalvageSource interface {
	// SalvageFor returns the salvage signals for the task's lane, or
	// nil when nothing salvageable exists (or the source could not
	// answer). The Task carries the row's recorded Salvage pointer —
	// implementations resolve the lane from it when present.
	SalvageFor(ctx context.Context, t *Task) *SalvageSignals
}

// SalvageSignals is the POOR facts set the exhaustion policy reads:
// the lane's branch, its last known head, the open PR from the lane
// (number or URL), and whether uncommitted charter-relevant worktree
// changes exist. Poverty is the portability (the
// AssigneeObservation discipline): three identifiers and a bit the app
// seam derives from git/GitHub, nothing more.
type SalvageSignals struct {
	Branch      string
	Head        string
	OpenPR      string
	Uncommitted bool // charter-relevant worktree changes exist
}

// DifferFrom is the loop bound: do these signals carry facts the row's
// recorded pointer does NOT already hold? A nil (or empty) pointer is
// a FIRST SIGHTING — anything salvageable differs. Identical
// {Branch, Head, OpenPR} to the pointer ⇒ NOT new salvage ⇒ the
// ordinary terminal (one salvage round per real change). Uncommitted
// is deliberately NOT part of the identity triple (§5.2): it is a
// presence fact with no identity of its own, riding the pointer's
// State prose instead.
func (s *SalvageSignals) DifferFrom(p *SalvagePointer) bool {
	if p == nil || p.Empty() {
		return true // first sighting
	}
	return s.Branch != p.Branch || s.Head != p.Head || s.OpenPR != p.PR
}

// Empty reports whether the signals assert anything salvageable at all
// (a source answering non-nil with an all-empty facts set is lying —
// salvageDeposeAtCap degrades that to the ordinary terminal, loudly).
func (s *SalvageSignals) Empty() bool {
	return s.Branch == "" && s.Head == "" && s.OpenPR == "" && !s.Uncommitted
}

// Pointer composes the S5 row pointer the exhaustion depose records:
// the signals' facts plus the exhaustion-salvage State prose the next
// claimant's charter renders (the RenderSalvage handoff — inherit,
// don't restart).
func (s *SalvageSignals) Pointer(attempts int) SalvagePointer {
	state := fmt.Sprintf(
		"exhaustion-salvage: the prior attempts reached the cap (%d) holding salvageable work on this lane — inherit and verify, do not restart",
		attempts)
	if s.Uncommitted {
		state += "; uncommitted worktree changes were present at deposition"
	}
	return SalvagePointer{Branch: s.Branch, Head: s.Head, PR: s.OpenPR, State: state}
}

// WithSalvageSource installs the salvage-prefer terminal's probe seam.
// Nil (or absent) is the inert contract: the exhaustion sites never
// probe and today's failed terminal stays byte-identical (pinned).
func WithSalvageSource(src SalvageSource) Option {
	return func(s *Service) { s.salvage = src }
}

// salvageProbeBudget clamps one SalvageFor call engine-side. The seam's
// own hygiene asks implementations for ~2s; the clamp protects the
// claim transaction's row lock from a source that misbehaves (the
// probe runs inside the exhaustion arm's tx, under FOR UPDATE).
const salvageProbeBudget = 2 * time.Second

// salvageDeposeAtCap is the shared exhaustion-site policy (claimOnce
// AND TaskStart ride this verbatim — one policy, two doors): probe the
// seam under the row lock; signals present AND differing from the
// row's recorded pointer ⇒ depose-with-salvage INSTEAD of the failed
// terminal — the S5 pointer, backlog behind the failure backoff,
// attempts=0 (the rearm posture), overlay cleared, one audited
// salvage_depose event. No signals, an inert source, identical facts
// (the loop bound), or a raced row ⇒ returns false and the caller runs
// today's failed terminal byte-identically.
//
// The caller owns the transaction and the commit: this writes (or does
// not) and returns the event for the caller's commitPublishing, with
// the pre-write session identity for the post-commit DEPOSED directive
// (the existing depose sender seam; at the exhaustion sites the row is
// backlog and typically identity-less — the reclaims cleared it — so
// the directive is best-effort to the last implementer when one
// survives, and a no-op otherwise, exactly like depose's backlog arm).
//
// No board_transitions row is written: the row is already 'backlog' at
// both sites (the candidate predicate / task_start's guard), and the
// depose of a backlog row moves nothing (the TestDepose backlog-arm
// precedent — its record IS the audit event).
func (s *Service) salvageDeposeAtCap(ctx context.Context, tx pgx.Tx, projectID string, cur *Task, attempts int) (bool, Event, string, error) {
	if s.salvage == nil {
		return false, Event{}, "", nil // inert: today's terminal, byte-identical
	}
	// The probe: clamped, under the caller's tx context (the row lock
	// is held; SKIP LOCKED elsewhere keeps the fleet moving). A source
	// failure answers nil loudly — the ordinary terminal, never a
	// failed claim.
	pctx, cancel := context.WithTimeout(ctx, salvageProbeBudget)
	signals := s.salvage.SalvageFor(pctx, cur)
	cancel()
	if signals == nil || signals.Empty() {
		// nil = nothing salvageable (or the source could not answer);
		// all-empty facts = a source asserting salvage while recording
		// nothing — same treatment, loudly.
		if signals != nil {
			slog.WarnContext(ctx, "board: salvage source returned signals with no recordable facts — degrading to the ordinary terminal",
				"taskId", cur.ID)
		}
		return false, Event{}, "", nil
	}
	if !signals.DifferFrom(cur.Salvage) {
		// The loop bound: identical facts to the recorded pointer —
		// no NEW salvage; the ordinary failed terminal.
		return false, Event{}, "", nil
	}
	ptr := signals.Pointer(attempts)
	ptrJSON, err := json.Marshal(ptr)
	if err != nil {
		return false, Event{}, "", err
	}
	// The pre-write session identity for the post-commit directive
	// (assignee first, the last implementer as fallback — the forward
	// exit's capture; both are typically NULL here, the reclaims'
	// doing).
	deposedSession := cur.AssigneeSession
	if deposedSession == "" {
		deposedSession = cur.ImplementerSession
	}
	// The depose-with-salvage write: the REARM posture (attempts=0,
	// overlay cleared, pending_origin=NULL, episode slots cleared —
	// rearm.go's column set) parked backlog behind the failure backoff
	// (depose.go's cooldown discipline), plus the S5 pointer. NOTE: the
	// typed-wait columns (wait-typed-state's wait_on/wait_reason) clear
	// with the mint/exits on that story's landing; against this schema
	// the row is an unclaimed backlog row whose waits were already
	// cleared by their exits.
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='backlog', stage_entered_at=now(),
			claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
			claim_incremented_attempts=false, claim_persona=NULL,
			nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
			pending_origin=NULL, pending_input=false,
			attempts=0, failure_count=0, last_attempt_failed=false, last_failure_at=NULL,
			cooldown_until=now()+make_interval(secs=>$2::float8),
			salvage=$3
		WHERE id=$1 AND stage='backlog'`,
		cur.ID, s.cfg.failureBackoff(cur.FailureCount+1).Seconds(), ptrJSON)
	if err != nil {
		return false, Event{}, "", err
	}
	if tag.RowsAffected() == 0 {
		// Raced away (claimed/moved under us): fall through to the
		// terminal path, whose own predicate arbitrates identically to
		// today.
		return false, Event{}, "", nil
	}
	// Episode-unique id (nanos — the reconciler's persona-escalate
	// precedent): attempts RESET here, so an id keyed on the count
	// would collide across salvage episodes and silently drop the
	// audit event (ON CONFLICT DO NOTHING).
	evt, err := recordEventTx(ctx, tx, projectID,
		"engine:salvage_depose:"+cur.ID+":"+strconv.FormatInt(time.Now().UnixNano(), 10),
		"salvage_depose", "task", cur.ID, Actor{Kind: ActorSystem, Surface: "engine"},
		map[string]any{
			"task_id":         cur.ID,
			"salvage":         ptr,
			"facts":           signals,
			"attempts_at_cap": attempts,
			"deposed_session": deposedSession,
			"from_stage":      StageBacklog,
		})
	if err != nil {
		return false, Event{}, "", err
	}
	return true, evt, deposedSession, nil
}

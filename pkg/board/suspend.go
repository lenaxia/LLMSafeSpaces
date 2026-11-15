// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
)

// suspend.go — the workspace-suspension freeze seam (the 2026-11-13/14
// recurring incident: rate-limit suspensions tear the workspace pod
// down, every worker claim inside it lapses, and the reclaim machinery
// burned the attempts to exhaustion — 3/3, escalated, ready set
// drained, manual rearm sweep every time; structural, not flake).
//
// Owner-approved fix direction (2026-11-14): a suspended workspace must
// not burn its workers' claims. Shape (a) — FREEZE — chosen over shape
// (b) (auto-pause) because the lease clock already has an
// evidence-driven renewal arm (the seamless ladder's ladderRenew): a
// suspension is POSITIVE evidence the assignee cannot answer, so the
// freeze is one more renewal source, not a new claim-lifecycle
// mechanism. Pausing instead would churn pause flags the human never
// asked for and race the pause-must-pause directive machinery for
// workers that are already gone.
//
// The engine (pkg/board) stays Kubernetes-agnostic in the
// AssigneeLivenessSource house pattern: everything it knows about
// workspace suspension arrives through SuspendSource, an injectable
// seam — nil = inert, and every engine guarantee holds at nil (today's
// semantics; the existing suite passes unchanged, that inert contract
// is pinned). The production adapter (api/internal/app/
// board_suspend_source.go) answers from the workspace watcher's phase
// cache: Suspending/Suspended ⇒ suspended.
//
// Honesty mapping (the evidence discipline of liveness.go):
//
//   - workspace in the suspended set  ⇒ FREEZE: RC1 renews the claim
//     (no expiry evaluation, no attempt increment, no nudge, no
//     escalation) and the claim choke point grants nothing new into
//     the workspace (a fresh spawn there would auto-RESUME it through
//     the activator — the exact suspension the platform imposed).
//   - workspace absent from the set    ⇒ today's semantics exactly (a
//     genuinely dead worker while the workspace runs still reclaims,
//     still increments, still escalates — the regression pin).
//   - source failure / seam absent    ⇒ NOT suspended, loudly (the
//     liveness rule: absence of evidence never decodes as suspension;
//     a broken source must not freeze the fleet's leases forever).
//
// Renewal (not mere skip) is deliberate: the freeze re-arms
// lease_expires_at by one window each time the lease would otherwise
// lapse, so the row LEAVES the victim set and post-resume the worker
// finds a live claim with a full window — no first-sweep expiry race
// against pod startup, no delivery-failure attempt burn on the bounce
// seam. Idempotent by the victim predicate: a frozen row re-enters the
// sweep only when the renewed lease lapses again (≈2 writes/day/row),
// and rapid suspend/resume cycles converge to the same state.

// SuspendSource answers one POOR question per sweep: which workspaces
// are currently suspended (the Workspace CRD's Suspending/Suspended
// phases — manual, idle, and rate-limit suspensions all land there;
// the controller tears the pod down and the sessions go with it).
//
// Call hygiene (the liveness seam's): cheap — one call per sweep, one
// per claim-choke-point consult; implementations answer from an
// in-process cache (the watcher), never a fresh Kubernetes round trip.
// A source that cannot answer returns a nil/empty set and logs loudly
// itself; the engine never errors a sweep on the suspension question.
type SuspendSource interface {
	SuspendedWorkspaces(ctx context.Context) map[string]bool
}

// suspendSnapshot consults the seam once: nil set = nothing suspended
// (the inert contract — seam absent, or the adapter's honest
// no-evidence answer).
func (s *Service) suspendSnapshot(ctx context.Context) map[string]bool {
	if s.suspended == nil {
		return nil
	}
	set := s.suspended.SuspendedWorkspaces(ctx)
	if len(set) == 0 {
		return nil
	}
	return set
}

// workspaceSuspended is the claim choke point's per-consult form: true
// only on POSITIVE evidence (the workspace is in the source's set).
func (s *Service) workspaceSuspended(ctx context.Context, workspaceID string) bool {
	if s.suspended == nil || workspaceID == "" {
		return false
	}
	set := s.suspended.SuspendedWorkspaces(ctx)
	if set == nil {
		return false
	}
	if set[workspaceID] {
		return true
	}
	// One loud line per frozen-claim consult would flood at sweep
	// cadence; the ADAPTER owns degradation visibility (the liveness
	// seam's loud-gauge rule). Here: absent = not suspended, quietly.
	return false
}

// freezeLeaseSQL is the freeze arm's write: renewal by one full window,
// the nudge slot cleared (a sentinel positioned pre-suspend must not
// anchor a post-resume wedge verdict against a session that never saw
// it), busy_renews reset (the suspension is an epoch break for
// busy-only evidence). last_progress_at is deliberately untouched —
// suspension is not progress, and claiming so would poison the
// progress watermark. No transition row, no audit event, no overlay:
// the renewal discipline of ladderRenew (renewals never flood the
// trail). The renewal is projected to the S4 observation plane as a
// Renewed lease event with source workspace_suspended — which claims
// the suspension is holding must be VISIBLE, or the next incident
// looks like a stuck board again (the 2026-11-14 class).
const freezeLeaseSQL = `
	UPDATE tasks SET lease_expires_at=now()+make_interval(secs=>$2::float8),
		nudge_id=NULL, nudged_at=NULL, busy_renews=0, updated_at=now()
	WHERE id=$1 AND stage=$3`

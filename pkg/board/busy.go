// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// busy.go — the read edge's badge-honesty seam (RC-C S2, the
// 2026-10-30 incident remediation: the busy badge was a CLAIMED-SPINNER
// — TaskCard rendered a spinning loader for stage=in_progress ∧
// assignee present, with NO liveness input, so a lapsed, dead, or
// wedged claim spun forever telling the user someone was working when
// nobody was).
//
// Two belts, deliberately split across the seam:
//
//   - BADGE HONESTY (this file): the spinner reflects LIVENESS, never
//     claim state. The read edge decorates each claimed in_progress row
//     with AssigneeBusy — TRUE only when the harness's derived-busy
//     truth says the assignee session is MID-TURN (streaming or an
//     in-flight tool call). Idle, unknown (no source wired, session
//     never observed here), and lapsed claims all render QUIET.
//   - BUSY TTL (this file, claim-side): a claim past its lease without
//     renewal stops reporting busy — lease_expires_at is the TTL. The
//     renewal budget's semantics are untouched (reconciler.go's
//     boardBusyRenewCap still bounds consecutive busy-ONLY renewals;
//     movement evidence still renews uncapped): the ladder may extend
//     a lease on busy evidence exactly as before, and the moment it
//     stops — cap exhausted, episode closed, no renewal landed — the
//     read edge quiets the badge at the very next poll. A source
//     latched busy-true forever therefore cannot spin the badge past
//     cap × lease + one window, the read-side mirror of the ladder's
//     own bound.
//
// The seam is POOR and PURE by construction (the liveness.go house
// pattern): one batched question per read — "which of these assignee
// refs are mid-turn right now?" — no cursors, no sweep state, no
// per-ref deltas (those belong to ObserveAssignees and MUST NOT be
// consumed here; a read that ate the sweep's movement evidence would
// blind the lease ladder). Nil = inert: nothing is busy, every claimed
// row renders quiet — the honest answer when the harness cannot be
// asked, and never a regression to the dishonest spin.

import (
	"context"
	"time"
)

// AssigneeBusySource answers "is this assignee session mid-turn RIGHT
// NOW?" for the read edge (the badge). One batched call per read; the
// answer map carries ONLY the busy refs (absent = quiet). Refs are the
// assignee values as displayed (raw harness id or #1756 phrase — the
// production adapter resolves both). Implementations must be cheap
// (in-memory consults after at most one identity resolve) and must
// NEVER mutate observation state: this query is not evidence for the
// lease ladder, and the lease ladder's answers are not cached here.
type AssigneeBusySource interface {
	BusyAssignees(ctx context.Context, refs []string) map[string]bool
}

// WithBusySource installs the badge-honesty seam (RC-C S2). Nil (or
// absent) is the inert posture: no row is ever reported busy — every
// claimed row renders claimed-but-quiet, never the dishonest spin.
func WithBusySource(src AssigneeBusySource) Option {
	return func(s *Service) { s.busySrc = src }
}

// busyReportable is the busy TTL's claim-side gate: only a CLAIMED
// in_progress row holding a LIVE lease may report busy. A claim past
// its lease without renewal — the lapsed shape the incident spun
// forever — quiets here whatever the harness's (possibly stale) busy
// truth says; a claim with no lease at all (the reclaim query's NULL
// shape) is equally not working. now is the read's own clock: the
// boundary is a badge, not a verdict, and rides the poll cadence.
func busyReportable(t *Task, now time.Time) bool {
	return t != nil &&
		t.Stage == StageInProgress &&
		t.AssigneeSession != "" &&
		t.LeaseExpiresAt != nil &&
		t.LeaseExpiresAt.After(now)
}

// decorateAssigneeBusy is the read edge's busy pass: collect the
// reportable rows' assignee refs, ask the seam ONE batched question,
// stamp AssigneeBusy on the response copies. Derived and read-edge
// only — the stored row carries no busy column (the harness's answer
// is a property of NOW, sampled per read; persisting it would only add
// staleness). Called after translateTaskReads so the refs are exactly
// what the badge displays. Failure degrades to quiet (never spin):
// an unanswerable question is not evidence someone is working.
func (s *Service) decorateAssigneeBusy(ctx context.Context, tasks ...*Task) {
	src := s.busySrc
	if src == nil || len(tasks) == 0 {
		return // inert seam: claimed-but-quiet (the honest default)
	}
	now := time.Now()
	reportable := make([]*Task, 0, len(tasks))
	refs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		t.AssigneeBusy = false // re-derived fresh every read
		if busyReportable(t, now) {
			reportable = append(reportable, t)
			refs = append(refs, t.AssigneeSession)
		}
	}
	if len(refs) == 0 {
		return
	}
	busy := src.BusyAssignees(ctx, refs)
	if len(busy) == 0 {
		return
	}
	for _, t := range reportable {
		if busy[t.AssigneeSession] {
			t.AssigneeBusy = true
		}
	}
}

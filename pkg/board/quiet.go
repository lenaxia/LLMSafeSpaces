// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// quiet.go — the #2047 read-edge quiet/busy-seconds surface (parent
// #2040): the 2026-11-15 incident's workers were SILENT — the busy
// badge answered bool and the wire carried NOTHING that distinguished
// "working quietly" (mid-turn, derived-busy) from "silent" (neither
// frames nor progress — the shape that held 3 of 4 slots for 4+ hours
// with zero alerts).
//
// The seam is the busy.go house pattern (POOR and PURE): one batched
// question per read — "for each assignee ref, how long since their
// last WORKER-activity stamp, and how long has a derived-busy worker
// been frame-silent?" — no cursors, no sweep state, no movement
// deltas. Nil = inert: every row renders unknown (nil fields), the
// honest answer when the harness side cannot be asked.
//
// Answer honesty (the hub side defines it; stated here for the wire):
//
//   - QuietSeconds nil = UNKNOWN (this process never observed a worker
//     frame for the session — hub restart, unindexed). Unknown is not
//     quiet and never feeds an alert verdict.
//   - BusySeconds is set only while the derived-busy truth holds the
//     session mid-turn; its value is the same quiet clock — how long
//     the busy worker has been frame-silent (a streaming worker sits
//     near zero; a mid-tool-call worker grows — ALIVE either way, the
//     2026-10-29 ruling). There is no per-session busy-episode-since
//     stamp anywhere today; fabricating one for a surface-only v1
//     would be state for state's sake. Nil = not busy / unknown.

import (
	"context"
	"time"
)

// AssigneeQuietAnswer is one assignee ref's quiet/busy-seconds truth.
// Field semantics mirror the Task wire fields (see quietReportable /
// the Task docs) — pointers because UNKNOWN (nil) is a first-class
// answer, never 0.
type AssigneeQuietAnswer struct {
	// QuietSeconds: seconds since the last worker-activity stamp;
	// nil = unknown (never observed in this process).
	QuietSeconds *int
	// BusySeconds: set only while derived-busy — the quiet clock's
	// value at that moment; nil = not busy or busy-unknown.
	BusySeconds *int
}

// AssigneeQuietSource answers the read edge's quiet/busy-seconds
// question (one batched call per read; implementations must be cheap
// and must NEVER mutate observation state — this query is not
// evidence for the lease ladder, the same purity as the busy seam).
type AssigneeQuietSource interface {
	AssigneeQuiet(ctx context.Context, refs []string) map[string]AssigneeQuietAnswer
}

// WithQuietSource installs the quiet/busy-seconds seam (#2047). Nil
// (or absent) is the inert posture: every eligible row renders
// unknown — never a fabricated quiet figure.
func WithQuietSource(src AssigneeQuietSource) Option {
	return func(s *Service) { s.quietSrc = src }
}

// quietReportable is the quiet pass's eligibility gate: any claimed
// in_progress row with an assignee. Deliberately WIDER than the busy
// gate (busyReportable) — the busy TTL quiets a lapsed claim's badge,
// but a lapsed claim's QUIET figure is exactly the incident's signal
// and must stay visible.
func quietReportable(t *Task) bool {
	return t != nil &&
		t.Stage == StageInProgress &&
		t.AssigneeSession != ""
}

// decorateAssigneeQuiet is the read edge's quiet pass: collect the
// reportable rows' assignee refs, ask the seam ONE batched question,
// stamp AssigneeQuietSeconds everywhere known, and AssigneeBusySeconds
// only where the busy gate also holds (the TTL stance — a lapsed
// claim never spins). Called after decorateAssigneeBusy so the two
// surfaces stay independently readable. Failure degrades to unknown
// (nil fields), never an invented figure.
func (s *Service) decorateAssigneeQuiet(ctx context.Context, tasks ...*Task) {
	src := s.quietSrc
	if src == nil || len(tasks) == 0 {
		return // inert seam: unknown everywhere (the honest default)
	}
	now := time.Now()
	eligible := make([]*Task, 0, len(tasks))
	refs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		t.AssigneeQuietSeconds = nil // re-derived fresh every read
		t.AssigneeBusySeconds = nil
		if quietReportable(t) {
			eligible = append(eligible, t)
			refs = append(refs, t.AssigneeSession)
		}
	}
	if len(refs) == 0 {
		return
	}
	answers := src.AssigneeQuiet(ctx, refs)
	if len(answers) == 0 {
		return
	}
	for _, t := range eligible {
		ans, ok := answers[t.AssigneeSession]
		if !ok {
			continue
		}
		t.AssigneeQuietSeconds = ans.QuietSeconds
		if ans.BusySeconds != nil && busyReportable(t, now) {
			t.AssigneeBusySeconds = ans.BusySeconds
		}
	}
}

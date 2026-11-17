// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_quiet_source.go — the #2047 production AssigneeQuietSource
// (the board read edge's quiet/busy-seconds seam, quiet.go engine-side)
// plus the hub's QuietFor single-key read: the liveness hub's pure
// read-edge answer to "how long has this assignee's last WORKER
// activity been silent?" (board_liveness.go's hub holds the ordinal
// index; this file holds the quiet question, mirroring
// board_busy_source.go's split).
//
// Purity is the same contract (busy.go / board_busy_source.go): NO
// cursors advance, NO nudge marks are read or written, NO movement
// deltas are consumed. Degradation is QUIET: unknown renders nil
// (never 0 — unknown is a first-class answer and never evidence of
// silence, #1685's fail-toward-silence applied to absence).
//
// The busy-seconds definition (stated once, honestly): there is no
// per-session busy-episode-since stamp anywhere today; the honest
// figure derivable from existing state is the QUIET clock's value at
// the moment the derived-busy truth holds — how long the mid-turn
// worker has been frame-silent (a streaming worker sits near zero; a
// mid-tool-call worker grows; both are ALIVE — the 2026-10-29 ruling).

import (
	"context"
	"log/slog"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// QuietFor answers how long since the session's last WORKER-activity
// stamp (issue #2047's hub read). ok=false when this process holds no
// stamp for the session — hub restart, unindexed, never observed —
// and unknown NEVER alerts.
func (h *boardLivenessHub) QuietFor(sessionID string) (time.Duration, bool) {
	if h == nil || sessionID == "" {
		return 0, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.framesWorkerAt[sessionID]
	if !ok {
		return 0, false
	}
	return time.Since(at), true
}

// AssigneeQuiet implements board.AssigneeQuietSource (#2047): one
// batched identity resolve + the in-process worker index, answering
// per ref exactly as passed. Same legs as BusyAssignees — identity
// (resolveRefs), the worker-frame distrust guard, busyFn — with the
// wall-clock quiet figure leading.
func (h *boardLivenessHub) AssigneeQuiet(ctx context.Context, refs []string) map[string]board.AssigneeQuietAnswer {
	out := make(map[string]board.AssigneeQuietAnswer, len(refs))
	if h == nil || len(refs) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, boardObserveBudget)
	defer cancel()

	arefs := make([]board.AssigneeRef, 0, len(refs))
	for _, r := range refs {
		if r != "" {
			arefs = append(arefs, board.AssigneeRef{SessionID: r})
		}
	}
	if len(arefs) == 0 {
		return out
	}
	rows, err := h.resolveRefs(ctx, arefs)
	if err != nil {
		// Degraded identity resolve: every ref unknown, loudly. The
		// quiet surface degrades for this read only; the next read
		// retries one cheap query.
		slog.WarnContext(ctx, "board: quiet surface resolve degraded this read (all unknown)",
			"refs", len(arefs), "err", err.Error())
		return out
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, row := range rows {
		if !row.present || row.deleted {
			// Present in the answer set with nil figures — unknown (or
			// provably gone) is an ANSWER, never an omission: the caller
			// distinguishes "resolved to silence" from "never asked" by
			// membership (#2047's contract — the review-r1 fix; the old
			// bare continue contradicted the comment above it and left
			// dead sessions missing from the map entirely).
			out[row.ref] = board.AssigneeQuietAnswer{}
			continue
		}
		ans := board.AssigneeQuietAnswer{QuietSeconds: nil, BusySeconds: nil}
		// The quiet clock: whichever identity key moved latest (the
		// ordinal map orders; the companion map times).
		at, ok := h.framesWorkerAt[row.rawID]
		if p, pok := h.framesWorkerAt[row.phrase]; pok && (p.After(at) || !ok) {
			at, ok = p, true
		}
		if ok {
			quiet := int(now.Sub(at).Seconds())
			if quiet < 0 {
				quiet = 0
			}
			ans.QuietSeconds = &quiet
		}
		// The busy leg: BusyAssignees' guards verbatim — the distrust
		// guard reads the WORKER ordinal index (a busy truth this
		// process never observed a worker frame for is not vouched).
		if ans.QuietSeconds != nil && h.busyFn != nil {
			stamp := h.framesWorker[row.rawID]
			if p := h.framesWorker[row.phrase]; p > stamp {
				stamp = p
			}
			if stamp > 0 {
				if busy, known := h.busyFn(row.workspaceID, row.rawID); known && busy {
					busySecs := *ans.QuietSeconds
					ans.BusySeconds = &busySecs
				}
			}
		}
		out[row.ref] = ans
	}
	return out
}

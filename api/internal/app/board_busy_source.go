// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_busy_source.go — the RC-C S2 production AssigneeBusySource:
// the boardLivenessHub's PURE read-edge answer to "is this assignee
// mid-turn right now?" (the busy badge's truth — board_liveness.go's
// hub holds the legs, this file holds the badge's question so the
// sweep machinery and the read machinery stay separately readable).
//
// The same three legs ObserveAssignees composes, sampled WITHOUT any
// sweep state:
//
//   - identity: resolveRefs (raw or #1756 phrase → workspace + raw id
//     + deleted flag) — one batched session_index read;
//   - the frame index: this process must have SEEN a frame for the
//     session (stamp > 0) before its busy answer is trusted — the
//     event that flips a session busy (a PART_START consult) also
//     stamps this index, so busy-with-no-frame-ever is an answer about
//     a session this replica never observed (the ObserveAssignees
//     guard, verbatim: distrust, the safe direction);
//   - busyFn: the usagestream consumer's consult-maintained #1574
//     derived-busy truth — the ONLY leg that distinguishes mid-turn
//     silence from idleness.
//
// PURITY is the contract (engine busy.go): NO cursors advance, NO
// nudge marks are read or written, NO movement deltas are consumed —
// a read that ate the sweep's same-source delta would blind the lease
// ladder's next renewal decision. This method answers the badge and
// nothing else. Degradation is always QUIET: an unreadable index, a
// missing row, a deleted session, an unobserved frame, an unwired
// busyFn — each renders claimed-but-quiet, never the dishonest spin.
//
// The claim-side TTL is NOT here: the engine's decorate pass gates
// busy on the row's live lease (a lapsed claim quiets whatever this
// answer says), so a stale-latched busy truth is bounded by the lease
// window on the read side — the read-edge mirror of the ladder's
// boardBusyRenewCap bound.

import (
	"context"
	"log/slog"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// BusyAssignees implements board.AssigneeBusySource (RC-C S2). The
// answer map carries ONLY the busy refs (absent = quiet), keyed by the
// ref exactly as passed (raw harness id or phrase).
func (h *boardLivenessHub) BusyAssignees(ctx context.Context, refs []string) map[string]bool {
	out := make(map[string]bool, len(refs))
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
		// Degraded identity resolve: NO ref is busy (quiet — the safe
		// direction), loudly. The badge degrades for this read only;
		// the next poll retries one cheap query.
		slog.WarnContext(ctx, "board: busy-badge resolve degraded this read (all quiet)",
			"refs", len(arefs), "err", err.Error())
		return out
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, row := range rows {
		if !row.present || row.deleted {
			continue // unknown here, or provably gone: quiet
		}
		stamp := h.frames[row.rawID]
		if p := h.frames[row.phrase]; p > stamp {
			stamp = p
		}
		if stamp == 0 {
			continue // never observed in this process: unknown quiets
		}
		if h.busyFn == nil {
			continue // no busy leg wired: unknown quiets
		}
		if busy, known := h.busyFn(row.workspaceID, row.rawID); known && busy {
			out[row.ref] = true
		}
	}
	return out
}

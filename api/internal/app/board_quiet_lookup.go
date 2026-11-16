// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_quiet_lookup.go — the #2047 production QuietBoardLookup: the
// proxy-side evaluator's ONLY board input (one batched read per
// workspace per alert pass). Composes the tasks-side truth (the
// in_progress claims with assignees, scoped by the projects'
// target_workspace_id) with the hub's quiet/busy answers (the same
// AssigneeQuiet the read edge's decoration rides — one resolve, one
// source of truth).
//
// The seam stays NARROW by construction (issue #2047 R4): the proxy
// handler gets this one interface, never the board service; the join
// is the only new input and it rides the existing alert evaluation
// pass — no new polling loop. pending_input rows are excluded (their
// worker is waiting on a HUMAN — not the silent-worker shape), and
// suspended workspaces never reach the pass at all (the reconciler
// only sweeps Active).

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
)

// quietBoardLookup is the production seam implementation.
type quietBoardLookup struct {
	pool *pgxpool.Pool
	hub  *boardLivenessHub
}

// newQuietBoardLookup builds the #2047 seam: board tasks by workspace
// + the liveness hub's quiet/busy answers, composed per pass.
func newQuietBoardLookup(pool *pgxpool.Pool, hub *boardLivenessHub) handlers.QuietBoardLookup {
	return &quietBoardLookup{pool: pool, hub: hub}
}

// InProgressAssignees implements handlers.QuietBoardLookup: every
// claimed in_progress task of the workspace's projects, with the
// assignee's quiet/busy truth attached. The nil contract is LOAD-BEARING
// (review r2): nil = DEGRADED (pool error, scan failure — no evidence
// either way, the evaluator fires no verdicts), while a SUCCESSFUL read
// returns non-nil — an explicit empty set means the workspace genuinely
// holds no claimed in_progress rows, and the evaluator resolves live
// alerts whose subjects departed.
func (q *quietBoardLookup) InProgressAssignees(ctx context.Context, workspaceID string) []handlers.QuietTaskFact {
	if q == nil || q.pool == nil || q.hub == nil || workspaceID == "" {
		return nil
	}
	rows, err := q.pool.Query(ctx, `
		SELECT t.short_id, p.short_id, t.lease_expires_at, t.last_progress_at, t.assignee_session
		FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.target_workspace_id = $1
		  AND t.stage = 'in_progress'
		  AND t.assignee_session IS NOT NULL AND t.assignee_session <> ''
		  AND NOT t.pending_input`, workspaceID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var raws []handlers.QuietTaskFact
	refs := make([]string, 0, 4)
	now := time.Now()
	for rows.Next() {
		var f handlers.QuietTaskFact
		var lease, progress *time.Time
		if err := rows.Scan(&f.TaskShortID, &f.ProjectID, &lease, &progress, &f.AssigneeSession); err != nil {
			return nil
		}
		switch {
		case lease == nil:
			f.LeaseState = "none"
		case lease.After(now):
			f.LeaseState = "live"
		default:
			f.LeaseState = "lapsed"
		}
		f.LastProgressAt = progress
		raws = append(raws, f)
		refs = append(refs, f.AssigneeSession)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	// Success with zero rows: the EXPLICIT empty set (non-nil) — the
	// departure-delta signal, distinct from degraded nil above.
	out := make([]handlers.QuietTaskFact, 0, len(raws))

	// The liveness join: one batched quiet question (identity resolve
	// + worker stamps + busy truth — board_quiet_source.go).
	answers := q.hub.AssigneeQuiet(ctx, refs)
	for _, f := range raws {
		if ans, ok := answers[f.AssigneeSession]; ok {
			f.QuietSeconds = ans.QuietSeconds
			f.Busy = ans.BusySeconds != nil
		}
		out = append(out, f)
	}
	return out
}

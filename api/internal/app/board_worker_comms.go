// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_comms.go — the #1747 worker-directive seams, wired on the
// SAME delivery machinery the platform already uses (the pkg/agent
// adapter's session routes — no new transport):
//
//   - Aborter (delete of an in-flight claim): adapter.Abort — the V1
//     session abort, the only interrupt endpoint.
//   - Messenger (pause-must-pause): adapter.SendAsync delivers the
//     halt/resume directive as a session message; it lands at the
//     worker's current turn boundary (finish the thought, then stop) —
//     never a mid-generation cut.
//
// Both resolve the task's (owner, target workspace) from the board rows
// — the adapter's addressing keys.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// haltDirectiveText / resumeDirectiveText render the worker-facing
// directive messages (the charter envelope's voice).
func haltDirectiveText(shortID string) string {
	return fmt.Sprintf("BOARD PAUSE DIRECTIVE: your task %s has been PAUSED by the owner. "+
		"Finish your current thought, then STOP and wait. Do not move the task's stage and do not "+
		"start new work while paused (forward writes will be refused with 409 task_paused). "+
		"A resume directive will follow when work continues.", shortID)
}

func resumeDirectiveText(shortID string) string {
	return fmt.Sprintf("BOARD RESUME DIRECTIVE: your task %s was resumed — continue working. "+
		"If your claim lapsed while paused, your next write returns 409 claim_token: re-read the "+
		"task with board_read; the engine may already have spawned a fresh worker.", shortID)
}

// leaseAppealDirectiveText is the #1847 pre-reclaim appeal (spec item
// 4): ONE ping at the lapsed claimant's turn boundary — reclaim or
// release — before the row re-pools. Delivered exactly like halt/resume
// (queued behind the in-flight turn, never a mid-generation cut).
func leaseAppealDirectiveText(shortID string, window time.Duration) string {
	return fmt.Sprintf("BOARD LEASE APPEAL: the lease on your claimed task %s lapsed without observed "+
		"liveness. If you are still working it (or hold useful context), act now: keep working, make any board "+
		"write, or call board_reclaim with the claim token from your charter envelope — observed liveness re-arms "+
		"the lease and board_reclaim restores the claim with your context intact (attempts unchanged). If you are "+
		"done or abandoning, do nothing. Without an observed response within %s the task returns to the ready set "+
		"for a fresh worker (and board_reclaim remains your recovery path while the context is worth it).",
		shortID, window.Round(time.Second))
}

// boardTaskScope resolves (owner, target workspace) for a task by id or
// short id — the adapter's addressing keys.
func boardTaskScope(ctx context.Context, pool *pgxpool.Pool, taskRef string) (userID, workspaceID string, err error) {
	err = pool.QueryRow(ctx, `
		SELECT p.user_id, p.target_workspace_id::text
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE t.id::text = $1 OR lower(t.short_id) = lower($1)
		LIMIT 1`, taskRef).Scan(&userID, &workspaceID)
	return userID, workspaceID, err
}

// newBoardAborter builds the delete-of-in-flight-claim interrupt seam.
func newBoardAborter(pool *pgxpool.Pool, adapter agent.Adapter) board.WorkerAborter {
	return func(ctx context.Context, taskID, assigneeSession string) error {
		userID, workspaceID, err := boardTaskScope(ctx, pool, taskID)
		if err != nil {
			return fmt.Errorf("board: abort scope: %w", err)
		}
		return adapter.Abort(ctx, userID, workspaceID, assigneeSession)
	}
}

// newBoardMessenger builds the halt/resume directive seam (SendAsync —
// the message queues behind the in-flight turn and lands at its
// boundary). appealWindow feeds the #1847 lease-appeal directive's
// text (how long the lapsed claimant has to answer before re-pool).
func newBoardMessenger(pool *pgxpool.Pool, adapter agent.Adapter, appealWindow time.Duration) board.WorkerMessenger {
	return func(ctx context.Context, sessionID, directive, taskShortID string) error {
		userID, workspaceID, err := boardTaskScope(ctx, pool, taskShortID)
		if err != nil {
			return fmt.Errorf("board: directive scope: %w", err)
		}
		text := resumeDirectiveText(taskShortID)
		switch directive {
		case board.DirectiveHalt:
			text = haltDirectiveText(taskShortID)
		case board.DirectiveLeaseAppeal:
			text = leaseAppealDirectiveText(taskShortID, appealWindow)
		}
		_, err = adapter.SendAsync(ctx, userID, workspaceID, sessionID, text, session.SendOpts{})
		return err
	}
}

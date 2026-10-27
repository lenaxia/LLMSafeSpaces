// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_retention.go — the #1966 owner-ruling retention policy
// beneath the Workers folder: a hard cap on worker sessions per
// workspace (default 50); past the cap the OLDEST worker sessions are
// HARD-DELETED (the #1882 class semantics — retention, not archiving).
//
// Trigger — on worker create (the ruling's "on-create prune"), chosen
// over a periodic sweep because churn is create-driven (every board
// claim spawns): the prune runs exactly when the set grows past the
// cap, needs no new scheduler surface or cadence config, and the spawn
// path has JUST ensured the pod is active — the pod-side delete leg
// runs against a warm target instead of a sweep waking suspended
// workspaces. Accepted limitation (noted in the worklog): lowering the
// configured cap only takes effect on the next worker create.
//
// Safety disciplines (both pinned in board_worker_retention_test.go):
//   - worker-only eviction, proven before trusted: the candidate
//     listing carries the RAW signals (origin-class worker mark / a
//     parent), and the retainer re-applies the predicate itself — a
//     user session that reaches the list through misclassification is
//     refused; every candidate is then re-verified by a fresh
//     single-row read immediately before the delete;
//   - grace floor: rows updated less than the grace period ago
//     (mid-lease workers; default 1h) are never deleted. Bounded by
//     design: when the floor excludes every candidate the prune
//     deletes nothing and the next create retries — the floor is a
//     guard, never a queue to drain past.
//
// Best-effort like the #1915 marks: any failure logs and the spawn's
// outcome is untouched.

import (
	"context"
	"log/slog"
	"sort"
	"time"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

const (
	// defaultWorkerSessionCap is the ruling's default cap.
	defaultWorkerSessionCap = 50
	// defaultWorkerSessionGrace is the ruling's minimal grace floor
	// (never delete a worker session updated in the last hour).
	defaultWorkerSessionGraceSecs = 3600
)

// workerRetentionLister is the store face the retainer reads: the
// candidate listing (raw signals) and the single-row re-verification.
// Satisfied by *workflows.Store.
type workerRetentionLister interface {
	ListWorkerSessions(ctx context.Context, workspaceID string) ([]wf.WorkerSessionRow, error)
	IsWorkerSession(ctx context.Context, workspaceID, sessionID string) (bool, error)
}

// workerSessionDeleter hard-deletes one session (the #1882 authorized
// core: pod-side delete + platform teardown + origin-mark clean).
type workerSessionDeleter func(ctx context.Context, workspaceID, podIP, sessionID string) bool

type boardWorkerRetainer struct {
	lister  workerRetentionLister
	deleter workerSessionDeleter
	cap     int
	grace   time.Duration
}

func newBoardWorkerRetainer(lister workerRetentionLister, deleter workerSessionDeleter, cap, graceSeconds int) *boardWorkerRetainer {
	if cap <= 0 {
		cap = defaultWorkerSessionCap
	}
	if graceSeconds <= 0 {
		graceSeconds = defaultWorkerSessionGraceSecs
	}
	return &boardWorkerRetainer{lister: lister, deleter: deleter, cap: cap, grace: time.Duration(graceSeconds) * time.Second}
}

// prune enforces the cap for the workspace after a worker create.
// keepSessionID (the just-spawned worker) is never a candidate; podIP
// is the spawn's own activation result (the delete leg's target).
func (r *boardWorkerRetainer) prune(ctx context.Context, workspaceID, podIP, keepSessionID string) {
	rows, err := r.lister.ListWorkerSessions(ctx, workspaceID)
	if err != nil {
		slog.ErrorContext(ctx, "board: worker retention: candidate listing failed",
			"error", err.Error(), "workspaceID", workspaceID)
		return
	}
	if len(rows) <= r.cap {
		return
	}
	over := len(rows) - r.cap

	// Level-1 refusal (misclassification guard): the retainer re-applies
	// the worker predicate to the RAW signals of each row — a user or
	// orchestrator session (neither signal) can only be present through
	// misclassification and is refused here, whatever the caller fed the
	// listing. Archived rows refuse alongside (review r2: the user's
	// deliberate retirement outranks retention — belt and braces with
	// the SQL's archived filter). The grace floor and the keep guard
	// ride the same pass.
	eligible := make([]wf.WorkerSessionRow, 0, len(rows))
	for _, row := range rows {
		if row.SessionID == keepSessionID {
			continue
		}
		if row.Archived {
			continue // user-archived: retirement outranks retention
		}
		if !row.OriginWorker && !row.Parented {
			slog.WarnContext(ctx, "board: worker retention: refusing non-worker row in candidate list",
				"workspaceID", workspaceID, "sessionId", row.SessionID)
			continue
		}
		if time.Since(row.UpdatedAt) < r.grace {
			continue // mid-lease worker — the grace floor
		}
		eligible = append(eligible, row)
	}
	// Oldest-first (the listing pre-orders, but sort defensively — the
	// eviction order is contract, not an accident of the query plan).
	sort.SliceStable(eligible, func(i, j int) bool {
		return eligible[i].LastActivity.Before(eligible[j].LastActivity)
	})

	deleted := 0
	for _, row := range eligible {
		if deleted >= over {
			break
		}
		// Level-2 refusal (the ruling's "classifier must be proven before
		// the deleter trusts it"): fresh single-row re-verification
		// immediately before the delete — a mark that flipped or vanished
		// since the listing refuses the eviction.
		ok, err := r.lister.IsWorkerSession(ctx, workspaceID, row.SessionID)
		if err != nil {
			slog.ErrorContext(ctx, "board: worker retention: re-verification read failed",
				"error", err.Error(), "workspaceID", workspaceID, "sessionId", row.SessionID)
			continue
		}
		if !ok {
			slog.WarnContext(ctx, "board: worker retention: re-verification refused the candidate",
				"workspaceID", workspaceID, "sessionId", row.SessionID)
			continue
		}
		if !r.deleter(ctx, workspaceID, podIP, row.SessionID) {
			continue // failed hard delete — next candidate still gets its chance
		}
		deleted++
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "board: worker retention: pruned worker sessions past the cap",
			"workspaceID", workspaceID, "cap", r.cap, "deleted", deleted, "remaining", len(rows)-deleted)
	}
}

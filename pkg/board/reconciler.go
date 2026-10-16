// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reconciler is the self-healing convergence sweep (design §4.5): RC1 lease
// reclaims, RC2 external-truth drift, RC3 dependency consistency, RC4 epic
// completion net, RC5 terminal-stage immutability (enforced by guards + the DB
// trigger; the sweep never writes terminal rows). Idempotent end-to-end:
// every reconciler write carries a deterministic board_events id
// (reconciler:<action>:<item>:<episode>), so a crash between sweep passes
// replays to the same state (invariant 4).
type Reconciler struct {
	svc *Service
}

// NewReconciler constructs the reconciler over a Service.
func NewReconciler(pool *pgxpool.Pool, cfg EngineConfig, opts ...Option) *Reconciler {
	return &Reconciler{svc: NewService(pool, cfg, opts...)}
}

// Sweep runs one full pass (RC1 → pending coupling → RC2 → RC3 → RC4 net →
// watermark advance). Each phase is its own transaction; a fault at any
// failpoint aborts only that phase (kill-test seam) and the next sweep
// converges.
func (r *Reconciler) Sweep(ctx context.Context) (SweepResult, error) {
	return SweepResult{}, ErrNotImplemented
}

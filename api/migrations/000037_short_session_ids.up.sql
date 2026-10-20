-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Short session IDs (design 0066, #1691). short_id is the
-- platform-minted address: <prefix><8 chars> over [0-9a-z] (ruling 2),
-- minted in Go with crypto/rand at the registration seam
-- (EnsureSessionRegistered) — never in SQL and never deterministically
-- derived from the harness id (ruling 6: a hash would preserve the
-- coupling in disguise). Nullable + partial unique index: existing
-- rows are backfilled by the #1340-style convergence sweep, and the
-- WHERE clause drops to a plain unique index + NOT NULL in a
-- follow-up migration once the fleet-wide backfill has converged
-- (two-phase, honest about the transition). Uniqueness is
-- PER-WORKSPACE only (ruling 3: the owner settled global as
-- unnecessary) — the composite index is also the correctness backstop
-- when two API replicas race a registration: both insert, exactly one
-- wins per Postgres uniqueness, the loser re-mints and re-inserts.

ALTER TABLE public.session_index
    ADD COLUMN IF NOT EXISTS short_id text;

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_index_short
    ON public.session_index (workspace_id, short_id)
    WHERE short_id IS NOT NULL;

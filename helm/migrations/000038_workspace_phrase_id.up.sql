-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Workspace phrase IDs (#1757): every workspace gains a minted
-- 2-word + 1-digit phrase (e.g. amber-river-7) as its human/agent
-- address; the UUID stays the internal primary key. FLEET-unique via
-- the global unique index, minted in Go (crypto-seeded PRNG over the
-- frozen curated lists — the same wordlists as #1756's session
-- phrases, which already exclude the harness slug vocabulary) with
-- check-and-retry. Disjoint from session phrases (3 words) and
-- harness slugs (2 words, no digit) BY SHAPE — pinned in
-- pkg/shortid.
--
-- Nullable + plain unique index: legacy rows converge lazily — the
-- creation path mints, and the FIRST AUTHORIZED read backfills (the
-- access middleware, post-ownership; non-gate reads backfill in the
-- seam). There is NO sweep job (r1: the up-comment previously claimed
-- a #1691-style sweep that does not exist); the index enforces
-- fleet-wide uniqueness for every minted value including during the
-- backfill window.

ALTER TABLE public.workspaces
    ADD COLUMN IF NOT EXISTS phrase_id text;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_phrase
    ON public.workspaces (phrase_id)
    WHERE phrase_id IS NOT NULL AND deleted_at IS NULL;

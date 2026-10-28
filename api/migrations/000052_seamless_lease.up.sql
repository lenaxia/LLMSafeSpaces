-- 000052_seamless_lease — the seamless lease + pending_input-as-flag
-- (reports/specs/seamless-lease-design.md, owner-approved 2026-10-28).
--
-- Part 1 (lease): the nudge ladder's row-side state. nudge_id/nudged_at
-- carry the LATEST sentinel nudge only (owner ruling: "we only need to
-- track the most recent nudge, anything older is not important") — a
-- single nullable slot, overwritten per nudge, cleared on any observed
-- reaction.
--
-- Part 2 (flag-not-stage): pending_input stops being a stage. The row
-- keeps its REAL stage while a boolean flag + the existing pending_origin
-- column carry the input wait — no "return to original stage" machinery
-- (the row never moved). The attempts-exhaustion class
-- (pending_origin='escalated') lands in the NEW 'failed' stage per the
-- owner ruling ("exhausted retries should be failed status"); deliberate
-- parks (worker/drift origins) keep a real stage, flagged. Migration
-- heuristic for in-place rows: escalated → failed; deliberate parks →
-- in_progress (parks happen during work; the prior stage was never
-- stored — this is the one-time cost the flag avoids forever after).
--
-- ORDERING (the populated-DB contract, review round 2): the old
-- constraints are dropped FIRST and the rehome UPDATEs run BEFORE the
-- new constraints are added — a constraint swap validates existing rows
-- at ADD time, so rehoming under the still-old vocabulary is what makes
-- a populated pre-000052 database migratable (the exact rows this
-- migration exists to move). Fully idempotent: every DROP/ADD COLUMN is
-- IF EXISTS/IF NOT EXISTS, every re-added constraint is pre-dropped.
--
-- The WIRE still accepts the 'pending_input' literal as the flag-op
-- alias (surfaces keep compiling; the engine maps it to flag set/clear)
-- — the storable vocabulary is what changes here.

BEGIN;

ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_stage_check;
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_pending_origin_set;

ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS pending_input boolean NOT NULL DEFAULT false;

-- Rehome UNDER NO stage constraint: escalated rows take the failure
-- terminus (origin='escalated' preserved as the re-arm's marker);
-- deliberate parks take in_progress wearing the flag (origin
-- preserved). Every other row already satisfies both target shapes.
UPDATE public.tasks SET stage='failed'
    WHERE stage='pending_input' AND pending_origin='escalated';
UPDATE public.tasks SET pending_input=true, stage='in_progress'
    WHERE stage='pending_input';

ALTER TABLE public.tasks ADD CONSTRAINT tasks_stage_check
    CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'failed'::text])));

-- The flag and its origin travel together for INPUT WAITS, with one
-- deliberate exception: the attempts-exhaustion terminus carries
-- pending_origin='escalated' on an UNflagged failed row — the marker
-- rearm gates on ("targets attempts-exhausted rows") without pretending
-- the row awaits an answer to a question nobody asked. Every escalation
-- writer (claim, dispatch, persona, wedge verdict) writes exactly that
-- shape; the constraint admits it and nothing else outside the paired
-- form.
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_pending_input_set;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_pending_input_set
    CHECK ((pending_input = true AND pending_origin IS NOT NULL)
        OR (pending_input = false AND pending_origin IS NULL)
        OR (pending_input = false AND pending_origin = 'escalated' AND stage = 'failed'));

ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS nudge_id text;
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS nudged_at timestamptz;
-- The nudge-RESPONSE channel (item 6 amendment): board_heartbeat survives
-- as the concrete reply to a lease-nudge, so the row records the latest
-- heartbeat ARRIVAL (server clock — never the client-declared stamp). The
-- wedge predicate reads it ordinally: a heartbeat whose arrival is later
-- than nudged_at is a post-nudge response — the strong ALIVE signal OR'd
-- in at every rung, and the ONLY wedge detector when no liveness source
-- can answer. An arrival BEFORE the nudge is the dead periodic
-- discipline and saves nothing.
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS last_heartbeat_at timestamptz;

COMMIT;

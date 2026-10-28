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
-- keeps its REAL stage while a boolean flag + the existing
-- pending_origin column carry the input wait — no "return to original
-- stage" machinery (the row never moved). The attempts-exhaustion
-- class (pending_origin='escalated') lands in the NEW 'failed' stage
-- per the owner ruling ("exhausted retries should be failed status");
-- deliberate parks (worker/drift origins) keep a real stage, flagged.
-- Migration heuristic for in-place rows: escalated → failed; deliberate
-- parks → in_progress (parks happen during work; the prior stage was
-- never stored — this is the one-time cost the flag avoids forever
-- after).
--
-- The WIRE still accepts the 'pending_input' literal as the flag-op
-- alias (surfaces keep compiling; the engine maps it to flag set/clear)
-- — the storable vocabulary is what changes here.

BEGIN;

ALTER TABLE public.tasks DROP CONSTRAINT tasks_stage_check;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_stage_check
    CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'failed'::text])));

ALTER TABLE public.tasks DROP CONSTRAINT tasks_pending_origin_set;
ALTER TABLE public.tasks ADD COLUMN pending_input boolean NOT NULL DEFAULT false;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_pending_input_set
    CHECK ((pending_input = true) = (pending_origin IS NOT NULL));

ALTER TABLE public.tasks ADD COLUMN nudge_id text;
ALTER TABLE public.tasks ADD COLUMN nudged_at timestamptz;

UPDATE public.tasks SET stage='failed'
    WHERE stage='pending_input' AND pending_origin='escalated';
UPDATE public.tasks SET pending_input=true, stage='in_progress'
    WHERE stage='pending_input';

COMMIT;

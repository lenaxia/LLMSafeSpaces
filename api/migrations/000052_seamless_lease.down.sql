-- 000052_seamless_lease (down) — reverse the vocabulary swap, the flag
-- columns, and the nudge slot. ORDERING mirrors the up (the populated-DB
-- contract): the NEW constraints drop first, BOTH rehome classes return
-- to the pending_input stage while no stage constraint is in force, the
-- old-vocabulary constraints are re-added, and only then do the flag and
-- nudge columns go. Nudge state is dropped (it is sweep-local
-- bookkeeping; a downgrade reconciler re-derives nothing from it).

BEGIN;

ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_pending_input_set;
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_stage_check;

-- Rehome every class the old vocabulary cannot express while no stage
-- constraint is in force: deliberate parks (flagged, origin preserved)
-- and the attempts-exhaustion terminus (failed + escalated) return to
-- the pending_input stage; a failed row with NO origin (the manager
-- move and the nudge self-fail shapes — legal under the new paired-form
-- constraint's arm 2) has no pending_input-stage equivalent, so it
-- lands backlog (the re-entry stage of failure; the review round-4
-- reproduction class). Belt: any non-pending_input row still carrying
-- an origin loses it (the old constraint requires the paired form).
UPDATE public.tasks SET stage='pending_input'
    WHERE pending_input = true;
UPDATE public.tasks SET stage='pending_input'
    WHERE stage='failed' AND pending_origin='escalated';
UPDATE public.tasks SET stage='backlog', last_attempt_failed=true
    WHERE stage='failed' AND pending_origin IS NULL;
UPDATE public.tasks SET pending_origin=NULL
    WHERE stage <> 'pending_input';

ALTER TABLE public.tasks ADD CONSTRAINT tasks_stage_check
    CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'pending_input'::text])));
ALTER TABLE public.tasks ADD CONSTRAINT tasks_pending_origin_set
    CHECK ((stage = 'pending_input'::text) = (pending_origin IS NOT NULL));

ALTER TABLE public.tasks DROP COLUMN IF EXISTS nudge_id;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS nudged_at;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS last_heartbeat_at;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS pending_input;

COMMIT;

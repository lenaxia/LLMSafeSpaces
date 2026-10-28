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

-- Rehome both classes back to the pending_input stage: deliberate parks
-- (flagged, origin preserved) and the attempts-exhaustion terminus
-- (failed + escalated). Belt: any non-pending_input row still carrying
-- an origin loses it (the old constraint requires the paired form —
-- e.g. a manager-moved failed row keeps NULL already; the belt is for
-- anything else).
UPDATE public.tasks SET stage='pending_input'
    WHERE pending_input = true;
UPDATE public.tasks SET stage='pending_input'
    WHERE stage='failed' AND pending_origin='escalated';
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

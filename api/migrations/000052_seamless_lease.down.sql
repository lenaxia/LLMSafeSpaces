-- 000052_seamless_lease (down) — reverse the vocabulary swap, the flag
-- columns, and the nudge slot. Deliberate-park rows return to the
-- pending_input stage; failed/escalated rows return to
-- pending_input/escalated. Nudge state is dropped (it is sweep-local
-- bookkeeping; a downgrade reconciler re-derives nothing from it).

BEGIN;

ALTER TABLE public.tasks DROP CONSTRAINT tasks_pending_input_set;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS nudge_id;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS nudged_at;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS last_heartbeat_at;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS pending_input;

UPDATE public.tasks SET stage='pending_input'
    WHERE stage='failed' AND pending_origin='escalated';

ALTER TABLE public.tasks DROP CONSTRAINT tasks_stage_check;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_stage_check
    CHECK ((stage = ANY (ARRAY['backlog'::text, 'in_progress'::text, 'in_review'::text, 'done'::text, 'pending_input'::text])));
ALTER TABLE public.tasks ADD CONSTRAINT tasks_pending_origin_set
    CHECK ((stage = 'pending_input'::text) = (pending_origin IS NOT NULL));

COMMIT;

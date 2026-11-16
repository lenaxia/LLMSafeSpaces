-- 000059_epic_wrapup_review (down) — drop the wrap-up engine's row
-- shape in reverse: the FK floor, the epic lifecycle columns, the kind
-- index/constraint/column. board_events rows are history and stay.

BEGIN;

ALTER TABLE public.epics DROP CONSTRAINT IF EXISTS epics_wrapup_task_id_fkey;
ALTER TABLE public.epics DROP COLUMN IF EXISTS wrapup_ack_pending;
ALTER TABLE public.epics DROP COLUMN IF EXISTS wrapup_task_id;
ALTER TABLE public.epics DROP COLUMN IF EXISTS wrapup_count;
ALTER TABLE public.epics DROP COLUMN IF EXISTS last_wrapped_at;
ALTER TABLE public.epics DROP COLUMN IF EXISTS wrap_up_review;

DROP INDEX IF EXISTS public.idx_tasks_kind;
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_kind_check;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS kind;

COMMIT;

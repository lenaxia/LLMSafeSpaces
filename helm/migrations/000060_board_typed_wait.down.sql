-- 000060_board_typed_wait (down) — drop the typed-wait columns in
-- reverse (additive-only contract). board_events rows are history and
-- stay (a wait_declared audit trail outlives its row shape).

BEGIN;

ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_wait_on_check;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS wait_reason;
ALTER TABLE public.tasks DROP COLUMN IF EXISTS wait_on;

COMMIT;

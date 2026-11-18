-- 000060_board_typed_wait — typed waits on the claim (claim-wait
-- lifecycle, low-level design §1; the design named this migration
-- 000060 and main has taken nothing past 000059 — no renumber).
--
-- wait_on types the wait a claimed row carries; wait_reason is the
-- short human-readable prose (the textguard seam caps it at 512 bytes
-- at the verb edge — the column itself is uncapped TEXT, the house
-- posture for guarded prose). NULL wait_on = no wait; 'user' is dress
-- on the 000050 pending_input flag (byte-identical semantics — the
-- flag stays the user-wait's storage); 'event' is the NEW state and
-- ALWAYS coexists with pending_input=false.
--
-- NO BACKFILL (the read rule): pre-existing flagged rows render
-- wait_on='user' by COALESCE at the read edge — wait_on IS NULL on
-- every row this migration touches.

BEGIN;

ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS wait_on text;
ALTER TABLE public.tasks DROP CONSTRAINT IF EXISTS tasks_wait_on_check;
ALTER TABLE public.tasks ADD CONSTRAINT tasks_wait_on_check
    CHECK ((wait_on IS NULL OR wait_on = ANY (ARRAY['user'::text, 'event'::text])));
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS wait_reason text;

COMMIT;

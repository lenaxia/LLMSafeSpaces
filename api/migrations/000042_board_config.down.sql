-- Migration 000041 (down) — drop the Task Board Config tables (#1832 item 6).

BEGIN;

DROP TABLE IF EXISTS public.board_project_config_overrides CASCADE;
DROP TABLE IF EXISTS public.board_config_defaults CASCADE;

COMMIT;

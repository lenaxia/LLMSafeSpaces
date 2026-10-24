-- 000049 down: restore the 000042 key vocabulary. Rows carrying the
-- stagePersonas key (if any were written while 000049 was applied)
-- are deleted first — the re-added CHECK would otherwise reject the
-- down-migration itself.

BEGIN;

DELETE FROM public.board_config_defaults WHERE key = 'stagePersonas';
DELETE FROM public.board_project_config_overrides WHERE key = 'stagePersonas';

ALTER TABLE public.board_config_defaults
    DROP CONSTRAINT board_config_defaults_key_check;
ALTER TABLE public.board_config_defaults
    ADD CONSTRAINT board_config_defaults_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'defaultWorkerSlots'::text])));

ALTER TABLE public.board_project_config_overrides
    DROP CONSTRAINT board_project_config_overrides_key_check;
ALTER TABLE public.board_project_config_overrides
    ADD CONSTRAINT board_project_config_overrides_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text])));

COMMIT;

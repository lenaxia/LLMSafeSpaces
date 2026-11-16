-- Down: restore the 000049 key vocabulary (wrapUpReviewer leaves BOTH
-- tiers). Any stored wrapUpReviewer rows must be removed before this
-- runs (the CHECK constraint is the floor); the engine's wrap-up
-- state columns (000056) are untouched by this migration either way.

BEGIN;

ALTER TABLE public.board_config_defaults
    DROP CONSTRAINT board_config_defaults_key_check;
ALTER TABLE public.board_config_defaults
    ADD CONSTRAINT board_config_defaults_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'defaultWorkerSlots'::text, 'stagePersonas'::text])));

ALTER TABLE public.board_project_config_overrides
    DROP CONSTRAINT board_project_config_overrides_key_check;
ALTER TABLE public.board_project_config_overrides
    ADD CONSTRAINT board_project_config_overrides_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'stagePersonas'::text])));

COMMIT;

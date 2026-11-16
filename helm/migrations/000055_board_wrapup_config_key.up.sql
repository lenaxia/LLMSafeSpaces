-- 000055_board_wrapup_config_key — the wrapUpReviewer board-config
-- key (epic wrap-up review, low-level design §1a): the wrap-up
-- reviewer persona slot joins BOTH tiers — user defaults AND
-- per-project overrides. Vocabulary growth only (the 000049 pattern,
-- deliberately its own migration so the key story and the engine
-- story's row-shape migration 000056 can land in either order).
--
-- wrapUpReviewer is a BARE ROLE REFERENCE (id or slug of a platform
-- agent role; the seeded default is the `wrapup-reviewer` platform
-- role). Validation lives in pkg/board/config.go
-- (ValidateBoardConfigKey decodes the PersonaConfig role-ref shapes;
-- validatePersonaRoleRefs resolves the reference at SET time — an
-- unresolved ref refuses the whole write); these CHECK constraints
-- are the physical floor.
--
-- No row-shape change: the value column is jsonb and no existing row
-- changes meaning (a stored config without wrapUpReviewer resolves to
-- the code default — the seeded wrap-up reviewer persona — at merge
-- time). Deliberately NO per-epic tier exists (owner ruling
-- 2026-11-15: the epic's own wrapUpReview flag is the only per-epic
-- control); user → project only, the house ruling from 000042.

BEGIN;

ALTER TABLE public.board_config_defaults
    DROP CONSTRAINT board_config_defaults_key_check;
ALTER TABLE public.board_config_defaults
    ADD CONSTRAINT board_config_defaults_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'defaultWorkerSlots'::text, 'stagePersonas'::text, 'wrapUpReviewer'::text])));

ALTER TABLE public.board_project_config_overrides
    DROP CONSTRAINT board_project_config_overrides_key_check;
ALTER TABLE public.board_project_config_overrides
    ADD CONSTRAINT board_project_config_overrides_key_check
    CHECK ((key = ANY (ARRAY['reviewColumn'::text, 'autoAssignReviewer'::text, 'stagePersonas'::text, 'wrapUpReviewer'::text])));

COMMIT;

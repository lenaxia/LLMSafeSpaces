-- 000049_stage_personas — the stagePersonas board-config key (the
-- generalized stage-persona claim-then-spawn mechanism, owner
-- directive 2026-10-24: "Make it expandable so it works on any
-- arbitrary stage and can accept an agent persona").
--
-- Growing the key set is a deliberate migration (the 000042 header's
-- "room to grow" ruling): stagePersonas (object: stage → {name,
-- instructions?}) joins BOTH tiers — user defaults AND per-project
-- overrides (a project override is the composition lever: {} disables
-- an inherited persona). Validation lives in pkg/board/config.go
-- (decodePersonaMap); these CHECK constraints are the physical floor.
--
-- No schema change beyond the key vocabulary: the value column is
-- jsonb, the row shape is untouched, and no existing row changes
-- meaning (a stored config without stagePersonas resolves to the code
-- default — no personas — byte-identical engine behavior).

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

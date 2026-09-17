-- 0059 (#1425/#1419): trigger input mapping columns.
--
-- input_from selects what a fired run's input is: 'envelope' (the system
-- envelope — pre-0059 behavior, the DEFAULT *is* the backfill), 'body'
-- (webhook only — the posted payload becomes the run input), 'mapped'
-- (the static input document). input is the optional static input
-- document (jsonb NULL = no overlay). Additive + reversible: existing
-- rows read as envelope mode with no UPDATE pass.

BEGIN;

ALTER TABLE triggers ADD COLUMN IF NOT EXISTS input jsonb;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS input_from text NOT NULL DEFAULT 'envelope';

ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_input_from_check;
DO $$ BEGIN
    ALTER TABLE triggers ADD CONSTRAINT triggers_input_from_check
        CHECK (input_from IN ('envelope', 'body', 'mapped'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

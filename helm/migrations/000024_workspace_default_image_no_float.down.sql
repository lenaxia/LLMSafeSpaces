-- Reverse of 000024: restore the pre-fix floating default when the row
-- is missing.
--
-- Rollback behavior note (accurate, 2026-08-15 review): on a rollback,
-- an older API binary re-runs its boot-time settings Seed, which
-- re-inserts the schema-v10 default `base:latest` via
-- InsertInstanceSettingIfMissing BEFORE this statement is reached in
-- any later forward/down cycle — so in the common rollback path this
-- statement is a no-op (the row already exists and ON CONFLICT DO
-- NOTHING skips). It only restores the row on a database restored
-- without a subsequent boot. Harmless either way: a rollback to
-- schema-v10 code deterministically re-seeds the floating default
-- itself, which is the pre-fix behavior a rollback asks for.

BEGIN;

INSERT INTO instance_settings (key, value)
VALUES ('workspace.defaultImage', to_jsonb('ghcr.io/lenaxia/llmsafespaces/base:latest'::text))
ON CONFLICT (key) DO NOTHING;

COMMIT;

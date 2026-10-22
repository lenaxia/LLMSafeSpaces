-- 000042 down: restore the pre-#1831 random-only short-id checks
-- (design 0066's 8-char form). Best-effort — FAILS if phrase-form rows
-- exist; clear them (or restore a backup) before running down.

BEGIN;

ALTER TABLE projects
    DROP CONSTRAINT IF EXISTS projects_short_id_check;
ALTER TABLE projects
    ADD CONSTRAINT projects_short_id_check CHECK ((short_id ~ '^prj-[0-9a-z]{8}$'::text));

ALTER TABLE epics
    DROP CONSTRAINT IF EXISTS epics_short_id_check;
ALTER TABLE epics
    ADD CONSTRAINT epics_short_id_check CHECK ((short_id ~ '^epc-[0-9a-z]{8}$'::text));

ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_short_id_check;
ALTER TABLE tasks
    ADD CONSTRAINT tasks_short_id_check CHECK ((short_id ~ '^tsk-[0-9a-z]{8}$'::text));

COMMIT;

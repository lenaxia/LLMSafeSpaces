-- 000042_board_phrase_ids (issue #1831)
--
-- The board vocabulary flip (session-ID playbook, #1756/#1790 applied
-- to board entities): freshly minted project/epic/task short ids are
-- word phrases over the frozen session lists — prj-/epc-/tsk- prefix
-- (the closed kind map stays) + adjective-noun-verb, e.g.
-- tsk-amber-falcon-glides, with a deterministic counter suffix
-- (-2, -3, …) arbitrating mint collisions.
--
-- The design-0066 random 8-char form (tsk-ab12cd34) is RETIRED from
-- the mint but stays VALID storage during the one-release dual-accept
-- window: rows minted before the flip keep resolving (marked
-- short_id_deprecated on read, never re-minted). The CHECK therefore
-- widens to accept BOTH forms — narrowing again is the window's exit
-- (a later migration, once no legacy-form row remains).
--
-- Down restores the pre-#1831 random-only checks; it FAILS if phrase
-- rows exist (a vocabulary flip's down is best-effort — restore a
-- backup or clear the phrase rows first).

BEGIN;

ALTER TABLE projects
    DROP CONSTRAINT IF EXISTS projects_short_id_check;
ALTER TABLE projects
    ADD CONSTRAINT projects_short_id_check CHECK ((short_id ~ '^prj-([0-9a-z]{8}|[a-z]+-[a-z]+-[a-z]+(-([2-9]|[1-9][0-9]+))?)$'::text));

ALTER TABLE epics
    DROP CONSTRAINT IF EXISTS epics_short_id_check;
ALTER TABLE epics
    ADD CONSTRAINT epics_short_id_check CHECK ((short_id ~ '^epc-([0-9a-z]{8}|[a-z]+-[a-z]+-[a-z]+(-([2-9]|[1-9][0-9]+))?)$'::text));

ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_short_id_check;
ALTER TABLE tasks
    ADD CONSTRAINT tasks_short_id_check CHECK ((short_id ~ '^tsk-([0-9a-z]{8}|[a-z]+-[a-z]+-[a-z]+(-([2-9]|[1-9][0-9]+))?)$'::text));

COMMIT;

-- 000050 down: drop the persona-claim provenance column. No data
-- preservation needed (the mint's audit events keep the persona
-- provenance); the #1946 scoped grant refuses fail-closed without the
-- column and the worker grant/reclaim paths revert to persona-blind
-- rows.

BEGIN;

ALTER TABLE tasks
    DROP COLUMN IF EXISTS claim_persona;

COMMIT;

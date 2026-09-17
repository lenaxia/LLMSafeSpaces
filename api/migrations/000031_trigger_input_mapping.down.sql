-- 0059 down: restore the pre-migration triggers shape. Rows created with
-- input mapping during the deploy window revert to envelope behavior
-- (their input is simply unread) — documented in design/0059 §4.

BEGIN;

ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_input_from_check;
ALTER TABLE triggers DROP COLUMN IF EXISTS input;
ALTER TABLE triggers DROP COLUMN IF EXISTS input_from;

COMMIT;

BEGIN;

ALTER TABLE workflows DROP CONSTRAINT IF EXISTS workflows_on_missing_workspace_chk;
ALTER TABLE workflows DROP COLUMN IF EXISTS on_missing_workspace;

COMMIT;

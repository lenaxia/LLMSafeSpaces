-- Epic 64: on_missing_workspace column on workflows.
--
-- Controls behavior when a workflow's target workspace is gone or never
-- existed at run time. 'abort' (default) fails the run fast.
-- 'create' provisions a new workspace for the workflow owner, pins it
-- as the workflow's target_workspace_id, and proceeds once Active.

BEGIN;

ALTER TABLE workflows ADD COLUMN IF NOT EXISTS on_missing_workspace text NOT NULL DEFAULT 'abort';

DO $$ BEGIN
    ALTER TABLE workflows ADD CONSTRAINT workflows_on_missing_workspace_chk
        CHECK (on_missing_workspace IN ('abort', 'create'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

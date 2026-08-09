BEGIN;

ALTER TABLE trigger_fires DROP CONSTRAINT IF EXISTS trigger_fires_action_type_chk;
ALTER TABLE trigger_fires DROP COLUMN IF EXISTS result;
ALTER TABLE trigger_fires DROP COLUMN IF EXISTS result_captured_at;

-- Restore original action_type constraint
DO $$ BEGIN
    ALTER TABLE trigger_fires ADD CONSTRAINT trigger_fires_action_type_check
        CHECK (action_type IN ('run_workflow', 'run_script'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

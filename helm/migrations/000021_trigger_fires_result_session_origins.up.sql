-- Epic 64 redesign: trigger_fires result column.
-- session_origins table deferred until implementation exists.

BEGIN;

-- trigger_fires: result storage for routine memory + observability
ALTER TABLE trigger_fires ADD COLUMN IF NOT EXISTS result jsonb;
ALTER TABLE trigger_fires ADD COLUMN IF NOT EXISTS result_captured_at timestamptz;

-- Relax action_type CHECK (routines use 'routine' not 'run_script')
ALTER TABLE trigger_fires DROP CONSTRAINT IF EXISTS trigger_fires_action_type_check;
ALTER TABLE trigger_fires DROP CONSTRAINT IF EXISTS trigger_fires_action_type_chk;
DO $$ BEGIN
    ALTER TABLE trigger_fires ADD CONSTRAINT trigger_fires_action_type_chk
        CHECK (action_type IN ('run_workflow', 'routine', 'webhook'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

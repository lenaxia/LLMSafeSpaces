-- Epic 64 redesign: trigger_fires result column + session_origins table.

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

-- Session origin tracking (bridge until unified sessions)
CREATE TABLE IF NOT EXISTS session_origins (
    session_id   text PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       text NOT NULL DEFAULT 'manual',
    trigger_id   uuid REFERENCES triggers(id) ON DELETE SET NULL,
    fire_id      uuid,
    title        text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_origins_workspace
    ON session_origins (workspace_id, created_at DESC);

COMMIT;

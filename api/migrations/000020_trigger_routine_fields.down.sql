BEGIN;

-- Recreate target_type + target_config from backfilled data
ALTER TABLE triggers ADD COLUMN target_type text;
ALTER TABLE triggers ADD COLUMN target_config jsonb DEFAULT '{}'::jsonb;

-- Reconstruct from workflow_id / routine fields
UPDATE triggers
SET target_type = 'run_workflow',
    target_config = jsonb_build_object('workflowId', workflow_id)
WHERE workflow_id IS NOT NULL;

UPDATE triggers
SET target_type = 'run_script',
    target_config = jsonb_build_object(
        'workspaceId', workspace_id,
        'path', script_path,
        'args', to_jsonb(script_args),
        'env', script_env,
        'prompt', prompt
    )
WHERE workflow_id IS NULL AND (script_path != '' OR prompt != '');

DO $$ BEGIN
    ALTER TABLE triggers ADD CONSTRAINT triggers_target_type_check
        CHECK (target_type IN ('run_workflow', 'run_script'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_memory_mode_chk;
ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_capture_mode_chk;
ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_preserve_session_chk;
ALTER TABLE triggers DROP COLUMN IF EXISTS workspace_id;
ALTER TABLE triggers DROP COLUMN IF EXISTS prompt;
ALTER TABLE triggers DROP COLUMN IF EXISTS agent;
ALTER TABLE triggers DROP COLUMN IF EXISTS script_path;
ALTER TABLE triggers DROP COLUMN IF EXISTS script_args;
ALTER TABLE triggers DROP COLUMN IF EXISTS script_env;
ALTER TABLE triggers DROP COLUMN IF EXISTS memory_mode;
ALTER TABLE triggers DROP COLUMN IF EXISTS memory_max_runs;
ALTER TABLE triggers DROP COLUMN IF EXISTS capture_mode;
ALTER TABLE triggers DROP COLUMN IF EXISTS preserve_session;
ALTER TABLE triggers DROP COLUMN IF EXISTS workflow_id;

COMMIT;

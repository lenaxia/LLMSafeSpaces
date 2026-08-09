-- Epic 64 redesign: triggers absorb routine fields.
--
-- Drops target_type + target_config. Adds explicit routine columns
-- (workspace_id, prompt, agent, script_path/args/env, memory, capture,
-- preserve_session) + workflow_id for DAG-mode triggers.
-- Backfills existing data from target_type/target_config before dropping.

BEGIN;

-- 1. Add new columns
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS workspace_id uuid REFERENCES workspaces(id) ON DELETE SET NULL;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS prompt text NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS agent text NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS script_path text NOT NULL DEFAULT '';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS script_args text[] DEFAULT '{}';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS script_env jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS memory_mode text NOT NULL DEFAULT 'none';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS memory_max_runs int NOT NULL DEFAULT 1;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS capture_mode text NOT NULL DEFAULT 'errors_only';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS preserve_session text NOT NULL DEFAULT 'never';
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS workflow_id uuid REFERENCES workflows(id) ON DELETE SET NULL;

-- 2. Backfill workflow_id from run_workflow targets
UPDATE triggers
SET workflow_id = (target_config->>'workflowId')::uuid
WHERE target_type = 'run_workflow'
  AND target_config->>'workflowId' IS NOT NULL;

-- 3. Backfill routine fields from run_script targets
UPDATE triggers
SET workspace_id = (target_config->>'workspaceId')::uuid,
    prompt = COALESCE(target_config->>'prompt', ''),
    script_path = COALESCE(target_config->>'path', ''),
    script_env = COALESCE(target_config->'env', '{}'::jsonb)
WHERE target_type = 'run_script';

-- Backfill script_args from JSON array (can't do inline in UPDATE easily)
UPDATE triggers t
SET script_args = COALESCE(
    ARRAY(SELECT jsonb_array_elements_text(tc->'args') FROM (SELECT target_config AS tc) sub),
    ARRAY[]::text[]
)
WHERE target_type = 'run_script';

-- 4. Add CHECK constraints
DO $$ BEGIN
    ALTER TABLE triggers ADD CONSTRAINT triggers_memory_mode_chk
        CHECK (memory_mode IN ('none', 'last_result'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE triggers ADD CONSTRAINT triggers_capture_mode_chk
        CHECK (capture_mode IN ('errors_only', 'full'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE triggers ADD CONSTRAINT triggers_preserve_session_chk
        CHECK (preserve_session IN ('never', 'always', 'on_failure'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- 5. Drop old columns + constraint
ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_target_type_check;
ALTER TABLE triggers DROP COLUMN IF EXISTS target_type;
ALTER TABLE triggers DROP COLUMN IF EXISTS target_config;

COMMIT;

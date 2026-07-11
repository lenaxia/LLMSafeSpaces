-- Epic: Agent Customization, US-1.1: Rollback platform settings, workspace prompts, org policy keys.

BEGIN;

DROP TABLE IF EXISTS workspace_prompts;
DROP TABLE IF EXISTS platform_settings;

ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_chk;
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_check;
DO $$ BEGIN
    ALTER TABLE org_policies ADD CONSTRAINT org_policies_key_check CHECK (key IN (
        'allowed_models',
        'allowed_providers',
        'max_workspaces_per_member',
        'max_active_workspaces_per_member'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

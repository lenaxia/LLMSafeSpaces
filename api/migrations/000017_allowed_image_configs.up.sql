-- Design/0047 D3: allowed_image_configs org policy key.
--
-- Extends the org_policies CHECK constraint to include the new key.
-- Mirrors the pattern from 000012 (mcp) and 000015 (default_runtime):
-- drop + recreate with the full key list. The DO $$ block handles the
-- duplicate_object case where the constraint name varies between
-- legacy and current naming conventions.

BEGIN;

ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_check;
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_chk;
DO $$ BEGIN
    ALTER TABLE org_policies ADD CONSTRAINT org_policies_key_chk CHECK (key IN (
        'allowed_models',
        'allowed_providers',
        'max_workspaces_per_member',
        'max_active_workspaces_per_member',
        'sys_prompt_org',
        'allow_user_prompt',
        'allow_user_mcp_servers',
        'max_mcp_servers_per_workspace',
        'default_runtime',
        'allow_user_workflow_create',
        'allowed_image_configs'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

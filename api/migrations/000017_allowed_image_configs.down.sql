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
        'allow_user_workflow_create'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

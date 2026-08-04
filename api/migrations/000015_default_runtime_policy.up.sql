-- Add 'default_runtime' to the org_policies CHECK constraint.
-- This extends the established pattern (migration 000012 added agent + MCP
-- keys) to include the org default workspace image (design/0046 launch
-- hierarchy). The value is an image-factory config hash (string), stored
-- in the same jsonb value column as all other policies.
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
        'default_runtime'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

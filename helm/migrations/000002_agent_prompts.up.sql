-- Epic: Agent Customization, US-1.1: Platform settings, workspace prompts, org policy keys.
--
-- Creates:
--   1. platform_settings — platform-wide mutable key/value (base system prompt)
--   2. workspace_prompts — per-workspace user prompt overrides
--   3. Extends org_policies CHECK to add sys_prompt_org and allow_user_prompt
--
-- The platform prompt is stored separately from org_policies because it has no
-- org_id — it applies to all orgs. workspace_prompts stores user-level
-- customization, consulted only when the org's allow_user_prompt is true.

BEGIN;

-- 1. Platform settings table (singleton-style key/value, no org scoping)
CREATE TABLE IF NOT EXISTS platform_settings (
    key         TEXT NOT NULL PRIMARY KEY CHECK (key IN ('sys_prompt_platform')),
    value       JSONB NOT NULL DEFAULT '{}',
    updated_by  TEXT REFERENCES users(id),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. Workspace prompt overrides (user-level customization)
-- agent_role_id will be added in Phase 2 (migration 000003) when agent_roles table exists
CREATE TABLE IF NOT EXISTS workspace_prompts (
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    prompt        TEXT NOT NULL DEFAULT '',
    updated_by    TEXT REFERENCES users(id),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id)
);

-- updated_at triggers (shared function already exists from migration 000006).
DROP TRIGGER IF EXISTS workspace_prompts_updated_at ON workspace_prompts;
CREATE TRIGGER workspace_prompts_updated_at BEFORE UPDATE ON workspace_prompts
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

DROP TRIGGER IF EXISTS platform_settings_updated_at ON platform_settings;
CREATE TRIGGER platform_settings_updated_at BEFORE UPDATE ON platform_settings
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- 3. Extend org_policies CHECK constraint to include prompt keys
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_check;
ALTER TABLE org_policies DROP CONSTRAINT IF EXISTS org_policies_key_chk;
DO $$ BEGIN
    ALTER TABLE org_policies ADD CONSTRAINT org_policies_key_chk CHECK (key IN (
        'allowed_models',
        'allowed_providers',
        'max_workspaces_per_member',
        'max_active_workspaces_per_member',
        'sys_prompt_org',
        'allow_user_prompt'
    ));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

COMMIT;

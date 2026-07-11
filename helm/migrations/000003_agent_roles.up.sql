-- Epic: Agent Customization, US-2.1: Agent roles table.
--
-- Stores named, inheritable agent role definitions scoped at platform or org
-- level. Each role has a versioned JSONB config (system prompt, permissions,
-- model, display metadata) extensible for future tool/MCP support.
--
-- Key design decisions (per design doc §4.3, §5.3-5.6):
--   - extends FK uses ON DELETE RESTRICT (stress test 3.2, 4.1) to prevent
--     silent detachment when a parent role is deleted.
--   - (scope, slug) uniqueness: platform slugs are globally unique, org slugs
--     are unique per org.
--   - is_default unique per org ensures exactly one default org role.
--   - config JSONB with version field for forward-compatible schema evolution.

BEGIN;

CREATE TABLE IF NOT EXISTS agent_roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope       TEXT NOT NULL CHECK (scope IN ('platform', 'org')),
    org_id      UUID REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    extends     UUID REFERENCES agent_roles(id) ON DELETE RESTRICT,
    is_default  BOOLEAN NOT NULL DEFAULT false,
    config      JSONB NOT NULL DEFAULT '{"version":1}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fk_extends_no_self CHECK (id != extends)
);

-- Partial unique indexes: platform slugs globally unique, org slugs unique per
-- org. Implemented as indexes (not table constraints) because PostgreSQL does
-- not support WHERE on UNIQUE table constraints.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_roles_platform_slug
    ON agent_roles(slug) WHERE scope = 'platform';
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_roles_org_slug
    ON agent_roles(org_id, slug) WHERE scope = 'org';

CREATE INDEX IF NOT EXISTS idx_agent_roles_scope ON agent_roles(scope);
CREATE INDEX IF NOT EXISTS idx_agent_roles_org ON agent_roles(org_id) WHERE scope = 'org';
CREATE INDEX IF NOT EXISTS idx_agent_roles_extends ON agent_roles(extends);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_roles_org_default
    ON agent_roles(org_id) WHERE scope = 'org' AND is_default = true;

-- Add agent_role_id column to workspace_prompts (deferred from migration 000002)
ALTER TABLE workspace_prompts
    ADD COLUMN IF NOT EXISTS agent_role_id UUID REFERENCES agent_roles(id) ON DELETE SET NULL;

-- updated_at trigger
DROP TRIGGER IF EXISTS agent_roles_updated_at ON agent_roles;
CREATE TRIGGER agent_roles_updated_at BEFORE UPDATE ON agent_roles
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

COMMIT;

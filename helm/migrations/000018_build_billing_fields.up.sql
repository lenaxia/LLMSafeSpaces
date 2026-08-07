-- Design/0047 Q1: add scope + org_id to image_factory_builds for billing
-- attribution. Org-scoped builds are billed to the org; platform-scoped
-- builds are carried by the platform owner. These columns mirror the
-- config's scope/org_id so billing queries can attribute build cost
-- without joining to the config table.
--
-- Both columns are nullable (backward compatible): existing rows and
-- coalesced builds (where the build was initiated by a different scope)
-- have NULL. The billing query treats NULL scope as "member" (the
-- historical default — all builds were member-scoped before design/0047).

BEGIN;

ALTER TABLE image_factory_builds
    ADD COLUMN IF NOT EXISTS scope text,
    ADD COLUMN IF NOT EXISTS org_id uuid;

-- Backfill existing rows from their config's scope/org_id so historical
-- builds are correctly attributed without a join.
UPDATE image_factory_builds b
SET scope = c.scope, org_id = c.org_id
FROM image_factory_configs c
WHERE b.config_id = c.id AND b.scope IS NULL;

-- Index for billing queries: "give me all builds for org X".
CREATE INDEX IF NOT EXISTS idx_builds_org
    ON image_factory_builds (org_id)
    WHERE org_id IS NOT NULL;

-- Index for platform builds: "how many platform builds this month".
CREATE INDEX IF NOT EXISTS idx_builds_platform
    ON image_factory_builds (started_at)
    WHERE scope = 'platform';

COMMIT;

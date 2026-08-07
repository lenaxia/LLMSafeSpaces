BEGIN;

ALTER TABLE image_factory_builds
    DROP COLUMN IF EXISTS scope,
    DROP COLUMN IF EXISTS org_id;

DROP INDEX IF EXISTS idx_builds_org;
DROP INDEX IF EXISTS idx_builds_platform;

COMMIT;

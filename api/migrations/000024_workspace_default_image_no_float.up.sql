-- Drop the floating-tag default for workspace.defaultImage (incident
-- 2026-08-14). The settings schema seeded Default
-- "ghcr.io/lenaxia/llmsafespaces/base:latest" into instance_settings from
-- v1 through v10. A floating tag resolves per pull — and per puller:
-- registry mirrors (spegel) served a 4-day-old v0.13.0 digest from node
-- caches while upstream latest had moved to v0.15.5, so newly created
-- workspaces silently launched a pre-fix image.
--
-- The schema default is now "" (fall through to the chart-pinned `base`
-- RuntimeEnvironment) and the settings API rejects mutable-tag refs.
-- This migration removes rows still holding the seeded default so reads
-- fall back to the new schema default. Rows an admin deliberately set to
-- anything else are preserved untouched.

BEGIN;

DELETE FROM instance_settings
 WHERE key = 'workspace.defaultImage'
   AND value = to_jsonb('ghcr.io/lenaxia/llmsafespaces/base:latest'::text);

COMMIT;

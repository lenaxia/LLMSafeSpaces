-- Reverse of 000029: drop the secret-delivery revision machinery.

BEGIN;

DROP TABLE IF EXISTS workspace_secret_revisions;

ALTER TABLE mcp_servers DROP COLUMN IF EXISTS version;
ALTER TABLE provider_credentials DROP COLUMN IF EXISTS version;
ALTER TABLE user_secrets DROP COLUMN IF EXISTS version;

COMMIT;

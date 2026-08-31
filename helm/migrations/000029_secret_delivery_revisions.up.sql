-- US-70.2 (#1183): secret delivery revisions, server-side core.
--
-- Per-row value-version counters (create = 1, every value-affecting
-- mutation bumps; distinct from key_version, which tracks the
-- encryption-key generation) plus the per-workspace revision row that
-- mints the monotonic manifest seq. The manifest hash covers binding
-- refs + these version counters and is computed without decrypting
-- anything, so replicas can answer conditional pulls cheaply.
--
-- workspace_secret_revisions.workspace_id is TEXT on purpose: it keys
-- on the workspace identifier the delivery path actually uses — the CR
-- name — not the uuid workspaces.id (the secretautopush
-- "invalid input syntax for type uuid" error-loop is the live
-- counterevidence for keying on uuids).

BEGIN;

ALTER TABLE user_secrets ADD COLUMN version bigint NOT NULL DEFAULT 1;
ALTER TABLE provider_credentials ADD COLUMN version bigint NOT NULL DEFAULT 1;
ALTER TABLE mcp_servers ADD COLUMN version bigint NOT NULL DEFAULT 1;

CREATE TABLE workspace_secret_revisions (
  workspace_id text PRIMARY KEY,
  seq bigint NOT NULL DEFAULT 0,
  manifest_hash text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;

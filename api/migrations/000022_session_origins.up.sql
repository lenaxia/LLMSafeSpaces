-- Epic 64: session origin tracking for sidebar display.
-- Links opencode sessions to their creating trigger/workflow run.

BEGIN;

CREATE TABLE IF NOT EXISTS session_origins (
    session_id   text PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       text NOT NULL DEFAULT 'manual',
    trigger_id   uuid REFERENCES triggers(id) ON DELETE SET NULL,
    fire_id      uuid,
    title        text,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_origins_workspace
    ON session_origins (workspace_id, created_at DESC);

COMMIT;

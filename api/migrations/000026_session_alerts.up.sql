-- D6 (#998): persisted session alerts so hung-session escalations
-- survive SSE disconnects and are readable by workflow surfaces. The
-- API-side escalation sweep (escalateHungs) appends one row per emitted
-- workspace.alert (notify-only, 30min per-workspace cooldown upstream).

CREATE TABLE IF NOT EXISTS public.session_alerts (
    id                  BIGSERIAL PRIMARY KEY,
    workspace_id        TEXT NOT NULL,
    session_id          TEXT NOT NULL,
    alert               TEXT NOT NULL DEFAULT 'session_hung',
    oldest_busy_seconds INT  NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_alerts_ws_created
    ON public.session_alerts (workspace_id, created_at DESC);

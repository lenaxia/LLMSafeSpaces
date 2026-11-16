-- #2047 (parent #2040): the worker_silent arm's persisted task facts.
-- session_alerts rows were session+seconds only — a history read could
-- say a worker was silent but never WHICH slot was held. The alert's
-- facts (task_short_id, project_id, lease_state) ride a nullable JSONB
-- payload; fact-less alerts (session_hung) keep NULL and the same
-- append path.

ALTER TABLE public.session_alerts
    ADD COLUMN IF NOT EXISTS payload JSONB NULL;

-- The session-scoped resolution UPDATE (#2047): worker_silent is
-- SESSION-scoped (a resumed worker resolves their own row, never a
-- sibling silent worker's). Partial index over (workspace_id,
-- session_id) WHERE unresolved keeps that scan trivial — same shape
-- as 000034's workspace-scoped resolution index.
CREATE INDEX IF NOT EXISTS session_alerts_session_unresolved_idx
    ON public.session_alerts (workspace_id, session_id)
    WHERE resolved_at IS NULL;

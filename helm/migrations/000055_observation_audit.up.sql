-- 000055_observation_audit — the observation plane's per-task audit
-- trail (S7, the 2026-10-30 incident remediation: audit visibility).
--
-- The incident's finding: reconstructing what the platform DID — were
-- nudges sent? did lapse verdicts fire? who reclaimed? — required
-- transcript forensics across five sessions, and the orchestrator
-- (no fault of its own) asserted a FALSE "heartbeat-extended" message
-- because the truth was unobservable. S4 put the ladder's decisions on
-- the workspace event bus (ephemeral, consumer-scoped); this table is
-- the DURABLE, orchestrator-readable record of the same decisions plus
-- the ones no bus entry carries (stamps with their lag, depositions
-- with their salvage pointer).
--
-- Deliberately SEPARATE from board_events: that table is the
-- idempotency ledger (requestId-keyed, replay-semantic, SSE-projected)
-- whose rows must never be pruned lightly; renewals there would also
-- flood the SSE projection. This table is a PRUNABLE working window
-- (per-task keep-N + TTL, observation.go's retention boundary): the
-- recent truth online, older rows summarized by the board_events
-- ledger that already carries nudge/wedge/reclaim/depose actions.
--
-- Kinds (observation.go's constants): stamped | nudged | renewed |
-- lapse_verdict | reclaimed | deposed. detail carries the decision's
-- own facts flat (lag_seconds+origin, nudge_id+rung, source, evidence,
-- reason+failure_count, salvage pointer) — JSONB, never queried by key
-- from Go (the read renders it verbatim).

CREATE TABLE IF NOT EXISTS task_observation_events (
    id BIGSERIAL PRIMARY KEY,
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    session TEXT NOT NULL DEFAULT '',
    detail JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-task recent window (the health read's ORDER BY id DESC walks this).
CREATE INDEX IF NOT EXISTS idx_task_observation_events_task
    ON task_observation_events (task_id, id DESC);

-- The TTL prune's scan arm (reconciler's hourly pass).
CREATE INDEX IF NOT EXISTS idx_task_observation_events_created
    ON task_observation_events (created_at);

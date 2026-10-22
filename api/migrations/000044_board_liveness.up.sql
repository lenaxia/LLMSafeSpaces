-- 000044_board_liveness (issue #1847)
--
-- Platform-derived claim liveness, storage side. The liveness of a
-- claim stops being worker-declared (heartbeat timestamps an LLM may
-- hallucinate — the C1 incident) and becomes platform-observed:
--
--   * board_session_liveness — the per-session liveness projection the
--     in-workspace agentd PUSHES over the pod SA-token automation seam
--     (the resource-pressure reporter precedent): busy (an open turn),
--     started (messages>0 / the first turn landed — distinguishes a
--     started session from a stillborn spawn), last_activity_at (the
--     tracker's event watermark — SERVER-clamped at classification
--     time; a future stamp is never "infinitely fresh"), pushed_at
--     (staleness bound: a pod that stops pushing cannot hold a latched
--     busy=true forever — the wedged-turn case releases).
--   * tasks.appeal_until — the pre-reclaim appeal window (#1847 spec
--     item 4/5): one ping to the original session at its turn
--     boundary, then reclaim if silent. NULL = no appeal open.
--   * tasks.lapse_reason — WHY a claim lapsed, recorded by RC1 at
--     appeal/reclaim time (spec item 6's escalation-reason input):
--     'no_assignee' (spawn never stamped), 'session_unknown' (no
--     liveness row — stillborn spawn), 'silent_beyond_grace' (started,
--     idle, activity older than the lease grace), 'stale_liveness'
--     (pushes stopped — pod down/OOM), 'appeal_ignored' (pinged at the
--     turn boundary, no response within the window).
--
-- Backward compatibility: with no liveness rows (agentd N-1 never
-- pushes), every lapsed claim classifies session_unknown and reclaims
-- exactly as before — the rework activates per-pod as agentd rolls.

BEGIN;

CREATE TABLE board_session_liveness (
    workspace_id     uuid        NOT NULL,
    session_id       text        NOT NULL,
    busy             boolean     NOT NULL DEFAULT false,
    started          boolean     NOT NULL DEFAULT false,
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    pushed_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, session_id)
);

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS appeal_until timestamptz,
    ADD COLUMN IF NOT EXISTS lapse_reason text;

COMMIT;

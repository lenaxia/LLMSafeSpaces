-- 000040_task_failed_state (issue #1813)
--
-- The failed-state attribute overlay: a task whose worker attempt
-- FAILED (lease lapse → reclaim; attempts-cap exhaustion) carries a
-- first-class VISIBLE failure overlay — set by the ENGINE, never a
-- stage (the five-stage vocabulary is binding post-#1763).
--   * last_attempt_failed — the overlay marker (UI column, REST/MCP
--     filter surface);
--   * failure_count — CONSECUTIVE failures (doubling backoff driver);
--   * last_failure_at — when the latest failure landed.
-- Truthful-marker note (r9): last_attempt_failed means an attempt
-- actually FAILED — an all-success exhaustion escalates without it.
-- Auto-return-to-ready rides the EXISTING cooldown_until gate: the
-- reclaim sets cooldown_until = now() + doubling backoff (configurable
-- via the failureBackoffSeconds platform knob). Any successful turn
-- (in_review / done entry) and the pending_input human exit reset the
-- overlay with the attempts reset.

BEGIN;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS last_attempt_failed boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS failure_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_failure_at timestamptz;

COMMIT;

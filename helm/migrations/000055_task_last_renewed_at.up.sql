-- 000055_task_last_renewed_at — the two-clock split (#2040 remediation,
-- issue #2043): tasks.last_progress_at meant "some writer touched the
-- row", but the RC1 ladder wrote now() into it on every renewal —
-- renewal was indistinguishable from worker progress, and each renewal
-- deferred wedge/reclaim detection by a full lease window (the
-- 2026-11-15 incident: renewal stamps 00:27/00:32/01:12 read as a
-- progressing worker for hours).
--
-- The split: last_renewed_at is the RENEWAL clock (the platform's
-- evidence-based lease extensions), last_progress_at is the PROGRESS
-- clock (worker-anchored only — claim mints and heartbeat responses).
-- The heartbeat-lapse arm reads the progress clock server-side:
-- now() - last_progress_at > HeartbeatLapseThreshold (default 90m = 2
-- lease windows; NULL = lapsed-by-default — no evidence of worker
-- progress is no movement veto). Full definition:
-- reports/specs/seamless-lease-design.md §The two clocks.
--
-- Backfill (conservative mid-deploy): last_renewed_at initialized from
-- last_progress_at, so in-flight rows carry today's semantics across
-- the deploy — a row whose last touch was its claim mint reads as
-- renewed-at-claim, and the lapse arm first fires only after the
-- threshold has genuinely elapsed with no worker progress. No
-- behavior jump at the deploy boundary.
--
-- The migration cohort note (issue #2043): sibling #2044 adds a
-- no_advance_renewsame cohort column; a single migration carrying both
-- is acceptable — this one lands FIRST in the cohort and stays
-- single-purpose so the sibling can either extend it or ride its own.

BEGIN;

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_renewed_at timestamptz;

-- Conservative backfill: pre-split, every writer of last_progress_at
-- was as good a renewal anchor as any (the incident's ambiguity cut
-- both ways); post-split only worker-anchored writes touch it. Idempotent:
-- only NULL last_renewed_at rows are touched, so a re-run after a
-- partial episode converges without overwriting ladder renewals.
UPDATE tasks SET last_renewed_at = last_progress_at
    WHERE last_renewed_at IS NULL AND last_progress_at IS NOT NULL;

COMMIT;

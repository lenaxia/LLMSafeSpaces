-- 000045_session_index_deleted_at (issue #1882)
--
-- Delete propagation for the sidebar's session index: every
-- platform-initiated session delete (agentd's ephemeral carrier
-- teardown, the engine's authorized deletes) now writes a DURABLE
-- death mark before removing the row. The belt-and-braces read-path
-- filter: ListSessionIndex excludes death-marked rows, so a row whose
-- DELETE failed (or whose effects tail crashed mid-sequence) stops
-- rendering immediately — the ghost-sidebar bug class of #1882 cannot
-- re-present a row the platform knows is dead.
--
-- Row-creating upserts (EnsureSessionRegistered ON CONFLICT DO
-- NOTHING, title/message/parent upserts) never touch deleted_at, so a
-- late pod event cannot resurrect a death-marked row.
--
-- Down is a plain column drop: the mark is write-side bookkeeping
-- only; rows are normally hard-deleted by the same effects tail.

BEGIN;

ALTER TABLE session_index
    ADD COLUMN IF NOT EXISTS deleted_at timestamp with time zone;

COMMIT;

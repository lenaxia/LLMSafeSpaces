-- 000048_session_archive_cascade (issue #1755)
--
-- Archive/unarchive cascades over the parentID subtree. The sticky
-- marker records HOW a session was archived: individually (false —
-- the default, and the conservative reading for every pre-existing
-- archived row) or by a parent's cascade (true). Unarchiving a parent
-- releases only the sessions its cascade archived — a child the user
-- archived individually stays archived across the parent round-trip.
--
-- Semantics (one recursive-CTE UPDATE per direction, enforced in
-- database.SetSessionArchivedStatus):
--   archive(target):   target archived individually; UNARCHIVED
--                      descendants archived with marker=true.
--                      Already-archived descendants are skipped —
--                      that skip preserves individual intent.
--   unarchive(target): target released; descendants released only
--                      when marker=true (cascade-held).
--
-- Down is a plain column drop: the marker is write-side bookkeeping
-- on the API-owned index; archived (000035) is the state that
-- survives.

BEGIN;

ALTER TABLE session_index
    ADD COLUMN IF NOT EXISTS archived_by_cascade boolean NOT NULL DEFAULT false;

COMMIT;

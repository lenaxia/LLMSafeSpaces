-- Copyright (C) 2026 Michael Kao
-- SPDX-License-Identifier: AGPL-3.0-or-later

-- #1850: short_id is WRITE-ONCE. The phrase minted at a session row's
-- birth is its immutable identity (the session-ID playbook); once set
-- it may never be changed, NULL-ed, or re-issued. Pre-#1850 nothing
-- pinned this at the DB level: the only in-code writer was the
-- registration seam's NULL-backfill (WHERE short_id IS NULL), but the
-- lazy-settlement defect class (row death -> phrase-less re-birth ->
-- fresh backfill mint) re-issued identity through exactly that seam.
-- The trigger is the backstop: re-generation of a minted phrase is
-- hard-refused no matter which write path attempts it.
--
-- Semantics:
--   * UPDATE setting short_id to a DIFFERENT value on a row whose
--     short_id is already non-NULL -> refused (write-once).
--   * UPDATE setting short_id to NULL on a minted row -> refused (a
--     phrase may never be un-minted back into the settlement window).
--   * Re-writing the IDENTICAL value -> allowed (idempotent writes).
--   * Backfilling a NULL (legacy pre-#1756 row) -> allowed (the
--     WHERE short_id IS NULL seam keeps working for legacy rows).
--   * INSERTs are untouched (BEFORE UPDATE only): every INSERT leg
--     births the row with its phrase (#1850 persistence-by-construction).

CREATE OR REPLACE FUNCTION public.session_index_short_id_write_once() RETURNS trigger AS $$
BEGIN
    IF OLD.short_id IS NOT NULL AND NEW.short_id IS DISTINCT FROM OLD.short_id THEN
        RAISE EXCEPTION 'session_index.short_id is write-once (workspace %, session %): phrase % cannot become %',
            OLD.workspace_id, OLD.session_id, OLD.short_id, NEW.short_id
            USING ERRCODE = 'P0001',
                  TABLE = 'session_index',
                  COLUMN = 'short_id';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_session_index_short_id_write_once ON public.session_index;

CREATE TRIGGER trg_session_index_short_id_write_once
    BEFORE UPDATE OF short_id ON public.session_index
    FOR EACH ROW
    WHEN (OLD.short_id IS NOT NULL)
    EXECUTE FUNCTION public.session_index_short_id_write_once();

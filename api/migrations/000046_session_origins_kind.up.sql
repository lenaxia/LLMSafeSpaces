-- 000046_session_origins_kind (issue #1901)
--
-- Reap discriminator for the #1882 carrier bypass. The blanket "any
-- session_origins mark admits the reap" verdict could not tell a
-- platform-deleted ephemeral CARRIER (reap-admissible: its absence
-- from the harness is platform policy, not a store reset) from a
-- PRESERVED session's persistent mark (PreserveAlways /
-- PreserveOnFailure-failed — a harness store reset is exactly the
-- kept_for_operator scenario for it, and the blanket bypass would
-- destroy its history row). kind is written by the only component
-- that knows the session's fate at write time: the engine's carrier
-- mark path writes 'carrier', the preserved-session path writes
-- 'session'.
--
-- Backfill: DEFAULT 'session' is the fail-safe verdict — every
-- pre-migration surviving mark IS a preserved session's (carrier
-- marks are cleaned post-effects). Accepted residual (#1901
-- orchestrator ack): pre-migration carrier marks that outlived their
-- cleanup stop being reaped (stale ghosts until manual/aging cleanup)
-- — the safe direction; deletion is irreversible.

BEGIN;

ALTER TABLE session_origins
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'session';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'session_origins_kind_check'
    ) THEN
        ALTER TABLE session_origins
            ADD CONSTRAINT session_origins_kind_check
            CHECK (kind IN ('carrier', 'session'));
    END IF;
END $$;

COMMIT;

-- 000056 down: drop the payload facts column and the session-scoped
-- resolution index. Existing worker_silent rows degrade to the
-- fact-less shape (alert + oldest_busy_seconds) — no data loss beyond
-- the facts themselves.

DROP INDEX IF EXISTS public.session_alerts_session_unresolved_idx;
ALTER TABLE public.session_alerts DROP COLUMN IF EXISTS payload;

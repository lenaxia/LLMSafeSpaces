-- 000047_session_phrase_ledger (issue #1889, F1) — down
--
-- The ledger is convergence bookkeeping only: dropping it returns the
-- mint seams to fresh-mint-on-resurrection (the pre-#1889 posture);
-- session_index rows and their phrases are untouched.

BEGIN;

DROP TABLE IF EXISTS public.session_phrase_ledger;

COMMIT;

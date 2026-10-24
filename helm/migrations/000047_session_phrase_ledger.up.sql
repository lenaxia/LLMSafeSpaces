-- 000047_session_phrase_ledger (issue #1889, F1)
--
-- Durable phrase binding surviving session_index row death. The #1850
-- belts (#1881 persistence-by-construction, #1865 reap by-ID
-- confirmation) closed the LIVE-reap re-phrase holes, but the binding
-- itself still lived and died with the session_index row: any row death
-- a session could legitimately come back from (hard delete with late
-- pod events still in flight, ghost reap post-grace, a missed delete
-- propagation) destroyed the (session → phrase) mapping, and the
-- resurrection minted a FRESH phrase — every observer of the original
-- phrase watched it stop resolving while the session lived on under a
-- new identity.
--
-- The ledger is keyed INDEPENDENTLY of the row: one row per
-- (workspace_id, session_id), holding the phrase the session's row was
-- born under. The mint seams consult it before minting; a re-born row
-- CONVERGES on its original phrase instead of a fresh mint.
--
-- Design decisions (the issue's "design work needed" items):
--   * uniqueness: the PK is (workspace_id, session_id) ONLY. A
--     (workspace_id, short_id) unique index is deliberately absent:
--     after a row dies its phrase is free for a NEW session to mint
--     (the session_index partial unique index arbitrates live rows);
--     enforcing ledger-level phrase uniqueness would mis-refuse that
--     legitimate re-mint.
--   * reaping semantics: ledger rows are never deleted by session
--     reaps/hard deletes — that indestructibility IS the feature. A
--     ledger phrase that collides with a re-minted live row on
--     resurrection re-mints deterministically (the ruling-3 suffix
--     discipline, one level up).
--   * retention: rows live as long as the workspace (no FK cascade —
--     session_index itself is deliberately un-FK'd text; the workspace
--     reset path clears the ledger explicitly in DeleteSessionIndex).
--
-- Backfill: every live minted row becomes ledger-known at cutover, so
-- a claim on a pre-ledger row can never outrank its real phrase.

BEGIN;

CREATE TABLE IF NOT EXISTS public.session_phrase_ledger (
    workspace_id text NOT NULL,
    session_id text NOT NULL,
    short_id text NOT NULL,
    minted_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (workspace_id, session_id)
);

INSERT INTO public.session_phrase_ledger (workspace_id, session_id, short_id, minted_at)
SELECT workspace_id, session_id, short_id, COALESCE(created_at, now())
FROM public.session_index
WHERE short_id IS NOT NULL
ON CONFLICT (workspace_id, session_id) DO NOTHING;

COMMIT;

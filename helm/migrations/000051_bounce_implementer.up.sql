-- 000051_bounce_implementer (issue #1959, owner ruling 2026-10-26)
--
-- The bounce-consumer identity column: tasks.implementer_session records
-- the session that moved the row backlog→…→in_progress→in_review — the
-- ORIGINAL WORKER the ruling's bounce re-dispatches ("the session that
-- did the work, which holds the context").
--
-- Why a second column: assignee_session serves TWO masters with
-- conflicting lifecycles. It is the LIVE-CLAIM identity (the #1832 WS4
-- stillborn heartbeat guard, the pause/abort addressing) — so the
-- reviewer persona's spawn-stamp OVERWRITES it, and the #1947 rc1-parity
-- lapse pass CLEARS it on every reviewer lease death. But it was ALSO
-- the bounce guard's never-token-less input — and the reviewer lifecycle
-- poisons it for that consumer twice over:
--
--   1. the mint→stamp window: a re-armed (or first-round) reviewer
--      persona claim mints with assignee_session NULL (the lapse cleared
--      it, or the implementer's own stamp never landed); the reviewer's
--      ASYNC spawn-stamp lands ~90s later, and a prompt reviewer ALWAYS
--      beats its own stamp — live 2026-10-25, tsk-orient-songbird-wiggles:
--      cloven-loggia-snorts verdicted 23:27:13 (stamp pending),
--      drafty-persimmon-generates verdicted 23:46:05, stamped 23:46:37.
--      The guard read "" and refused both (applied:false,
--      refused:stage_transition — the #1959 defect).
--   2. even post-stamp, the column holds the REVIEWER's session, not the
--      implementer's — the "bounce consumer" identity the forward exit
--      thought it retained was silently reassigned.
--
-- implementer_session is captured ONCE per round at the in_progress→
-- in_review forward exit (=assignee_session, the worker that did the
-- work) and survives the whole review lifecycle — the reviewer mint
-- preserves it (personaClaimSQL touches only the claim columns), the
-- #1947 lapse pass clears only the dead reviewer's claim identity. The
-- bounce reads IT, not assignee_session: the never-token-less invariant
-- holds by construction, and the re-dispatch targets the original worker
-- instead of refusing (or worse, re-claiming the reviewer).
--
-- Cleared on every NEW episode (worker grant, manual start, pending-input
-- exits, throttle requeue, in_progress lapse) exactly where
-- assignee_session is cleared there — except the in_review persona lapse,
-- which must preserve it (that preservation IS the fix). Not exposed on
-- read edges (internal engine addressing; no phrase-translation debt).
--
-- Backfill: pre-migration in_review rows whose retained assignee IS the
-- implementer (no live persona claim — with one, the assignee is the
-- reviewer's or the window's NULL and the bounce correctly falls back to
-- a fresh-implementer spawn until the next round captures it).

BEGIN;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS implementer_session text;

UPDATE tasks
   SET implementer_session = assignee_session
 WHERE stage = 'in_review'
   AND implementer_session IS NULL
   AND claim_persona IS NULL
   AND assignee_session IS NOT NULL;

COMMIT;

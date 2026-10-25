-- 000050_reviewer_criteria_met (issue #1946, owner ruling 2026-10-25)
--
-- The persona-claim PROVENANCE column: tasks.claim_persona records the
-- configured persona name whose mint holds the row's live claim. The
-- #1941 stage-persona mint already carries provenance in its audit
-- event and the spawned charter envelope; this column puts it where
-- the server-side guards can arbitrate it — the seam where a
-- reviewer-persona claim is distinguishable from a task-worker claim
-- (the two mint classes are otherwise identical rows).
--
-- Why it matters (#1946): ruling #1747 §5's blanket surface separation
-- caught the reviewer persona in the task-worker net, so auto-review
-- could verify acceptance criteria but never RECORD them met
-- (setCriteriaMet was manager-surface-only) — the loop could not
-- self-complete. The owner ruling ("Why is set criteria met manager
-- only. It should not be.") grants reviewer-persona claims
-- setCriteriaMet on their OWN claimed row; task-worker
-- self-certification stays forbidden. The grant's guard reads this
-- column: claim_persona IS NOT NULL on an in_review row means the live
-- token was minted by a persona episode (the ONLY in_review mint), so
-- the #1946 worker-surface criteria write can trust it.
--
-- NULL on every ordinary claim (worker grant, bounce re-claim,
-- pending-input exit re-claim — the engine's non-persona mints set it
-- NULL) and cleared wherever claim_token is cleared. No existing row
-- changes meaning: every live claim today is a worker claim, and NULL
-- is its value.

BEGIN;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS claim_persona text;

COMMIT;

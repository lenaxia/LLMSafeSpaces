-- US-70.4 (#1208): re-wrap reconciler retention columns.
--
-- The login-independent re-wrap reconciler walks user_keys and heals
-- rows the current master provider cannot unwrap (the 2026-08-28
-- incident class: a June-era wrap the login-gated migration never
-- touched). Blast-radius controls need three shadow columns:
--
--   wrapped_dek_previous             — the pre-heal wrap, retained AS
--                                       CIPHERTEXT re-wrapped under the
--                                       CURRENT KEK (W10: reversible with
--                                       current keys, zero plaintext at
--                                       rest)
--   wrapped_dek_previous_kek_version — the KEK version the retained wrap
--                                       is re-wrapped under
--   wrapped_dek_retained_until       — retention deadline (30d from the
--                                       heal); the pass NULLs the three
--                                       columns once it passes
--
--   updated_at                       — mutable ordering timestamp for the
--                                       oldest-first walk (oldest =
--                                       highest risk first). Backfilled
--                                       from COALESCE(rotated_at,
--                                       created_at) so June-era rows sort
--                                       ahead of rows touched yesterday;
--                                       every CAS heal stamps now(),
--                                       moving healed rows to the end of
--                                       the walk (they are healthy).
--
-- Idempotent per the repo convention (IF NOT EXISTS / WHERE IS NULL).

BEGIN;

ALTER TABLE user_keys ADD COLUMN IF NOT EXISTS wrapped_dek_previous bytea;
ALTER TABLE user_keys ADD COLUMN IF NOT EXISTS wrapped_dek_previous_kek_version integer;
ALTER TABLE user_keys ADD COLUMN IF NOT EXISTS wrapped_dek_retained_until timestamptz;
ALTER TABLE user_keys ADD COLUMN IF NOT EXISTS updated_at timestamptz;

UPDATE user_keys
   SET updated_at = COALESCE(rotated_at, created_at)
 WHERE updated_at IS NULL;

ALTER TABLE user_keys ALTER COLUMN updated_at SET NOT NULL;
ALTER TABLE user_keys ALTER COLUMN updated_at SET DEFAULT now();

COMMIT;

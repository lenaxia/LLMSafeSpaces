-- Reverse of 000030: drop the re-wrap reconciler retention columns.
--
-- The retained previous wraps are ciphertext under the current KEK by
-- construction (W10); dropping the columns discards the reversal window
-- for any heal performed while they existed. Not meaningfully
-- reversible after that.

BEGIN;

ALTER TABLE user_keys DROP COLUMN IF EXISTS updated_at;
ALTER TABLE user_keys DROP COLUMN IF EXISTS wrapped_dek_retained_until;
ALTER TABLE user_keys DROP COLUMN IF EXISTS wrapped_dek_previous_kek_version;
ALTER TABLE user_keys DROP COLUMN IF EXISTS wrapped_dek_previous;

COMMIT;

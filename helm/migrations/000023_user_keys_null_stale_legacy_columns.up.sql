-- Null out legacy password-tier residue on server_kek/passkey rows.
--
-- The migrate-passkey-dek CLI (worklog 0662) re-wrapped each user's DEK
-- from password-derived to server-KEK, but only touched wrapped_dek /
-- key_version / dek_source. The salt, wrapped_dek_recovery, and
-- recovery_salt columns — meaningful only under the removed password
-- tier — were left carrying orphan bytes from the legacy wrap.
--
-- Fresh InitializeUserKeysServerKEK writes (key_service.go) set these
-- NULL. This migration brings the two pre-existing prod rows in line
-- with that invariant. Idempotent: re-running is a no-op once the
-- columns are already NULL.
--
-- Not meaningfully reversible — the legacy bytes are gone once nulled.
-- See the down file for the rationale.

UPDATE user_keys
   SET salt = NULL,
       wrapped_dek_recovery = NULL,
       recovery_salt = NULL
 WHERE user_id IN (SELECT id FROM users WHERE dek_source IN ('server_kek', 'passkey'));

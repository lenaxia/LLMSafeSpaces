// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgKeyStore implements KeyStore using PostgreSQL.
type PgKeyStore struct {
	pool *pgxpool.Pool
}

// NewPgKeyStore creates a new PostgreSQL-backed key store.
func NewPgKeyStore(pool *pgxpool.Pool) *PgKeyStore {
	return &PgKeyStore{pool: pool}
}

func (s *PgKeyStore) GetUserKey(ctx context.Context, userID string) (*UserKeyRecord, error) {
	// users.dek_source is JOINed so the caller (KeyService unlock/provisioning
	// paths) knows which unwrap method to use without a second round-trip.
	row := s.pool.QueryRow(ctx,
		`SELECT k.user_id, k.key_version, k.wrapped_dek, k.wrapped_dek_recovery, k.salt, k.recovery_salt, k.created_at, k.rotated_at,
		        COALESCE(u.dek_source, 'password')
		 FROM user_keys k
		 JOIN users u ON u.id = k.user_id
		 WHERE k.user_id = $1`, userID)

	var r UserKeyRecord
	err := row.Scan(&r.UserID, &r.KeyVersion, &r.WrappedDEK, &r.WrappedDEKRecovery, &r.Salt, &r.RecoverySalt, &r.CreatedAt, &r.RotatedAt, &r.DEKSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query user_keys: %w", err)
	}
	return &r, nil
}

// CreateUserKey stores a user's key material. On conflict (the user
// already has a row — e.g. password reset reinitializing a fresh DEK
// for a user who already has key material), the row is overwritten.
// This is required because user_keys.user_id is the PRIMARY KEY and a
// plain INSERT would fail with unique_violation for any user who has
// ever created a secret, which is the only case where reinit matters.
// Overwriting with a freshly-generated DEK (see InitializeUserKeys)
// is exactly the desired reset behavior: the prior wraps and anything
// encrypted under the prior DEK become permanently undecryptable.
//
// When record.DEKSource == "server_kek" (Epic 58 SSO provisioning /
// Epic 59 passkey provisioning), the users.dek_source flag is flipped
// to 'server_kek' in the SAME transaction as the user_keys insert. The
// atomicity is load-bearing: if the two writes split, a later GetUserKey
// JOIN could return a record whose dek_source disagrees with the actual
// wrap, making the next unlock pick the wrong unwrap method and fail.
func (s *PgKeyStore) CreateUserKey(ctx context.Context, record *UserKeyRecord) error {
	const insertStmt = `INSERT INTO user_keys (user_id, key_version, wrapped_dek, wrapped_dek_recovery, salt, recovery_salt, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id) DO UPDATE SET
		   key_version = EXCLUDED.key_version,
		   wrapped_dek = EXCLUDED.wrapped_dek,
		   wrapped_dek_recovery = EXCLUDED.wrapped_dek_recovery,
		   salt = EXCLUDED.salt,
		   recovery_salt = EXCLUDED.recovery_salt,
		   created_at = EXCLUDED.created_at,
		   rotated_at = NULL`

	if dekSourceIsServerWrapped(record.DEKSource) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin server-kek provisioning tx: %w", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, insertStmt,
			record.UserID, record.KeyVersion, record.WrappedDEK, record.WrappedDEKRecovery, record.Salt, record.RecoverySalt, record.CreatedAt); err != nil {
			return fmt.Errorf("upsert user_keys: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET dek_source = $2 WHERE id = $1`, record.UserID, record.DEKSource); err != nil {
			return fmt.Errorf("set dek_source: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit server-kek provisioning: %w", err)
		}
		return nil
	}

	_, err := s.pool.Exec(ctx, insertStmt,
		record.UserID, record.KeyVersion, record.WrappedDEK, record.WrappedDEKRecovery, record.Salt, record.RecoverySalt, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert user_keys: %w", err)
	}
	return nil
}

// UpdateWrappedDEK updates the wrapped DEK for a user. When the
// context carries an active *pgx.Tx (threaded through by
// SecretStore.ReEncryptUserSecrets via withTx), the UPDATE runs inside
// that transaction so the user_keys row and the user_secrets re-encrypt
// commit or roll back atomically. Otherwise the UPDATE runs on the
// pool directly. See Bug 9 in worklog 0094.
func (s *PgKeyStore) UpdateWrappedDEK(ctx context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	const sqlStmt = `UPDATE user_keys SET wrapped_dek = $1, salt = $2, key_version = $3, rotated_at = NOW() WHERE user_id = $4`
	if tx := txFromContext(ctx); tx != nil {
		if _, err := tx.Exec(ctx, sqlStmt, wrappedDEK, salt, keyVersion, userID); err != nil {
			return fmt.Errorf("update wrapped_dek (tx): %w", err)
		}
		return nil
	}
	if _, err := s.pool.Exec(ctx, sqlStmt, wrappedDEK, salt, keyVersion, userID); err != nil {
		return fmt.Errorf("update wrapped_dek: %w", err)
	}
	return nil
}

// UpdateWrappedDEKRecovery updates the recovery-key wrap. Like
// UpdateWrappedDEK, the implementation honors an active *pgx.Tx
// threaded through the context (via withTx) so future callers that
// want to bundle a recovery-key rotation into the same atomic unit as
// the password-key rotation can do so. No current caller does, but
// the parity with UpdateWrappedDEK closes a latent footgun.
func (s *PgKeyStore) UpdateWrappedDEKRecovery(ctx context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	const sqlStmt = `UPDATE user_keys SET wrapped_dek_recovery = $1, recovery_salt = $2 WHERE user_id = $3`
	if tx := txFromContext(ctx); tx != nil {
		if _, err := tx.Exec(ctx, sqlStmt, wrappedDEKRecovery, recoverySalt, userID); err != nil {
			return fmt.Errorf("update wrapped_dek_recovery (tx): %w", err)
		}
		return nil
	}
	if _, err := s.pool.Exec(ctx, sqlStmt, wrappedDEKRecovery, recoverySalt, userID); err != nil {
		return fmt.Errorf("update wrapped_dek_recovery: %w", err)
	}
	return nil
}

// UpdateWrappedDEKAndSource atomically re-wraps the DEK AND flips
// users.dek_source in a single transaction. Used by KeyService.ChangePassword
// when a server_kek-tier user sets their first password (opt-up to the stronger
// password tier). Like the other update methods, an active *pgx.Tx threaded
// through the context is honored; otherwise a fresh transaction is owned here.
func (s *PgKeyStore) UpdateWrappedDEKAndSource(ctx context.Context, userID string, wrappedDEK, salt []byte, keyVersion int, dekSource string) error {
	const (
		updateKeys = `UPDATE user_keys SET wrapped_dek = $1, salt = $2, key_version = $3, rotated_at = NOW() WHERE user_id = $4`
		updateUser = `UPDATE users SET dek_source = $2 WHERE id = $1`
	)
	if tx := txFromContext(ctx); tx != nil {
		if _, err := tx.Exec(ctx, updateKeys, wrappedDEK, salt, keyVersion, userID); err != nil {
			return fmt.Errorf("update wrapped_dek (tx): %w", err)
		}
		if _, err := tx.Exec(ctx, updateUser, userID, dekSource); err != nil {
			return fmt.Errorf("set dek_source (tx): %w", err)
		}
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin dek-source transition tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, updateKeys, wrappedDEK, salt, keyVersion, userID); err != nil {
		return fmt.Errorf("update wrapped_dek: %w", err)
	}
	if _, err := tx.Exec(ctx, updateUser, userID, dekSource); err != nil {
		return fmt.Errorf("set dek_source: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dek-source transition: %w", err)
	}
	return nil
}

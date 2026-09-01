// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// The column is NOT NULL DEFAULT 'server_kek' post-migration 000014; the
	// COALESCE never fires today but keeps the query defensive.
	row := s.pool.QueryRow(ctx,
		`SELECT k.user_id, k.key_version, k.wrapped_dek, k.wrapped_dek_recovery, k.salt, k.recovery_salt, k.created_at, k.rotated_at,
		        COALESCE(u.dek_source, 'server_kek')
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
// already has a row), the row is overwritten — user_keys.user_id is the
// PRIMARY KEY and a plain INSERT would fail with unique_violation.
// Overwriting with a freshly-generated DEK (see InitializeUserKeysServerKEK)
// is exactly the desired reset behavior: the prior wraps and anything
// encrypted under the prior DEK become permanently undecryptable.
//
// When record.DEKSource == "server_kek" (SSO provisioning, Epic 58) or
// "passkey" (passkey-only login, Epic 59), the users.dek_source flag is
// flipped in the SAME transaction as the user_keys insert. The atomicity
// is load-bearing: if the two writes split, a later GetUserKey JOIN could
// return a record whose dek_source disagrees with the actual wrap, making
// the next unlock pick the wrong unwrap method and fail.
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

// ListUserKeysForReconcile returns one oldest-first walk window of
// user_keys (US-70.4 ReconcileKeyStore). Oldest = highest risk: rows
// untouched the longest predate the most KEK rotation windows, which is
// exactly the mike-class incident shape. updated_at is stamped by every
// CAS heal, moving healed rows to the END of the walk (they are
// healthy); the user_id tiebreak keeps the ordering deterministic for
// offset pagination.
func (s *PgKeyStore) ListUserKeysForReconcile(ctx context.Context, limit, offset int) ([]UserKeyReconcileRow, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive, got %d", limit)
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset must be non-negative, got %d", offset)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, key_version, wrapped_dek, wrapped_dek_previous, wrapped_dek_previous_kek_version, wrapped_dek_retained_until, updated_at
		   FROM user_keys
		  ORDER BY updated_at ASC, user_id ASC
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list user_keys for reconcile: %w", err)
	}
	defer rows.Close()

	var out []UserKeyReconcileRow
	for rows.Next() {
		var r UserKeyReconcileRow
		if err := rows.Scan(&r.UserID, &r.KeyVersion, &r.WrappedDEK, &r.WrappedDEKPrevious,
			&r.WrappedDEKPreviousKEKVersion, &r.WrappedDEKRetainedUntil, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user_keys reconcile row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompareAndSwapWrappedDEK conditionally rewrites the row's active wrap
// (US-70.4 ReconcileKeyStore). The UPDATE applies only when the row's
// current wrapped_dek byte-equals expectedWrapped — the guard against a
// concurrent legitimate rotation (user rotates while the reconciler
// heals): the loser sees won=false with nil error and backs off, never
// retries. On win, the row gains the retained previous wrap (W10: old
// wrap bytes as ciphertext under the CURRENT KEK) with its retention
// deadline, and updated_at=now() moves the row to the end of the walk
// order. rotated_at is stamped alongside — a heal is a wrap rotation in
// UpdateWrappedDEK's sense, and the column feeds the migration-000030
// updated_at backfill semantics.
func (s *PgKeyStore) CompareAndSwapWrappedDEK(ctx context.Context, userID string, expectedWrapped, newWrapped []byte, newVersion int, previous *RetainedWrap) (bool, error) {
	var prevCT []byte
	var prevVer *int
	var prevUntil *time.Time
	if previous != nil {
		prevCT = previous.Ciphertext
		v := previous.KEKVersion
		prevVer = &v
		u := previous.Until
		prevUntil = &u
	}

	// wrapped_dek = $5 compares bytea byte-for-byte — the CAS precondition.
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_keys
		    SET wrapped_dek = $2,
		        key_version = $3,
		        wrapped_dek_previous = $4,
		        wrapped_dek_previous_kek_version = $5,
		        wrapped_dek_retained_until = $6,
		        updated_at = NOW(),
		        rotated_at = NOW()
		  WHERE user_id = $1
		    AND wrapped_dek = $7`,
		userID, newWrapped, newVersion, prevCT, prevVer, prevUntil, expectedWrapped)
	if err != nil {
		return false, fmt.Errorf("compare-and-swap wrapped_dek: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, nil
}

// DeleteExpiredRetainedWraps NULLs the previous-wrap columns of every
// row past its retention deadline (US-70.4 ReconcileKeyStore). The
// active wrap is untouched and updated_at is deliberately NOT bumped —
// the walk ordering tracks active-wrap lifecycle, not retention
// bookkeeping. Returns the number of rows cleaned.
func (s *PgKeyStore) DeleteExpiredRetainedWraps(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_keys
		    SET wrapped_dek_previous = NULL,
		        wrapped_dek_previous_kek_version = NULL,
		        wrapped_dek_retained_until = NULL
		  WHERE wrapped_dek_retained_until IS NOT NULL
		    AND wrapped_dek_retained_until <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired retained wraps: %w", err)
	}
	return tag.RowsAffected(), nil
}

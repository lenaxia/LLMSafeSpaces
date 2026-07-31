// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore implements Store against PostgreSQL. It is a thin wrapper; the
// queries are straightforward SELECT/INSERT/UPDATE over user_passkeys and
// user_recovery_codes (migrations 000009/000010).
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore constructs a Postgres-backed passkey store.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) ListCredentials(ctx context.Context, userID string) ([]Credential, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, credential_id, public_key, attestation_type, attestation_format, aaguid,
		        sign_count, transports, name, created_at, last_used_at
		 FROM user_passkeys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query user_passkeys: %w", err)
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PgStore) GetCredentialByCredentialID(ctx context.Context, credentialID []byte) (*Credential, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, user_id, credential_id, public_key, attestation_type, attestation_format, aaguid,
		        sign_count, transports, name, created_at, last_used_at
		 FROM user_passkeys WHERE credential_id = $1`, credentialID)
	c, err := scanCredential(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) CreateCredential(ctx context.Context, c *Credential) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_passkeys
		   (id, user_id, credential_id, public_key, attestation_type, attestation_format, aaguid, sign_count, transports, name, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, COALESCE($11, now()))`,
		c.ID, c.UserID, c.CredentialID, c.PublicKey, c.AttestationType, c.AttestationFormat, c.AAGUID,
		c.SignCount, c.Transports, c.Name, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert user_passkeys: %w", err)
	}
	return nil
}

// CreateCredentialAndRecoveryCodes atomically persists the credential AND the
// recovery-code hashes in a single transaction. Partial failure rolls back
// both — a passkey-only user always gets either (credential + recovery codes)
// or neither.
func (s *PgStore) CreateCredentialAndRecoveryCodes(ctx context.Context, cred *Credential, hashes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin credential+recovery tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_passkeys
		   (id, user_id, credential_id, public_key, attestation_type, attestation_format, aaguid, sign_count, transports, name, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, COALESCE($11, now()))`,
		cred.ID, cred.UserID, cred.CredentialID, cred.PublicKey, cred.AttestationType, cred.AttestationFormat, cred.AAGUID,
		cred.SignCount, cred.Transports, cred.Name, cred.CreatedAt); err != nil {
		return fmt.Errorf("insert user_passkeys: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_recovery_codes (id, user_id, code_hash) VALUES ($1, $2, $3)`,
			uuid.New(), cred.UserID, h); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit credential+recovery: %w", err)
	}
	return nil
}

func (s *PgStore) UpdateCredentialAfterLogin(ctx context.Context, id uuid.UUID, signCount uint32, lastUsedAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE user_passkeys SET sign_count = $1, last_used_at = $2 WHERE id = $3`,
		signCount, lastUsedAt, id)
	if err != nil {
		return fmt.Errorf("update user_passkeys sign_count: %w", err)
	}
	return nil
}

func (s *PgStore) DeleteCredential(ctx context.Context, userID string, id uuid.UUID) error {
	// Last-credential guard: refuse to delete if it would leave the user with
	// zero passkeys (a passkey-only user would be permanently locked out).
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM user_passkeys WHERE id = $1 AND user_id = $2
		 AND (SELECT count(*) FROM user_passkeys WHERE user_id = $2) > 1`,
		id, userID)
	if err != nil {
		return fmt.Errorf("delete user_passkeys: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row doesn't exist / isn't owned, or it was the last one.
		var exists bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_passkeys WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return ErrCredentialNotFound
		}
		return ErrLastCredential
	}
	return nil
}

func (s *PgStore) CountCredentials(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_passkeys WHERE user_id = $1`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count user_passkeys: %w", err)
	}
	return n, nil
}

func (s *PgStore) CreateRecoveryCodes(ctx context.Context, userID string, hashes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin recovery-codes tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Re-enrollment invalidates prior unused codes for the user (delete unused,
	// keep consumed rows for audit).
	if _, err := tx.Exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID); err != nil {
		return fmt.Errorf("clear unused recovery codes: %w", err)
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_recovery_codes (id, user_id, code_hash) VALUES ($1, $2, $3)`,
			uuid.New(), userID, h); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (s *PgStore) ListAvailableRecoveryCodes(ctx context.Context, userID string) ([]RecoveryCode, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, code_hash, used_at, created_at
		 FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID)
	if err != nil {
		return nil, fmt.Errorf("query recovery codes: %w", err)
	}
	defer rows.Close()
	var out []RecoveryCode
	for rows.Next() {
		var r RecoveryCode
		if err := rows.Scan(&r.ID, &r.UserID, &r.CodeHash, &r.UsedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PgStore) ConsumeRecoveryCode(ctx context.Context, userID, codeHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_recovery_codes SET used_at = now()
		 WHERE id = (SELECT id FROM user_recovery_codes WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL LIMIT 1)`,
		userID, codeHash)
	if err != nil {
		return fmt.Errorf("consume recovery code: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRecoveryCodeNotFound
	}
	return nil
}

// scanner abstracts pgx.Row and pgx.Rows so the same scan helper serves the
// single-row (QueryRow) and multi-row (Query) paths.
type scanner interface {
	Scan(dest ...any) error
}

func scanCredential(sc scanner) (Credential, error) {
	var c Credential
	if err := sc.Scan(
		&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &c.AttestationType, &c.AttestationFormat,
		&c.AAGUID, &c.SignCount, &c.Transports, &c.Name, &c.CreatedAt, &c.LastUsedAt,
	); err != nil {
		return Credential{}, fmt.Errorf("scan user_passkeys: %w", err)
	}
	return c, nil
}

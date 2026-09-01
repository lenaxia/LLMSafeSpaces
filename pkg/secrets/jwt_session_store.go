// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JWTSession is the in-memory shape of a jwt_sessions row.
//
// Layout matches migration 000045:
//
//	jwt_sessions(jti UUID PK, user_id TEXT FK, wrapped_dek BYTEA,
//	             kek_salt BYTEA, created_at TIMESTAMPTZ, expires_at TIMESTAMPTZ)
//
// Rows are written only by pre-US-70.5 logins (the durable write half
// is deleted); WrappedDEK/KEKSalt are historical columns kept for the
// age-out window and never unwrapped by current code.
//
// JTI is the canonical UUID form that auth.go generates via
// uuid.New().String() and embeds in the JWT's "jti" claim.
type JWTSession struct {
	JTI        uuid.UUID
	UserID     string
	WrappedDEK []byte
	KEKSalt    []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// JWTSessionStore abstracts the durable jwt_sessions table for tests.
// US-70.5 deleted the write and rehydrate halves; the remaining surface
// enumerates live sessions (for the warm-cache walk) and drives the
// table toward empty (revocation deletes + the janitor) as
// pre-demolition rows age out.
type JWTSessionStore interface {
	// DeleteJWTSession removes the row for a specific jti. Used by EvictDEK
	// (logout, cache miss handling, etc.) so the durable row does not
	// outlive its Redis counterpart.
	DeleteJWTSession(ctx context.Context, jti uuid.UUID) error
	// DeleteJWTSessionsForUser removes all rows for a user. Used by
	// RevokeAllUserSessions (password reset / explicit logout-everywhere).
	// Returns the number of rows deleted.
	DeleteJWTSessionsForUser(ctx context.Context, userID string) (int64, error)
	// DeleteExpiredJWTSessions prunes rows with expires_at < before. The
	// janitor goroutine calls this on a ticker. Returns the number of rows
	// deleted. Bounded by the idx_jwt_sessions_expires_at index for O(log N)
	// scan even at 1M rows.
	DeleteExpiredJWTSessions(ctx context.Context, before time.Time) (int64, error)
	// ListActiveJWTSessionsForUser returns non-expired rows for userID,
	// ordered created_at DESC (most-recent first) and bounded by limit
	// (limit <= 0 means unlimited-per-caller-convention, though the
	// caller MUST supply a sensible bound in production).
	//
	// Used by KeyService.GetCachedDEKForUser to enumerate candidate jtis
	// for the warm-cache walk (US-70.5: the rows are an index of live
	// sessions; the DEK itself is read from the Redis cache only).
	//
	// "Active" means expires_at is strictly AFTER the store's clock
	// (SQL: expires_at > NOW()). A row at the exact expires_at is
	// expired — matches DeleteExpiredJWTSessions's < semantics.
	//
	// Bounded by idx_jwt_sessions_user_id + a filter on expires_at
	// (partial index on expires_at could speed the AND but is not
	// required at current data scale).
	//
	// Returns nil (or []) with nil error when no matching rows exist —
	// callers use empty to signal "no live session" (ErrDEKUnavailable).
	ListActiveJWTSessionsForUser(ctx context.Context, userID string, limit int) ([]*JWTSession, error)
}

// PgJWTSessionStore is the production JWTSessionStore backed by Postgres.
type PgJWTSessionStore struct {
	pool *pgxpool.Pool
}

// NewPgJWTSessionStore creates a new PostgreSQL-backed JWTSessionStore.
func NewPgJWTSessionStore(pool *pgxpool.Pool) *PgJWTSessionStore {
	return &PgJWTSessionStore{pool: pool}
}

// DeleteJWTSession removes the row for jti. Idempotent: deleting a
// non-existent row is not an error (the DELETE returns rowsAffected=0).
func (s *PgJWTSessionStore) DeleteJWTSession(ctx context.Context, jti uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM jwt_sessions WHERE jti = $1`, jti); err != nil {
		return fmt.Errorf("delete jwt_sessions: %w", err)
	}
	return nil
}

// DeleteJWTSessionsForUser removes every row for userID. Used by
// RevokeAllUserSessions so a password-reset cascade leaves no durable
// DEK row behind. Returns the number of rows deleted so the caller
// can audit-log the magnitude.
func (s *PgJWTSessionStore) DeleteJWTSessionsForUser(ctx context.Context, userID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM jwt_sessions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete jwt_sessions for user: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredJWTSessions prunes rows whose expires_at is strictly
// before the provided cutoff. The janitor passes time.Now(); tests
// pass a fixed clock for determinism. Returns the count for logging
// and monitoring (a sudden spike in deletes flags a clock skew or a
// large rotation event).
func (s *PgJWTSessionStore) DeleteExpiredJWTSessions(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM jwt_sessions WHERE expires_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired jwt_sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListActiveJWTSessionsForUser returns non-expired rows for userID.
// See interface godoc for full contract. Uses idx_jwt_sessions_user_id
// (schema 000001) plus a filter on expires_at > NOW(). At current data
// scale (thousands of sessions per active user max) the trailing filter
// on expires_at is a scan of the per-user rows only; no compound index
// needed. If per-user row counts grow past ~10k a partial index
// (WHERE expires_at > NOW()) can be added later without changing this
// query.
//
// The NOW() comparison is inline in the SQL so the database's clock
// is authoritative — same source of truth the janitor uses when it
// prunes.
//
// When limit <= 0, no LIMIT clause is added. Callers should always
// pass a sensible bound (KeyService.GetCachedDEKForUser passes 5, per
// jwtSessionUserLookupLimit).
func (s *PgJWTSessionStore) ListActiveJWTSessionsForUser(ctx context.Context, userID string, limit int) ([]*JWTSession, error) {
	query := `SELECT jti, user_id, wrapped_dek, kek_salt, created_at, expires_at
	          FROM jwt_sessions
	          WHERE user_id = $1 AND expires_at > NOW()
	          ORDER BY created_at DESC`
	args := []interface{}{userID}
	if limit > 0 {
		query += " LIMIT $2"
		args = append(args, limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query jwt_sessions for user: %w", err)
	}
	defer rows.Close()

	out := make([]*JWTSession, 0)
	for rows.Next() {
		var r JWTSession
		if err := rows.Scan(&r.JTI, &r.UserID, &r.WrappedDEK, &r.KEKSalt, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan jwt_sessions row: %w", err)
		}
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jwt_sessions: %w", err)
	}
	return out, nil
}

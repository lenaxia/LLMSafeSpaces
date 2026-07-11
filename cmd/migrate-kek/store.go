// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// --- derive helpers (mirror secrets_adapters.go) ---

func deriveKey(master []byte, purpose string) []byte {
	if len(master) < 32 {
		return nil
	}
	r := hkdf.New(sha256.New, master, []byte("llmsafespaces-server"), []byte(purpose))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil
	}
	return key
}

func hexDecode(s string) ([]byte, error) {
	return hex.DecodeString(s)
}

// --- pgMigrationStore ---

type pgConn interface {
	QueryRow(ctx context.Context, query string, args ...any) pgRow
	Query(ctx context.Context, query string, args ...any) (pgRows, error)
	Exec(ctx context.Context, query string, args ...any) error
	Close()
}

type pgRow interface {
	Scan(dest ...any) error
}

type pgRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// pgMigrationStore implements secrets.MigrationStore.
type pgMigrationStore struct {
	db pgConn
}

func (s *pgMigrationStore) ListMigrationRows(ctx context.Context, table, resumeFromID string, limit int) ([]secrets.MigrationRow, error) {
	// Table-specific columns: provider_credentials has owner_type; the
	// other two tables (api_keys, org_sso_configs) do not.
	needsOwnerType := table == "provider_credentials"
	var colList string
	if needsOwnerType {
		colList = "id, owner_type, ciphertext, key_version"
	} else {
		colList = "id, ciphertext, key_version"
	}
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE id > $1 ORDER BY id ASC`, colList, table)
	args := []any{""}
	if resumeFromID != "" {
		args[0] = resumeFromID
	}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []secrets.MigrationRow
	for rows.Next() {
		var r secrets.MigrationRow
		r.Table = table
		if needsOwnerType {
			var ownerType string
			if err := rows.Scan(&r.ID, &ownerType, &r.Ciphertext, &r.KeyVersion); err != nil {
				return out, err
			}
			r.OwnerType = ownerType
		} else {
			if err := rows.Scan(&r.ID, &r.Ciphertext, &r.KeyVersion); err != nil {
				return out, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgMigrationStore) UpdateMigrationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error {
	query := fmt.Sprintf(`UPDATE %s SET ciphertext = $1, key_version = $2 WHERE id = $3`, table)
	return s.db.Exec(ctx, query, newCiphertext, newKeyVersion, rowID)
}

func (s *pgMigrationStore) FlushDEKCache(ctx context.Context) error {
	// Redis flush — delegated to the composite store (see main.go).
	return nil
}

// --- compositeMigrationStore ---

// compositeMigrationStore wraps a PG store and a Redis flusher.
type compositeMigrationStore struct {
	pg    *pgMigrationStore
	redis redisFlusher
}

type redisFlusher interface {
	FlushDEKCache(ctx context.Context) error
}

func (s *compositeMigrationStore) ListMigrationRows(ctx context.Context, table, resumeFromID string, limit int) ([]secrets.MigrationRow, error) {
	return s.pg.ListMigrationRows(ctx, table, resumeFromID, limit)
}

func (s *compositeMigrationStore) UpdateMigrationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error {
	return s.pg.UpdateMigrationRow(ctx, table, rowID, newCiphertext, newKeyVersion)
}

func (s *compositeMigrationStore) FlushDEKCache(ctx context.Context) error {
	return s.redis.FlushDEKCache(ctx)
}

// --- Constructors (stubs — PG/Redis connections deferred per rotate-kek convention) ---

func newPgMigrationStore(dbURL string) (*pgMigrationStore, error) {
	return nil, fmt.Errorf("postgres connection not yet wired (use MigrationCoordinator directly for testing)")
}

func (s *pgMigrationStore) Close() {}

type redisCacheFlusherImpl struct{}

func newRedisCacheFlusher(redisURL string) (*redisCacheFlusherImpl, error) {
	return nil, fmt.Errorf("redis connection not yet wired")
}

func (r *redisCacheFlusherImpl) Close() {}

func (r *redisCacheFlusherImpl) FlushDEKCache(ctx context.Context) error {
	return nil
}

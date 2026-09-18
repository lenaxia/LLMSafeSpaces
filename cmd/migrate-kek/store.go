// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

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

// pgPoolConn adapts *pgxpool.Pool to the pgConn seam: pgx's Exec returns a
// CommandTag the store layer has no use for, so it is discarded here.
type pgPoolConn struct {
	pool *pgxpool.Pool
}

func (c pgPoolConn) QueryRow(ctx context.Context, query string, args ...any) pgRow {
	return c.pool.QueryRow(ctx, query, args...)
}

func (c pgPoolConn) Query(ctx context.Context, query string, args ...any) (pgRows, error) {
	//nolint:sqlclosecheck // the returned rows are closed by the caller through the pgRows seam (defer rows.Close() in the List* methods)
	return c.pool.Query(ctx, query, args...)
}

func (c pgPoolConn) Exec(ctx context.Context, query string, args ...any) error {
	_, err := c.pool.Exec(ctx, query, args...)
	return err
}

func (c pgPoolConn) Close() { c.pool.Close() }

var _ pgRow = pgx.Row(nil)
var _ pgRows = pgx.Rows(nil)

// dialPingTimeout bounds both the Postgres pool ping and the Redis client
// ping at construction — both are "is the endpoint alive" probes with the
// same budget.
const dialPingTimeout = 10 * time.Second

// migrationTableColumns maps each migratable table to its row-id, ciphertext,
// and owner-type column layout. The table name is the allowlist: queries
// interpolate it, so any table not in this map is rejected (never passed to
// SQL). idIsUUID selects the ::uuid cast for resume cursors and row updates —
// provider_credentials.id and org_sso_configs.org_id are uuid columns while
// api_keys.id is varchar.
var migrationTableColumns = map[string]struct {
	id         string
	ciphertext string
	ownerType  bool
	idIsUUID   bool
}{
	"provider_credentials": {id: "id", ciphertext: "ciphertext", ownerType: true, idIsUUID: true},
	"api_keys":             {id: "id", ciphertext: "key_ciphertext", idIsUUID: false},
	"org_sso_configs":      {id: "org_id", ciphertext: "oidc_client_secret", idIsUUID: true},
}

// idPredicate renders "<col> > $N" (with the ::uuid cast when needed) for the
// resume cursor. An empty cursor drops the predicate entirely — an empty
// string is not a valid uuid, so passing it as a parameter would error.
func idPredicate(col string, param int, idIsUUID bool) string {
	if idIsUUID {
		return fmt.Sprintf(" AND %s > $%d::uuid", col, param)
	}
	return fmt.Sprintf(" AND %s > $%d", col, param)
}

// pgMigrationStore implements secrets.MigrationStore.
type pgMigrationStore struct {
	db    pgConn
	close func()
}

func newPgMigrationStore(dbURL string) (*pgMigrationStore, error) {
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		return nil, fmt.Errorf("construct pgx pool: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialPingTimeout)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &pgMigrationStore{db: pgPoolConn{pool: pool}, close: pool.Close}, nil
}

func (s *pgMigrationStore) ListMigrationRows(ctx context.Context, table, resumeFromID string, limit int) ([]secrets.MigrationRow, error) {
	cols, ok := migrationTableColumns[table]
	if !ok {
		return nil, fmt.Errorf("unknown migration table %q", table)
	}
	colList := fmt.Sprintf(`%s, %s, key_version`, cols.id, cols.ciphertext)
	if cols.ownerType {
		colList = fmt.Sprintf(`%s, owner_type, %s, key_version`, cols.id, cols.ciphertext)
	}
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE 1=1`, colList, table)
	args := []any{}
	if resumeFromID != "" {
		args = append(args, resumeFromID)
		query += idPredicate(cols.id, len(args), cols.idIsUUID)
	}
	query += fmt.Sprintf(` ORDER BY %s ASC`, cols.id)
	if limit > 0 {
		// limit is a program-supplied batch size, not operator input, so it
		// is interpolated directly.
		query += fmt.Sprintf(` LIMIT %d`, limit)
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
		if cols.ownerType {
			var ownerType string
			if err := rows.Scan(&r.ID, &ownerType, &r.Ciphertext, &r.KeyVersion); err != nil {
				return out, err
			}
			r.OwnerType = ownerType
		} else if err := rows.Scan(&r.ID, &r.Ciphertext, &r.KeyVersion); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *pgMigrationStore) UpdateMigrationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error {
	cols, ok := migrationTableColumns[table]
	if !ok {
		return fmt.Errorf("unknown migration table %q", table)
	}
	idMatch := fmt.Sprintf(`%s = $3`, cols.id)
	if cols.idIsUUID {
		idMatch = fmt.Sprintf(`%s = $3::uuid`, cols.id)
	}
	query := fmt.Sprintf(`UPDATE %s SET %s = $1, key_version = $2 WHERE %s`, table, cols.ciphertext, idMatch)
	return s.db.Exec(ctx, query, newCiphertext, newKeyVersion, rowID)
}

func (s *pgMigrationStore) FlushDEKCache(ctx context.Context) error {
	// A pg-only store has no DEK cache to flush. When --redis-url is given,
	// run() wraps this store in compositeMigrationStore, whose FlushDEKCache
	// delegates to the Redis flusher.
	return nil
}

func (s *pgMigrationStore) Close() {
	if s.close != nil {
		s.close()
	}
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

// --- Redis cache flusher ---

// redisCacheFlusherImpl evicts the Redis DEK cache after a migration so
// stale DEKs (wrapped under the old KEK) are not served.
type redisCacheFlusherImpl struct {
	client *redis.Client
}

func newRedisCacheFlusher(redisURL string) (*redisCacheFlusherImpl, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), dialPingTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping Redis: %w", err)
	}
	return &redisCacheFlusherImpl{client: client}, nil
}

// FlushDEKCache deletes every dek:* key. It scans rather than FLUSHDB-ing:
// the Redis instance is shared with rate limiters, sessions, and token
// revocations that must survive a KEK migration. Keys are collected across
// the full SCAN iteration and deleted afterwards — interleaving deletes
// with the cursor walk is sensitive to server-side rehashing.
func (r *redisCacheFlusherImpl) FlushDEKCache(ctx context.Context) error {
	const scanBatch = 500
	var match []string
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, secrets.DEKCachePrefix+"*", scanBatch).Result()
		if err != nil {
			return fmt.Errorf("scan %s* keys: %w", secrets.DEKCachePrefix, err)
		}
		match = append(match, keys...)
		if next == 0 {
			break
		}
		cursor = next
	}
	for i := 0; i < len(match); i += scanBatch {
		end := i + scanBatch
		if end > len(match) {
			end = len(match)
		}
		if err := r.client.Del(ctx, match[i:end]...).Err(); err != nil {
			return fmt.Errorf("delete %s* keys: %w", secrets.DEKCachePrefix, err)
		}
	}
	return nil
}

func (r *redisCacheFlusherImpl) Close() {
	_ = r.client.Close()
}

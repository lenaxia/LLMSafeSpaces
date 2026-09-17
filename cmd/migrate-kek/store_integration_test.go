// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package main

// Integration tests for the migrate-kek store layer against a real Postgres
// (issue #830). Same testharness conventions as cmd/rotate-kek — see the
// header comment there for why the harness package cannot be imported from
// cmd/ and what is replicated here.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/migrations"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// --- harness-convention wiring (TEST_DATABASE_URL + skip-if-unreachable) ---

func testDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
}

func newIntegrationStore(t *testing.T) (*pgMigrationStore, *pgxpool.Pool) {
	t.Helper()
	dsn := testDSN()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skipping migrate-kek integration test: pgx pool: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("skipping migrate-kek integration test: Postgres unreachable at %s — set TEST_DATABASE_URL: %v", dsn, err)
	}
	t.Cleanup(pool.Close)

	applyMigrations(t, dsn)

	store, err := newPgMigrationStore(dsn)
	require.NoError(t, err, "newPgMigrationStore must connect against the live test Postgres")
	t.Cleanup(store.Close)
	return store, pool
}

func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	migDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = migDB.Close() }()
	drv, err := pgxdriver.WithInstance(migDB, &pgxdriver.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", src, "pgx", drv)
	require.NoError(t, err)
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("apply migrations: %v", err)
	}
}

var integrationIDCounter uint64
var integrationEpoch = time.Now().UnixNano()

func integrationID(label string) string {
	return fmt.Sprintf("mk-%s-%d-%d", label, integrationEpoch, atomic.AddUint64(&integrationIDCounter, 1))
}

func integrationUUID(label string) string {
	n := atomic.AddUint64(&integrationIDCounter, 1)
	return fmt.Sprintf("%08x-0004-4000-8000-%06x%06x", uint32(integrationEpoch), uint32(n>>16), uint32(n&0xffffff))
}

// --- fixtures ---

func seedUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email, password_hash) VALUES ($1, $1, $2, 'x')`,
		userID, userID+"@test.invalid")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
}

func seedProviderCredential(t *testing.T, pool *pgxpool.Pool, ownerType string, ct []byte) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO provider_credentials (owner_type, owner_id, name, ciphertext, key_version, kind, slug)
		 VALUES ($1, 'seed', $2, $3, 1, 'openai', $2) RETURNING id`,
		ownerType, "cred-"+integrationID("n"), ct).Scan(&id)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_credentials WHERE id = $1::uuid`, id)
	})
	return id
}

func seedAPIKey(t *testing.T, pool *pgxpool.Pool, userID string, ct []byte) string {
	t.Helper()
	id := integrationID("ak")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, user_id, key, name, key_ciphertext, key_version)
		 VALUES ($1, $2, $3, $3, $4, 1)`,
		id, userID, "lsp_"+id, ct)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id) })
	return id
}

func seedOrgSSO(t *testing.T, pool *pgxpool.Pool, ct []byte) string {
	t.Helper()
	owner := integrationID("orgowner")
	seedUser(t, pool, owner)
	orgID := integrationUUID("org")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO organizations (id, name, slug, created_by) VALUES ($1::uuid, $2, $2, $3)`,
		orgID, "slug-"+orgID, owner)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO org_sso_configs (org_id, oidc_discovery_url, oidc_client_id, oidc_client_secret)
		 VALUES ($1::uuid, 'https://idp.example', 'client', $2)`,
		orgID, ct)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM org_sso_configs WHERE org_id = $1::uuid`, orgID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1::uuid`, orgID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, owner)
	})
	return orgID
}

// filterMigrationRows keeps only rows whose ID is in want — the harness
// convention for shared-table assertions (the CI Postgres is shared across
// concurrently-running suite binaries; never assume exclusive ownership).
func filterMigrationRows(rows []secrets.MigrationRow, want map[string]bool) []secrets.MigrationRow {
	var out []secrets.MigrationRow
	for _, r := range rows {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func migrationRowIDs(rows []secrets.MigrationRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// --- store-layer tests ---

// TestPgMigrationStore_ListMigrationRows verifies listing across all three
// migration tables with the correct per-table column mapping (this query was
// wrong before wiring: it selected nonexistent `ciphertext` columns on
// api_keys and org_sso_configs — issue #830's "verify --tables semantics").
func TestPgMigrationStore_ListMigrationRows(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	// provider_credentials: owner_type must come through.
	adminID := seedProviderCredential(t, pool, "admin", []byte("lkms:v1:admin-blob"))
	orgID := seedProviderCredential(t, pool, "org", []byte("lkms:v1:org-blob"))

	// The CI Postgres is shared with the rotate-kek suite (go test runs the
	// two package binaries concurrently) — scope assertions to rows we
	// seeded instead of assuming table ownership.
	rows, err := store.ListMigrationRows(ctx, "provider_credentials", "", 0)
	require.NoError(t, err)
	mine := filterMigrationRows(rows, map[string]bool{adminID: true, orgID: true})
	require.Len(t, mine, 2)
	ownerByID := map[string]string{}
	for _, r := range mine {
		ownerByID[r.ID] = r.OwnerType
	}
	assert.Equal(t, "admin", ownerByID[adminID])
	assert.Equal(t, "org", ownerByID[orgID])

	// Resume cursor + limit on the varchar-keyed api_keys table.
	userID := integrationID("u")
	seedUser(t, pool, userID)
	ak1 := seedAPIKey(t, pool, userID, []byte("lkms:v1:ak1"))
	ak2 := seedAPIKey(t, pool, userID, []byte("lkms:v1:ak2"))
	rows, err = store.ListMigrationRows(ctx, "api_keys", "", 0)
	require.NoError(t, err)
	mineAK := filterMigrationRows(rows, map[string]bool{ak1: true, ak2: true})
	require.Len(t, mineAK, 2)
	assert.Equal(t, []string{ak1, ak2}, migrationRowIDs(mineAK), "insertion order is id ASC here because ids share a monotonically increasing marker")
	rows, err = store.ListMigrationRows(ctx, "api_keys", ak1, 0)
	require.NoError(t, err)
	mineAK = filterMigrationRows(rows, map[string]bool{ak1: true, ak2: true})
	assert.Equal(t, []string{ak2}, migrationRowIDs(mineAK))
	rows, err = store.ListMigrationRows(ctx, "api_keys", "", 1)
	require.NoError(t, err)
	require.Len(t, rows, 1, "LIMIT must cap the listing")

	// org_sso_configs: uuid PK + oidc_client_secret column.
	ssoID := seedOrgSSO(t, pool, []byte("lkms:v1:sso"))
	rows, err = store.ListMigrationRows(ctx, "org_sso_configs", "", 0)
	require.NoError(t, err)
	mineSSO := filterMigrationRows(rows, map[string]bool{ssoID: true})
	require.Len(t, mineSSO, 1)
	assert.Equal(t, ssoID, mineSSO[0].ID)
	assert.Equal(t, []byte("lkms:v1:sso"), mineSSO[0].Ciphertext)

	_, err = store.ListMigrationRows(ctx, "user_keys", "", 0)
	require.Error(t, err, "user_keys is not a migration table")
	assert.Contains(t, err.Error(), "unknown migration table")

	_, err = store.ListMigrationRows(ctx, "users; DROP TABLE users", "", 0)
	require.Error(t, err, "unknown tables must be rejected — no SQL-injection surface")
}

// TestPgMigrationStore_UpdateMigrationRow verifies the UPDATE path writes
// ciphertext + key_version to the right columns (key_version resets to 1 —
// cosmetic under KMS, D6).
func TestPgMigrationStore_UpdateMigrationRow(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	userID := integrationID("u")
	seedUser(t, pool, userID)
	ak := seedAPIKey(t, pool, userID, []byte("lkms:v1:old"))
	require.NoError(t, store.UpdateMigrationRow(ctx, "api_keys", ak, []byte("aws-kms:v1:new"), 1))
	var ct []byte
	var ver int
	err := pool.QueryRow(ctx, `SELECT key_ciphertext, key_version FROM api_keys WHERE id = $1`, ak).Scan(&ct, &ver)
	require.NoError(t, err)
	assert.Equal(t, []byte("aws-kms:v1:new"), ct)
	assert.Equal(t, 1, ver)

	pcID := seedProviderCredential(t, pool, "admin", []byte("lkms:v1:old"))
	require.NoError(t, store.UpdateMigrationRow(ctx, "provider_credentials", pcID, []byte("aws-kms:v1:new"), 1))
	err = pool.QueryRow(ctx, `SELECT ciphertext FROM provider_credentials WHERE id = $1::uuid`, pcID).Scan(&ct)
	require.NoError(t, err)
	assert.Equal(t, []byte("aws-kms:v1:new"), ct)

	require.Error(t, store.UpdateMigrationRow(ctx, "bogus", "x", ct, 1))
}

// --- audit e2e (the runbook's step-5 gate) ---

// TestIntegration_MigrateAuditRunbookOutput drives runAudit — the exact CLI
// function behind `migrate-kek --audit` — against a seeded DB containing
// every ciphertext class, and asserts the runbook's promised output: per-
// table prefix distribution and a FAIL verdict while legacy rows remain.
func TestIntegration_MigrateAuditRunbookOutput(t *testing.T) {
	_, pool := newIntegrationStore(t)

	// provider_credentials: one per class.
	seedProviderCredential(t, pool, "admin", []byte("aws-kms:v1:migrated")) // target
	seedProviderCredential(t, pool, "admin", []byte("lkms:v1:local"))       // local
	seedProviderCredential(t, pool, "admin", []byte{0x01, 0x02, 0x03})      // legacy raw
	seedProviderCredential(t, pool, "admin", []byte("gcp-kms:v1:other"))    // other KMS

	res := captureStderr(t, func() error {
		return runAudit(testDSN(), "aws")
	})

	require.Error(t, res.err, "audit must FAIL while non-target rows remain")
	out := res.output
	assert.Contains(t, out, "provider_credentials")
	assert.Contains(t, out, "OUTSTANDING", "3 of 4 provider_credentials rows are not on aws-kms")
	assert.Contains(t, out, "FAIL: at least one table has rows not yet on aws-kms")

	// The other two tables are empty → trivially OK, and the header matches
	// the runbook's documented table shape.
	assert.Contains(t, out, "TABLE")
	assert.Contains(t, out, "api_keys")
	assert.Contains(t, out, "org_sso_configs")
}

// capturedStderr carries the output and return error of a stderr-printing
// function under test.
type capturedStderr struct {
	output string
	err    error
}

func captureStderr(t *testing.T, fn func() error) capturedStderr {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(drained)
	}()
	fnErr := fn()
	w.Close()
	os.Stderr = saved
	<-drained
	_ = r.Close()
	return capturedStderr{output: buf.String(), err: fnErr}
}

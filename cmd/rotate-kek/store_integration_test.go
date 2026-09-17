// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package main

// Integration tests for the rotate-kek store layer against a real Postgres
// (issue #830). These follow the testharness conventions exactly — same
// TEST_DATABASE_URL contract, same skip-if-unreachable gate, same embedded
// migration set (api/migrations) — because Go's internal-package visibility
// forbids cmd/ from importing api/internal/testharness itself (the same
// constraint the harness README documents for pkg/secrets' getTestPool).
// In CI these run in .github/workflows/secrets-integration.yml alongside the
// pkg/secrets suites against the same Postgres service.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
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

// newIntegrationStore connects to the test Postgres (skipping when
// unreachable), applies the embedded migration set idempotently (mirroring
// testharness.MigrateUp), and returns a real pgRotationStore plus the raw
// pool for seeding/asserting.
func newIntegrationStore(t *testing.T) (*pgRotationStore, *pgxpool.Pool) {
	t.Helper()
	dsn := testDSN()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skipping rotate-kek integration test: pgx pool: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("skipping rotate-kek integration test: Postgres unreachable at %s — set TEST_DATABASE_URL: %v", dsn, err)
	}
	t.Cleanup(pool.Close)

	applyMigrations(t, dsn)

	store, err := newPgRotationStore(dsn)
	require.NoError(t, err, "newPgRotationStore must connect against the live test Postgres")
	t.Cleanup(store.Close)
	return store, pool
}

// applyMigrations mirrors testharness's runMigrate: golang-migrate over the
// embedded api/migrations FS, idempotent (ErrNoChange is success).
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

// integrationID returns a per-call unique marker (harness ID() convention) so
// parallel tests on the shared test DB never collide.
func integrationID(label string) string {
	return fmt.Sprintf("rk-%s-%d-%d", label, integrationEpoch, atomic.AddUint64(&integrationIDCounter, 1))
}

// integrationUUID returns a per-call unique marker shaped as a valid uuid —
// required for the uuid-keyed tables (organizations.org_id,
// provider_credentials.id, org_sso_configs.org_id).
func integrationUUID(label string) string {
	n := atomic.AddUint64(&integrationIDCounter, 1)
	return fmt.Sprintf("%08x-0004-4000-8000-%06x%06x", uint32(integrationEpoch), uint32(n>>16), uint32(n&0xffffff))
}

// --- fixtures ---

func integrationMasterKey(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// seedUser inserts a users row and registers cleanup.
func seedUser(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, username, email, password_hash) VALUES ($1, $1, $2, 'x')`,
		userID, userID+"@test.invalid")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
}

// seedOrg inserts an organizations row (owned by a seeded user, satisfying
// organizations.created_by's FK) and registers cleanup.
func seedOrg(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	owner := integrationID("orgowner")
	seedUser(t, pool, owner)
	orgID := integrationUUID("org")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO organizations (id, name, slug, created_by) VALUES ($1::uuid, $2, $2, $3)`,
		orgID, "slug-"+orgID, owner)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1::uuid`, orgID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, owner)
	})
	return orgID
}

// seedProviderCredential inserts one provider_credentials row encrypted
// under key at version and registers cleanup. Returns the row id.
func seedProviderCredential(t *testing.T, pool *pgxpool.Pool, ownerType string, ct []byte, version int) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO provider_credentials (owner_type, owner_id, name, ciphertext, key_version, kind, slug)
		 VALUES ($1, 'seed', $2, $3, $4, 'openai', $2) RETURNING id`,
		ownerType, "cred-"+integrationID("n"), ct, version).Scan(&id)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM provider_credentials WHERE id = $1::uuid`, id)
	})
	return id
}

// seedAPIKey inserts one api_keys row and registers cleanup.
func seedAPIKey(t *testing.T, pool *pgxpool.Pool, userID string, ct []byte, version int) string {
	t.Helper()
	id := integrationID("ak")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, user_id, key, name, key_ciphertext, key_version)
		 VALUES ($1, $2, $3, $3, $4, $5)`,
		id, userID, "lsp_"+id[:20], ct, version)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id) })
	return id
}

// seedUserKey inserts one user_keys row and registers cleanup.
func seedUserKey(t *testing.T, pool *pgxpool.Pool, userID string, ct []byte, version int) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO user_keys (user_id, key_version, wrapped_dek, salt) VALUES ($1, $2, $3, $4)`,
		userID, version, ct, []byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM user_keys WHERE user_id = $1`, userID) })
}

// --- store-layer tests ---

// filterRotationRows keeps only rows whose ID is in want — the harness
// convention for shared-table assertions (the CI Postgres is shared across
// concurrently-running suite binaries; never assume exclusive ownership).
func filterRotationRows(rows []secrets.RotationRow, want map[string]bool) []secrets.RotationRow {
	var out []secrets.RotationRow
	for _, r := range rows {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func rowIDs(rows []secrets.RotationRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// TestPgRotationStore_ListRotationRows verifies the real Postgres listing
// across all four tables: version filtering, resume cursor, ordering, and the
// per-table column mapping (api_keys id/key_ciphertext, org_sso_configs
// org_id/oidc_client_secret, user_keys user_id/wrapped_dek,
// provider_credentials owner_type).
func TestPgRotationStore_ListRotationRows(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	oldKey := secrets.DeriveServerKey(integrationMasterKey(0x10), "provider-credentials")
	userID := integrationID("u")
	seedUser(t, pool, userID)

	ct1, err := secrets.EncryptSecret(oldKey, []byte("pc-admin"))
	require.NoError(t, err)
	id1 := seedProviderCredential(t, pool, "admin", ct1, 1)
	orgKey := secrets.DeriveServerKey(integrationMasterKey(0x10), "org-credentials")
	ct2, err := secrets.EncryptSecret(orgKey, []byte("pc-org"))
	require.NoError(t, err)
	id2 := seedProviderCredential(t, pool, "org", ct2, 1)
	// Already at target — must be excluded.
	ct3, err := secrets.EncryptSecret(oldKey, []byte("pc-done"))
	require.NoError(t, err)
	seedProviderCredential(t, pool, "admin", ct3, 2)

	// uuid v4 ids are random: the store's ORDER BY id ASC is byte order, not
	// insertion order. Sort to know the expected sequence.
	first, second := id1, id2
	if first > second {
		first, second = second, first
	}

	// The CI Postgres is shared with the migrate-kek suite (go test runs the
	// two package binaries concurrently) — scope every assertion to rows we
	// seeded via filterRotationRows rather than assuming table ownership.
	rows, err := store.ListRotationRows(ctx, "provider_credentials", "", 2, 0)
	require.NoError(t, err)
	mine := filterRotationRows(rows, map[string]bool{id1: true, id2: true})
	require.Len(t, mine, 2, "exactly our rows below target must be listed (row at target excluded); got %v", rowIDs(rows))
	assert.Equal(t, []string{first, second}, rowIDs(mine), "our rows must be ordered by id ASC")
	ownerByID := map[string]string{}
	for _, r := range mine {
		ownerByID[r.ID] = r.OwnerType
	}
	assert.Equal(t, "admin", ownerByID[id1])
	assert.Equal(t, "org", ownerByID[id2])
	ctByID := map[string][]byte{id1: ct1, id2: ct2}
	for _, r := range mine {
		assert.Equal(t, ctByID[r.ID], r.Ciphertext, "row %s", r.ID)
	}

	// Resume cursor: listing after the first row must return only the second
	// of ours.
	rows, err = store.ListRotationRows(ctx, "provider_credentials", first, 2, 0)
	require.NoError(t, err)
	mine = filterRotationRows(rows, map[string]bool{id1: true, id2: true})
	assert.Equal(t, []string{second}, rowIDs(mine))

	// Limit: the id-ASC stream (ours included) is capped.
	rows, err = store.ListRotationRows(ctx, "provider_credentials", "", 2, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1, "LIMIT must cap the listing")

	// api_keys column mapping.
	akCT, err := secrets.EncryptSecret(oldKey, []byte("ak"))
	require.NoError(t, err)
	akID := seedAPIKey(t, pool, userID, akCT, 1)
	rows, err = store.ListRotationRows(ctx, "api_keys", "", 2, 0)
	require.NoError(t, err)
	mineAK := filterRotationRows(rows, map[string]bool{akID: true})
	require.Len(t, mineAK, 1)
	assert.Equal(t, akID, mineAK[0].ID)
	assert.Equal(t, akCT, mineAK[0].Ciphertext)
	assert.Empty(t, mineAK[0].OwnerType)

	// org_sso_configs column mapping (PK org_id, ciphertext oidc_client_secret).
	orgID := seedOrg(t, pool)
	ssoCT, err := secrets.EncryptSecret(oldKey, []byte("sso"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO org_sso_configs (org_id, oidc_discovery_url, oidc_client_id, oidc_client_secret)
		 VALUES ($1::uuid, 'https://idp.example', 'client', $2)`,
		orgID, ssoCT)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM org_sso_configs WHERE org_id = $1::uuid`, orgID) })
	rows, err = store.ListRotationRows(ctx, "org_sso_configs", "", 2, 0)
	require.NoError(t, err)
	mineSSO := filterRotationRows(rows, map[string]bool{orgID: true})
	require.Len(t, mineSSO, 1)
	assert.Equal(t, orgID, mineSSO[0].ID, "org_sso_configs rows must be keyed by org_id")
	assert.Equal(t, ssoCT, mineSSO[0].Ciphertext)

	// user_keys column mapping (keyed by user_id, ciphertext wrapped_dek).
	ukCT, err := secrets.EncryptSecret(oldKey, []byte("uk"))
	require.NoError(t, err)
	seedUserKey(t, pool, userID, ukCT, 1)
	rows, err = store.ListRotationRows(ctx, "user_keys", "", 2, 0)
	require.NoError(t, err)
	mineUK := filterRotationRows(rows, map[string]bool{userID: true})
	require.Len(t, mineUK, 1)
	assert.Equal(t, userID, mineUK[0].ID, "user_keys rows must be keyed by user_id")
	assert.Equal(t, ukCT, mineUK[0].Ciphertext)

	// Unknown table is rejected (no SQL-injection surface via table name).
	_, err = store.ListRotationRows(ctx, "users; DROP TABLE users", "", 2, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown rotation table")
}

// TestPgRotationStore_UpdateRotationRow verifies the UPDATE path against real
// Postgres: ciphertext + key_version land in the right columns for a
// varchar-keyed table and a uuid-keyed table.
func TestPgRotationStore_UpdateRotationRow(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	oldKey := secrets.DeriveServerKey(integrationMasterKey(0x10), "master-kek")
	newKey := secrets.DeriveServerKey(integrationMasterKey(0x20), "master-kek")

	userID := integrationID("u")
	seedUser(t, pool, userID)

	ct, err := secrets.EncryptSecret(oldKey, []byte("ak-plain"))
	require.NoError(t, err)
	akID := seedAPIKey(t, pool, userID, ct, 1)
	newCT, err := secrets.EncryptSecret(newKey, []byte("ak-plain"))
	require.NoError(t, err)
	require.NoError(t, store.UpdateRotationRow(ctx, "api_keys", akID, newCT, 2))
	var gotCT []byte
	var gotVer int
	err = pool.QueryRow(ctx, `SELECT key_ciphertext, key_version FROM api_keys WHERE id = $1`, akID).Scan(&gotCT, &gotVer)
	require.NoError(t, err)
	assert.Equal(t, newCT, gotCT)
	assert.Equal(t, 2, gotVer)

	pcCT, err := secrets.EncryptSecret(oldKey, []byte("pc-plain"))
	require.NoError(t, err)
	pcID := seedProviderCredential(t, pool, "admin", pcCT, 1)
	pcNew, err := secrets.EncryptSecret(newKey, []byte("pc-plain"))
	require.NoError(t, err)
	require.NoError(t, store.UpdateRotationRow(ctx, "provider_credentials", pcID, pcNew, 2))
	err = pool.QueryRow(ctx, `SELECT ciphertext, key_version FROM provider_credentials WHERE id = $1::uuid`, pcID).Scan(&gotCT, &gotVer)
	require.NoError(t, err)
	assert.Equal(t, pcNew, gotCT)
	assert.Equal(t, 2, gotVer)

	require.Error(t, store.UpdateRotationRow(ctx, "bogus_table", "x", newCT, 2), "unknown table must be rejected")
}

// --- e2e: the runbook scenario ---

// buildCLIProviders mirrors run()'s provider construction so the e2e
// exercises the same derivation path the binary uses (post-#832:
// secrets.DeriveServerKey).
func buildCLIProviders(oldMaster, newMaster []byte) (map[string]secrets.RootKeyProvider, map[string]secrets.RootKeyProvider, error) {
	purposes := []string{"provider-credentials", "org-credentials", "master-kek", "dek-cache"}
	oldP := make(map[string]secrets.RootKeyProvider, len(purposes))
	newP := make(map[string]secrets.RootKeyProvider, len(purposes))
	for _, p := range purposes {
		op, err := secrets.NewStaticKeyProvider(secrets.DeriveServerKey(oldMaster, p))
		if err != nil {
			return nil, nil, err
		}
		np, err := secrets.NewStaticKeyProvider(secrets.DeriveServerKey(newMaster, p))
		if err != nil {
			return nil, nil, err
		}
		oldP[p] = op
		newP[p] = np
	}
	return oldP, newP, nil
}

// failingUpdateStore wraps a RotationStore and fails UpdateRotationRow after
// okCount successful updates — simulating the interrupted run the runbook's
// --resume-from procedure documents.
type failingUpdateStore struct {
	secrets.RotationStore
	okCount int
	failAt  int
}

func (f *failingUpdateStore) UpdateRotationRow(ctx context.Context, table, rowID string, ct []byte, ver int) error {
	if f.okCount >= f.failAt {
		return fmt.Errorf("simulated interruption at %s/%s", table, rowID)
	}
	f.okCount++
	return f.RotationStore.UpdateRotationRow(ctx, table, rowID, ct, ver)
}

// ownRowsStore scopes a real RotationStore to a fixed set of row IDs so a
// coordinator e2e is deterministic on the SHARED CI Postgres (go test runs
// the rotate-kek and migrate-kek package binaries concurrently; both suites
// seed provider_credentials). The underlying listing is still the real SQL —
// only rows this test does not own are dropped before the coordinator sees
// them.
type ownRowsStore struct {
	secrets.RotationStore
	own map[string]bool
}

func (o *ownRowsStore) ListRotationRows(ctx context.Context, table, resumeFromID string, targetVersion, limit int) ([]secrets.RotationRow, error) {
	rows, err := o.RotationStore.ListRotationRows(ctx, table, resumeFromID, targetVersion, limit)
	if err != nil {
		return nil, err
	}
	return filterRotationRows(rows, o.own), nil
}

// stripStaticPrefix unwraps the lkms:v1: prefix StaticKeyProvider adds, so
// DecryptSecret (raw AES-GCM) can verify the round-trip.
func stripStaticPrefix(t *testing.T, ct []byte) []byte {
	t.Helper()
	const prefix = "lkms:v1:"
	require.True(t, len(ct) > len(prefix) && string(ct[:len(prefix)]) == prefix, "ciphertext must carry the local-provider prefix")
	raw, err := base64.StdEncoding.DecodeString(string(ct[len(prefix):]))
	require.NoError(t, err)
	return raw
}

// TestIntegration_RotateE2E_OldToNewDecryptVerify is the issue #830 core
// scenario: an encrypted fixture row rotates old-KEK → new-KEK through the
// REAL store + coordinator, then decrypts under the new key (and fails under
// the old key).
func TestIntegration_RotateE2E_OldToNewDecryptVerify(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	oldMaster := integrationMasterKey(0x10)
	newMaster := integrationMasterKey(0x20)

	// The fixture row is encrypted exactly the way the server encrypts it:
	// purpose-scoped key derived from the master (the CLI's derivation).
	oldProvKey := secrets.DeriveServerKey(oldMaster, "provider-credentials")
	newProvKey := secrets.DeriveServerKey(newMaster, "provider-credentials")
	plaintext := []byte(`{"kind":"openai","slug":"openai","apiKey":"sk-integration"}`)
	ct, err := secrets.EncryptSecret(oldProvKey, plaintext)
	require.NoError(t, err)
	id := seedProviderCredential(t, pool, "admin", ct, 1)

	oldP, newP, err := buildCLIProviders(oldMaster, newMaster)
	require.NoError(t, err)

	scoped := &ownRowsStore{RotationStore: store, own: map[string]bool{id: true}}
	coord := secrets.NewRotationCoordinator(scoped, oldP, newP)
	res, err := coord.RotateTable(ctx, "provider_credentials", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed)
	assert.Equal(t, 0, res.Failed)
	assert.Equal(t, id, res.LastRowID, "the runbook's resume cursor must be reported")

	var gotCT []byte
	var gotVer int
	err = pool.QueryRow(ctx, `SELECT ciphertext, key_version FROM provider_credentials WHERE id = $1::uuid`, id).Scan(&gotCT, &gotVer)
	require.NoError(t, err)
	assert.Equal(t, 2, gotVer)

	// Decrypt-verify under the NEW purpose key; old key must no longer decrypt.
	dec, err := secrets.DecryptSecret(newProvKey, stripStaticPrefix(t, gotCT))
	require.NoError(t, err, "re-wrapped row must decrypt under the new KEK")
	assert.Equal(t, string(plaintext), string(dec))
	_, err = secrets.DecryptSecret(oldProvKey, stripStaticPrefix(t, gotCT))
	require.Error(t, err, "old KEK must NOT decrypt the re-wrapped row")
}

// TestIntegration_RotateE2E_DryRunNoWrites is the runbook's step-2 promise:
// --dry-run reports the rows that WILL be re-wrapped and writes nothing.
func TestIntegration_RotateE2E_DryRunNoWrites(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	oldMaster := integrationMasterKey(0x10)
	newMaster := integrationMasterKey(0x20)
	oldKey := secrets.DeriveServerKey(oldMaster, "provider-credentials")

	userID := integrationID("u")
	seedUser(t, pool, userID)

	// Three rotatable rows across two tables (org row under its own purpose).
	ids := []string{
		seedProviderCredential(t, pool, "admin", encryptFor(t, oldKey, "s1"), 1),
		seedProviderCredential(t, pool, "org", encryptFor(t, secrets.DeriveServerKey(oldMaster, "org-credentials"), "s2"), 1),
		seedAPIKey(t, pool, userID, encryptFor(t, secrets.DeriveServerKey(oldMaster, "master-kek"), "s3"), 1),
	}

	oldP, newP, err := buildCLIProviders(oldMaster, newMaster)
	require.NoError(t, err)
	scoped := &ownRowsStore{RotationStore: store, own: map[string]bool{ids[0]: true, ids[1]: true, ids[2]: true}}
	coord := secrets.NewRotationCoordinator(scoped, oldP, newP)

	results, err := coord.RotateAll(ctx, 2, true)
	require.NoError(t, err)
	assert.Equal(t, 2, results["provider_credentials"].Processed)
	assert.Equal(t, 1, results["api_keys"].Processed)

	// Nothing was written: versions still 1, ciphertexts still decrypt under old.
	// The seeded ciphertext is raw AES-GCM (no provider prefix), so it is
	// decrypted directly here.
	var ver int
	var gotCT []byte
	err = pool.QueryRow(ctx, `SELECT key_version, ciphertext FROM provider_credentials WHERE id = $1::uuid`, ids[0]).Scan(&ver, &gotCT)
	require.NoError(t, err)
	assert.Equal(t, 1, ver, "dry-run must not bump key_version")
	_, err = secrets.DecryptSecret(oldKey, gotCT)
	require.NoError(t, err, "dry-run must leave the ciphertext untouched")
	err = pool.QueryRow(ctx, `SELECT key_version FROM api_keys WHERE id = $1`, ids[2]).Scan(&ver)
	require.NoError(t, err)
	assert.Equal(t, 1, ver)
}

func encryptFor(t *testing.T, key []byte, plaintext string) []byte {
	t.Helper()
	ct, err := secrets.EncryptSecret(key, []byte(plaintext))
	require.NoError(t, err)
	return ct
}

// TestIntegration_RotateE2E_ResumeFromInterruptedRun is the runbook's
// interrupted-run procedure: a rotation killed mid-run, restarted with
// --resume-from <last-row-id>, continues without double-rotation.
func TestIntegration_RotateE2E_ResumeFromInterruptedRun(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := context.Background()

	oldMaster := integrationMasterKey(0x10)
	newMaster := integrationMasterKey(0x20)
	oldKey := secrets.DeriveServerKey(oldMaster, "provider-credentials")
	newKey := secrets.DeriveServerKey(newMaster, "provider-credentials")

	// Three rows with deterministic ordering by id ASC.
	id1 := seedProviderCredential(t, pool, "admin", encryptFor(t, oldKey, "s1"), 1)
	id2 := seedProviderCredential(t, pool, "admin", encryptFor(t, oldKey, "s2"), 1)
	id3 := seedProviderCredential(t, pool, "admin", encryptFor(t, oldKey, "s3"), 1)
	ordered := []string{id1, id2, id3}
	if ordered[0] > ordered[1] || ordered[1] > ordered[2] {
		// uuid ordering by byte value; re-sort to the store's ASC order.
		for i := 0; i < len(ordered); i++ {
			for j := i + 1; j < len(ordered); j++ {
				if ordered[i] > ordered[j] {
					ordered[i], ordered[j] = ordered[j], ordered[i]
				}
			}
		}
	}

	oldP, newP, err := buildCLIProviders(oldMaster, newMaster)
	require.NoError(t, err)

	own := map[string]bool{}
	for _, id := range ordered {
		own[id] = true
	}
	scoped := &ownRowsStore{RotationStore: store, own: own}

	// Interrupt after the first row is updated.
	interrupted := &failingUpdateStore{RotationStore: scoped, failAt: 1}
	coord := secrets.NewRotationCoordinator(interrupted, oldP, newP)
	res, err := coord.RotateTable(ctx, "provider_credentials", "", 2, false)
	require.NoError(t, err, "per-row update failures are reported in the result, not as a call error")
	assert.Equal(t, 1, res.Processed)
	assert.Equal(t, 2, res.Failed, "remaining rows fail because the interruption repeats")
	require.NotEmpty(t, res.LastRowID, "the interrupted run must report its resume cursor")
	assert.Equal(t, ordered[0], res.LastRowID, "resume cursor is the first row in id order")

	// Restart with --resume-from <last-row-id> — exactly what the runbook says.
	resumed := secrets.NewRotationCoordinator(scoped, oldP, newP)
	res2, err := resumed.RotateTable(ctx, "provider_credentials", res.LastRowID, 2, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res2.Processed, "the resumed run must finish the remaining rows")
	assert.Equal(t, 0, res2.Failed)
	assert.Equal(t, ordered[2], res2.LastRowID)

	// No double-rotation: every row is at v2 and decrypts under the NEW key.
	for _, id := range ordered {
		var ver int
		var gotCT []byte
		err := pool.QueryRow(ctx, `SELECT key_version, ciphertext FROM provider_credentials WHERE id = $1::uuid`, id).Scan(&ver, &gotCT)
		require.NoError(t, err)
		assert.Equal(t, 2, ver, "row %s must be exactly once rotated", id)
		_, err = secrets.DecryptSecret(newKey, stripStaticPrefix(t, gotCT))
		assert.NoError(t, err, "row %s must decrypt under the new KEK", id)
		_, err = secrets.DecryptSecret(oldKey, stripStaticPrefix(t, gotCT))
		assert.Error(t, err, "row %s must NOT decrypt under the old KEK", id)
	}
}

// TestIntegration_SingleTableRunFlushesDEKCache is the PR #1409 review
// Finding 1 regression: the runbook's interrupted-run recovery is a
// PER-TABLE run, and the docs promise the Redis DEK cache is flushed
// automatically on success — for every invocation shape. Drives the real
// run() (the CLI's own code path) with --table and --redis-url against the
// live Postgres + a miniredis, and asserts the stale DEK keys are gone.
func TestIntegration_SingleTableRunFlushesDEKCache(t *testing.T) {
	_, pool := newIntegrationStore(t)

	oldMaster := integrationMasterKey(0x10)
	newMaster := integrationMasterKey(0x20)
	oldKey := secrets.DeriveServerKey(oldMaster, "master-kek")

	dir := t.TempDir()
	oldFile := filepath.Join(dir, "old.key")
	newFile := filepath.Join(dir, "new.key")
	require.NoError(t, os.WriteFile(oldFile, []byte(hex.EncodeToString(oldMaster)), 0o600))
	require.NoError(t, os.WriteFile(newFile, []byte(hex.EncodeToString(newMaster)), 0o600))

	userID := integrationID("u")
	seedUser(t, pool, userID)
	seedAPIKey(t, pool, userID, encryptFor(t, oldKey, "flush-me"), 1)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	require.NoError(t, rc.Set(context.Background(), "dek:stale-session", "wrapped", time.Hour).Err())
	require.NoError(t, rc.Set(context.Background(), "ratelimit:keep", "1", time.Hour).Err())

	// The exact invocation shape of the runbook's resume procedure (minus
	// --resume-from, which is orthogonal to the flush).
	err = run(oldFile, newFile, testDSN(), "redis://"+mr.Addr(), "api_keys", "", 2, false)
	require.NoError(t, err, "per-table run must succeed and flush")

	assert.False(t, mr.Exists("dek:stale-session"), "a successful per-table run must flush the DEK cache")
	assert.True(t, mr.Exists("ratelimit:keep"), "non-dek keys must survive the flush")

	// The dry-run counterpart: no writes of any kind, flush included.
	require.NoError(t, rc.Set(context.Background(), "dek:dryrun-session", "wrapped", time.Hour).Err())
	err = run(oldFile, newFile, testDSN(), "redis://"+mr.Addr(), "api_keys", "", 2, true)
	require.NoError(t, err)
	assert.True(t, mr.Exists("dek:dryrun-session"), "a dry-run must not flush (or write) anything")
	_ = rc.Close()
}

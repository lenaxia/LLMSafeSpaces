// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testharness is the single source of Postgres + Redis wiring for the
// API service's integration tests.
//
// It consolidates the two near-identical pool constructors duplicated across
// api/internal/services/database (getIntegrationPool, newIntegrationDB) and
// adds the migration runner the codebase was missing — so an integration test
// never has to reinvent Postgres/Redis setup or assume the schema is
// pre-migrated. The third constructor, getTestPool in pkg/secrets, cannot be
// consolidated here because Go's internal-package visibility forbids pkg/
// imports of api/internal/; that duplication is documented and deferred (see
// README.md).
//
// # Isolation model
//
// The harness connects to a single, shared, externally-provisioned test
// Postgres (TEST_DATABASE_URL; skipped if unreachable). This matches the
// project's existing integration-test contract. Per-test isolation follows the
// project convention: use unique IDs/markers per test so parallel tests do not
// collide. Reset() is provided for non-parallel tests that need a clean slate;
// it never touches schema_migrations.
//
// See README.md in this package for when to use the harness vs. unit-test
// mocks.
package testharness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lenaxia/llmsafespaces/api/migrations"
)

const (
	envTestDatabaseURL = "TEST_DATABASE_URL"
	defaultTestDSN     = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	testCtxTimeout     = 30 * time.Second
	connectTimeout     = 10 * time.Second
	migrateTimeout     = 60 * time.Second
)

// reservedTables are never truncated by Reset. schema_migrations is
// golang-migrate's version bookkeeping; wiping it forces re-application or
// marks the DB dirty.
var reservedTables = map[string]bool{"schema_migrations": true}

// Harness holds the Postgres and Redis handles for one integration test.
//
// Construct with New; never zero-value it. New registers a t.Cleanup that
// closes every handle, so tests do not manage teardown themselves.
type Harness struct {
	t        *testing.T
	id       string
	dsn      string
	pool     *pgxpool.Pool
	sqlDB    *sql.DB
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	logger   *zap.Logger
	logs     *observer.ObservedLogs
	rootCtx  context.Context
	rootStop context.CancelFunc
	closed   bool
}

// New connects to the test Postgres and a fresh miniredis. If Postgres is
// unreachable it skips the calling test (matching the project's existing
// integration-test contract, so a dev without Docker still gets a green
// `go test ./...`). Migrations are applied if the schema is not current.
//
// All handles are torn down via t.Cleanup.
func New(t *testing.T) *Harness {
	t.Helper()

	h := &Harness{
		t:   t,
		dsn: resolveDSN(),
		id:  newID(),
	}
	h.rootCtx, h.rootStop = context.WithCancel(context.Background())

	// Postgres pool (pgx). Construction is lazy; Ping probes reachability.
	pool, err := pgxpool.New(context.Background(), h.dsn)
	if err != nil {
		t.Skipf("testharness: cannot construct pgx pool (%s): %v", h.dsn, err)
	}
	pingCtx, pingCancel := context.WithTimeout(h.rootCtx, connectTimeout)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("testharness: Postgres unreachable at %s — set %s to run integration tests: %v",
			h.dsn, envTestDatabaseURL, err)
	}
	h.pool = pool

	// database/sql handle (pgx driver) for stores that use database/sql
	// (e.g. the email-token store) and as the migrate connection below.
	sqlDB, err := sql.Open("pgx", h.dsn)
	if err != nil {
		h.Close()
		t.Fatalf("testharness: cannot open *sql.DB: %v", err)
	}
	// Assign before the reachability check so a ping failure routes the
	// handle through h.Close() rather than leaking it.
	h.sqlDB = sqlDB
	if err := sqlDB.PingContext(pingCtx); err != nil {
		h.Close()
		t.Fatalf("testharness: *sql.DB ping failed: %v", err)
	}

	// Ensure the schema is current. Idempotent: a no-op if already migrated.
	if err := h.MigrateUp(); err != nil {
		h.Close()
		t.Fatalf("testharness: cannot apply migrations: %v", err)
	}

	// Isolated in-memory Redis.
	mr, err := miniredis.Run()
	if err != nil {
		h.Close()
		t.Fatalf("testharness: cannot start miniredis: %v", err)
	}
	h.mr = mr
	h.rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})

	h.logger, h.logs = newLogger()

	t.Cleanup(h.Close)
	return h
}

// Close releases every handle. Idempotent. New registers this as the test's
// t.Cleanup; tests may also call it directly to verify teardown behavior.
func (h *Harness) Close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.rootStop != nil {
		h.rootStop()
	}
	if h.rdb != nil {
		_ = h.rdb.Close()
	}
	if h.mr != nil {
		h.mr.Close()
	}
	if h.pool != nil {
		h.pool.Close()
	}
	if h.sqlDB != nil {
		_ = h.sqlDB.Close()
	}
}

// Pool returns a pgx connection pool to the test Postgres.
func (h *Harness) Pool() *pgxpool.Pool { return h.pool }

// SQLDB returns a database/sql handle to the same Postgres, for stores built
// on database/sql (e.g. the email-token store). It is a distinct handle from
// the one used internally for migrations.
func (h *Harness) SQLDB() *sql.DB { return h.sqlDB }

// Redis returns a redis client backed by the harness's isolated miniredis.
func (h *Harness) Redis() *redis.Client { return h.rdb }

// Miniredis returns the underlying miniredis for advanced assertions (TTL,
// key scans) that the redis client cannot express.
func (h *Harness) Miniredis() *miniredis.Miniredis { return h.mr }

// DSN returns the resolved connection string, for tests that construct their
// own pool (e.g. to exercise pool-level behavior).
func (h *Harness) DSN() string { return h.dsn }

// ID returns a short, process-unique identifier for this harness instance, for
// generating unique test markers (the project's parallel-isolation convention).
func (h *Harness) ID() string { return h.id }

// Logger returns a zap.Logger whose output is captured in memory. Assert on
// emitted entries via Logs().
func (h *Harness) Logger() *zap.Logger { return h.logger }

// Logs returns the captured log entries for log-based assertions (e.g. "no
// ERROR was emitted").
func (h *Harness) Logs() *observer.ObservedLogs { return h.logs }

// NewContext returns a context carrying a test-scoped deadline, derived from
// the harness root context so Close cancels it. Each call's cancel is wired to
// t.Cleanup so timers never leak.
func (h *Harness) NewContext() context.Context {
	ctx, cancel := context.WithTimeout(h.rootCtx, testCtxTimeout)
	h.t.Cleanup(cancel)
	return ctx
}

// MigrateUp applies all pending migrations. It is idempotent: a no-op (returns
// nil) when the schema is already current.
func (h *Harness) MigrateUp() error {
	return h.runMigrate(func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		return nil
	})
}

// MigrateDown reverts all migrations. Use only against a throwaway database;
// on the shared test DB it destroys the schema for every other test.
func (h *Harness) MigrateDown() error {
	return h.runMigrate(func(m *migrate.Migrate) error {
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		return nil
	})
}

// runMigrate opens a dedicated *sql.DB for golang-migrate so the migrate
// lifecycle (which may close its own connection) never affects the handles
// exposed by Pool()/SQLDB().
func (h *Harness) runMigrate(fn func(*migrate.Migrate) error) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("testharness: iofs source: %w", err)
	}
	migDB, err := sql.Open("pgx", h.dsn)
	if err != nil {
		return fmt.Errorf("testharness: open migrate db: %w", err)
	}
	defer func() { _ = migDB.Close() }()

	drv, err := pgxdriver.WithInstance(migDB, &pgxdriver.Config{})
	if err != nil {
		return fmt.Errorf("testharness: pgx/v5 migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx", drv)
	if err != nil {
		return fmt.Errorf("testharness: migrate instance: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	// golang-migrate does not accept a context; its operations run against the
	// dedicated migDB connection and complete quickly for the test schema. The
	// root context cancellation from Close() does not interrupt a migrate in
	// flight, but migrate is not a long-running operation in practice.
	if err := fn(m); err != nil {
		return fmt.Errorf("testharness: migrate op: %w", err)
	}
	return nil
}

// Reset truncates every user table in the public schema, restarting identity
// sequences, and never touches schema_migrations. Use it in non-parallel tests
// that need a clean slate; for parallel tests, prefer unique per-test IDs
// (see ID()).
func (h *Harness) Reset() error {
	ctx, cancel := context.WithTimeout(h.rootCtx, migrateTimeout)
	defer cancel()

	rows, err := h.pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		return fmt.Errorf("testharness: list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("testharness: scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("testharness: iterate tables: %w", err)
	}

	stmt, err := buildTruncateSQL(tables)
	if err != nil {
		return err
	}
	if _, err := h.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("testharness: truncate: %w", err)
	}
	return nil
}

// Seed inserts one row into the named table from a column→value map and
// returns the value of its "id" column. It is a thin convenience over Pool()
// for the common "insert a user/workspace/org, get its id" shape; it does not
// model relationships or arbitrary primary-key names.
func (h *Harness) Seed(table string, row map[string]any) string {
	query, args, err := buildSeedSQL(table, row)
	if err != nil {
		h.t.Fatalf("testharness.Seed(%q): %v", table, err)
	}
	var id string
	if err := h.pool.QueryRow(h.NewContext(), query, args...).Scan(&id); err != nil {
		h.t.Fatalf("testharness.Seed(%q): %v", table, err)
	}
	return id
}

// --- pure helpers (unit-tested in harness_unit_test.go) ---

func resolveDSN() string {
	if v := os.Getenv(envTestDatabaseURL); v != "" {
		return v
	}
	return defaultTestDSN
}

// MigrationFiles returns the embedded migration file names (the .up.sql set),
// sorted ascending by name so callers see versions in apply order.
func MigrationFiles() ([]string, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("testharness: read embedded migrations: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func filterUserTables(tables []string) []string {
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		if reservedTables[strings.ToLower(t)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func buildTruncateSQL(tables []string) (string, error) {
	tables = filterUserTables(tables)
	if len(tables) == 0 {
		return "", errors.New("testharness: no user tables to truncate")
	}
	quoted := make([]string, len(tables))
	for i, t := range tables {
		// Quote the identifier exactly as information_schema returns it (the
		// canonical stored form); do not mangle its case.
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return "TRUNCATE TABLE " + strings.Join(quoted, ", ") + " RESTART IDENTITY CASCADE", nil
}

func buildSeedSQL(table string, row map[string]any) (string, []any, error) {
	if table == "" {
		return "", nil, errors.New("testharness: empty table name")
	}
	if len(row) == 0 {
		return "", nil, errors.New("testharness: empty row")
	}
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	placeholders := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = row[c]
	}
	return fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s) RETURNING "id"`,
			strings.ReplaceAll(table, `"`, `""`),
			quoteIdentifiers(cols),
			strings.Join(placeholders, ", ")),
		args, nil
}

// quoteIdentifiers quotes each identifier for SQL, doubling any embedded `"`,
// matching the escaping already applied to table names. Column names here come
// from the test's row map (developer-controlled), but applying the same escape
// keeps buildSeedSQL internally consistent and robust.
func quoteIdentifiers(cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	return strings.Join(quoted, `, `)
}

func newLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

var idCounter uint64

func newID() string {
	return fmt.Sprintf("h%d", atomic.AddUint64(&idCounter, 1))
}

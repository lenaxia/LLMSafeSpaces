// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// migration_000052_populated_test.go — the populated-database migration
// pin (review round 2's reproduction class): CI's idempotency and
// round-trip gates run migrations on EMPTY schemas, so a migration whose
// constraint swaps validate before its rehome UPDATEs passes CI and dies
// in production on exactly the rows it exists to migrate. This test
// parks a THROWAWAY database at 000051, seeds the old world
// (stage='pending_input' rows — escalated + deliberate parks), steps
// across 000052 up, asserts the rehome shapes, steps back down, and
// asserts the reversal. Skips without TEST_DATABASE_URL.
package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	"github.com/lenaxia/llmsafespaces/api/migrations"
)

// scratchDB mints (and registers cleanup for) a throwaway database on
// the TEST_DATABASE_URL instance; the populated-migration tests must
// never touch the shared schema.
func scratchDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — populated-migration pin skipped")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("populated-migration pin skipped: %v", err)
	}
	name := "migtest_populated_52_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
		_ = admin.Close(ctx)
	})
	// Re-point the DSN at the scratch database (keep the params).
	base := dsn
	if i := strings.LastIndex(base, "/"); i >= 0 {
		q := ""
		if j := strings.Index(base[i:], "?"); j >= 0 {
			q = base[i+j:]
			base = base[:i+j]
		}
		base = base[:i+1] + name + q
	}
	return base
}

func migrateAt(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx", mustDriver(t, dsn))
	if err != nil {
		t.Fatalf("migrate instance: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func TestMigration000052_PopulatedPreStateUpAndDown(t *testing.T) {
	dsn := scratchDB(t)
	m := migrateAt(t, dsn)

	// Park at 000051 (the pre-seamless-lease world: pending_input is a
	// STAGE, paired with pending_origin).
	if err := m.Migrate(51); err != nil {
		t.Fatalf("park at 51: %v", err)
	}
	ctx := context.Background()
	pool, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = pool.Close(ctx) }()

	seed := `
	INSERT INTO users (id, username, email, password_hash, active, role)
	VALUES ('mig-u1', 'mig-u1', 'mig@example.com', 'h', true, 'user');
	INSERT INTO workspaces (id, user_id, name, namespace)
	VALUES ('11111111-1111-1111-1111-111111111111', 'mig-u1', 'mig-ws', 'mig-ns');
	INSERT INTO projects (id, short_id, user_id, name, target_workspace_id)
	VALUES ('22222222-2222-2222-2222-222222222222', 'prj-migrated', 'mig-u1', 'mig-p',
	        '11111111-1111-1111-1111-111111111111');
	INSERT INTO epics (id, short_id, project_id, title, project_name, status)
	VALUES ('33333333-3333-3333-3333-333333333333', 'epc-migrated',
	        '22222222-2222-2222-2222-222222222222', 'E', 'mig-p', 'active');
	INSERT INTO tasks (id, short_id, project_id, epic_id, title, project_name, epic_name, stage, pending_origin)
	VALUES
	  ('44444444-4444-4444-4444-444444444001', 'tsk-migrated', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'escalated park', 'mig-p', 'E', 'pending_input', 'escalated'),
	  ('55555555-5555-5555-5555-555555555555', 'tsk-migrate2', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'deliberate park', 'mig-p', 'E', 'pending_input', 'worker'),
	  ('66666666-6666-6666-6666-666666666666', 'tsk-migrate3', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'plain backlog', 'mig-p', 'E', 'backlog', NULL);`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed old world: %v", err)
	}

	// --- UP across 000052 on the POPULATED schema ---------------------
	if err := m.Steps(1); err != nil {
		t.Fatalf("step up across 000052 on a populated schema (the review's reproduction): %v", err)
	}
	row := func(id, q string, args ...any) string {
		var s string
		if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT %s FROM tasks WHERE id='%s'", q, id), args...).Scan(&s); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return s
	}
	if got := row("44444444-4444-4444-4444-444444444001",
		"stage || ' flag=' || pending_input::text || ' origin=' || coalesce(pending_origin,'-')"); got != "failed flag=false origin=escalated" {
		t.Fatalf("escalated park rehomed: got %q", got)
	}
	if got := row("55555555-5555-5555-5555-555555555555",
		"stage || ' flag=' || pending_input::text || ' origin=' || coalesce(pending_origin,'-')"); got != "in_progress flag=true origin=worker" {
		t.Fatalf("deliberate park rehomed: got %q", got)
	}
	if got := row("66666666-6666-6666-6666-666666666666",
		"stage || ' flag=' || pending_input::text || ' origin=' || coalesce(pending_origin,'-')"); got != "backlog flag=false origin=-" {
		t.Fatalf("untouched row unchanged: got %q", got)
	}

	// A NEW-world-only shape the downgrade must also carry: failed with
	// NO origin (the manager move and the nudge self-fail — legal under
	// the new paired-form constraint, unreachable in the old world).
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (id, short_id, project_id, epic_id, title, project_name, epic_name, stage)
		VALUES ('77777777-7777-7777-7777-777777777777', 'tsk-migrate4',
		        '22222222-2222-2222-2222-222222222222', '33333333-3333-3333-3333-333333333333',
		        'manager-failed', 'mig-p', 'E', 'failed')`); err != nil {
		t.Fatalf("seed new-world failed row: %v", err)
	}

	// --- DOWN from the POPULATED new world ----------------------------
	if err := m.Steps(-1); err != nil {
		t.Fatalf("step down from 000052 on a populated schema: %v", err)
	}
	for id, want := range map[string]string{
		"44444444-4444-4444-4444-444444444001": "pending_input|escalated",
		"55555555-5555-5555-5555-555555555555": "pending_input|worker",
		"66666666-6666-6666-6666-666666666666": "backlog|-",
		// The old vocabulary has no failed stage: the origin-less failed
		// row re-enters at backlog (the review round-4 reproduction).
		"77777777-7777-7777-7777-777777777777": "backlog|-",
	} {
		var got string
		if err := pool.QueryRow(ctx,
			"SELECT stage || '|' || coalesce(pending_origin,'-') FROM tasks WHERE id=$1", id).Scan(&got); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("down rehome %s: got %q want %q", id, got, want)
		}
	}
	// The rehome also stamps the failure overlay — assert it, so
	// deleting the last_attempt_failed clause from the down turns this
	// pin red (review round 5's one-line gap).
	var failedFlag bool
	if err := pool.QueryRow(ctx,
		`SELECT last_attempt_failed FROM tasks WHERE id=$1`,
		"77777777-7777-7777-7777-777777777777").Scan(&failedFlag); err != nil {
		t.Fatalf("read rehomed failed row: %v", err)
	}
	if !failedFlag {
		t.Fatalf("rehomed origin-less failed row must carry last_attempt_failed=true")
	}

	// Re-up on the already-migrated (and now down-graded) populated
	// schema: the operator-retry shape (idempotent re-application after
	// a partial episode).
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-up across 000052 after the down: %v", err)
	}
	if got := row("44444444-4444-4444-4444-444444444001",
		"stage || ' flag=' || pending_input::text || ' origin=' || coalesce(pending_origin,'-')"); got != "failed flag=false origin=escalated" {
		t.Fatalf("re-up escalated rehome: got %q", got)
	}
	if got := row("55555555-5555-5555-5555-555555555555",
		"stage || ' flag=' || pending_input::text || ' origin=' || coalesce(pending_origin,'-')"); got != "in_progress flag=true origin=worker" {
		t.Fatalf("re-up park rehome: got %q", got)
	}
}

// mustDriver builds the golang-migrate pgx driver over a dedicated
// database/sql handle (the harness pattern — the migrate lifecycle
// closes its own connections).
func mustDriver(t *testing.T, dsn string) database.Driver {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open migrate db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	drv, err := pgxdriver.WithInstance(sqlDB, &pgxdriver.Config{})
	if err != nil {
		t.Fatalf("pgx migrate driver: %v", err)
	}
	return drv
}

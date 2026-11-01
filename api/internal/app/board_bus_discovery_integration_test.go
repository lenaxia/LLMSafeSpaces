// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_bus_discovery_integration_test.go — review r6's missing-test 1
// (the NULL-blackout companion): the ONLY test that executes the REAL
// discovery SQL (every other bus test fakes the discovery read), and
// the one that proves a NULL projects.target_workspace_id row — the
// column is nullable (migration 000036; create validates, the schema
// does not) — cannot take the GLOBAL SSE-join join down. Pre-fix, the
// NULL fails the scan, syncOnce errors every cycle, and every
// workspace's consumer holds stale frames forever while publishes keep
// landing.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func discoveryTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegrationBoardBusDiscovery_NullTargetIsNotAGlobalBlackout(t *testing.T) {
	pool := discoveryTestPool(t)
	ctx := context.Background()

	owner := "user-bus-discovery-" + uuid.NewString()[:8]
	ws := uuid.NewString()
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1::text, $1::text, $1::text || '@example.com', 'hash', true, 'user')`, owner)
	require.NoError(t, err, "users row must exist (FK target)")
	// ONE ordered cleanup (review r7's leak finding): t.Cleanup is LIFO,
	// and split cleanups registered in creation order run REVERSED —
	// the workspaces DELETE would fire while projects still reference
	// it (FK NO ACTION), fail silently, and leak rows into the shared
	// test DB every run. Children first, exactly one func.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM projects WHERE user_id=$1`, owner)
		_, _ = pool.Exec(cctx, `DELETE FROM workspaces WHERE id=$1`, ws)
		_, _ = pool.Exec(cctx, `DELETE FROM users WHERE id=$1`, owner)
	})

	// One healthy target...
	_, err = pool.Exec(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, $2::text, 'bus-discovery', 'default', now(), now())`, ws, owner)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO projects (short_id, user_id, name, target_workspace_id, worker_slots, status, created_at, updated_at)
		VALUES ('prj-' || substr(gen_random_uuid()::text, 1, 8), $1, 'bus-discovery-p', $2::uuid, 2, 'active', now(), now())`, owner, ws)
	require.NoError(t, err)

	// ...and one NULL target (the pre-validation row shape): pre-fix,
	// THIS row's scan failure disabled discovery for EVERY workspace.
	_, err = pool.Exec(ctx, `
		INSERT INTO projects (short_id, user_id, name, target_workspace_id, worker_slots, status, created_at, updated_at)
		VALUES ('prj-' || substr(gen_random_uuid()::text, 1, 8), $1, 'bus-discovery-null', NULL, 2, 'active', now(), now())`, owner)
	require.NoError(t, err)

	got, err := DiscoverBoardWorkspaces(pool)(ctx)
	require.NoError(t, err, "the NULL row must not fail the scan — one bad row never blacks out every workspace's join")
	require.Contains(t, got, ws, "the healthy workspace still discovers")
	for _, id := range got {
		require.NotEmpty(t, id, "NULL targets render as nothing (filtered, never scanned as errors)")
	}
}

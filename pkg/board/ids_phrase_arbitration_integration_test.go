// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// #1831 phrase-id arbitration pin (in-package): the collision ladder is
// deterministic against REAL Postgres existence checks. In-package so
// the seam under test (mintUnique/arbitrateMint) is reachable with a
// FORCED base phrase — the random draw cannot be steered through the
// service, and a probabilistic hammer would not pin the ladder.
package board

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func phraseArbPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

// TestMintUniqueOwnerWideLadder forces the deterministic ladder: with
// the base phrase and its -2 suffixed form already held by the owner
// (in a SIBLING project — the per-project constraint permits it, the
// owner-wide mint check is the guard), the next arbitration of that
// base must land on base-3. A check scoped to the minting project only
// would return the BASE (collision invisible) — this pin reddens
// exactly that regression.
func TestMintUniqueOwnerWideLadder(t *testing.T) {
	pool := phraseArbPool(t)
	defer pool.Close()
	ctx := context.Background()

	cleanup := func() {
		_, _ = pool.Exec(ctx, `TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects CASCADE`)
		_, _ = pool.Exec(ctx, `INSERT INTO users (id, username, email, password_hash, active, role)
			VALUES ('phrase-arb-user', 'phrase-arb-user', 'phrase-arb@example.com', 'hash', true, 'user')
			ON CONFLICT (id) DO NOTHING`)
	}
	cleanup()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects CASCADE`)
	})

	const owner = "phrase-arb-user"
	_, err := pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ('phrase-arb-ws', $1, 'phrase-arb', 'default', now(), now())`, owner)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	// Two sibling projects of the SAME owner (the shape that makes
	// per-project uniqueness insufficient for the resolution perimeter).
	for _, pid := range []string{"phrase-arb-p1", "phrase-arb-p2"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO projects (id, short_id, user_id, name, target_workspace_id, worker_slots, tags)
			VALUES ($1, $2, $3, $1, 'phrase-arb-ws'::uuid, 2, '{}')`,
			pid, "prj-arb"+pid[len(pid)-2:], owner)
		if err != nil {
			t.Fatalf("seed project %s: %v", pid, err)
		}
	}
	// The project mints must not collide with these seeds — arbitrary
	// fixed prj- ids are fine (arbitration targets tasks here).
	seedTask := func(id, project, shortID string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO tasks (id, project_id, short_id, title, description, project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1, $2, $3, 'seed', 'd', 'p', 'e', '{}', '[]')`, id, project, shortID)
		if err != nil {
			t.Fatalf("seed task %s: %v", shortID, err)
		}
	}
	const base = PrefixTask + "amber-falcon-glides"
	seedTask("phrase-arb-t0", "phrase-arb-p1", base)      // owner holds the base
	seedTask("phrase-arb-t1", "phrase-arb-p2", base+"-2") // …and the -2 rung (sibling project)
	_ = fmt.Sprint()

	// The owner-wide task-mint scope: the candidate is taken if ANY of
	// the owner's projects holds it.
	got, err := mintUnique(ctx, pool, "tasks", PrefixTask, taskMintOwnerScope, owner)
	if err != nil {
		t.Fatalf("mintUnique: %v", err)
	}
	if want := base + "-3"; got != want {
		t.Fatalf("owner-wide ladder: forced base %s with base and -2 held must mint %s, got %s", base, want, got)
	}

	// A fresh base (nothing held) returns the base itself.
	fresh, err := mintUnique(ctx, pool, "tasks", PrefixTask, taskMintOwnerScope, owner)
	if err != nil {
		t.Fatalf("mintUnique fresh: %v", err)
	}
	if fresh == base || fresh == base+"-2" || fresh == base+"-3" {
		t.Fatalf("fresh draw collided with the seeded ladder impossibly: %s", fresh)
	}
	if !IsPhraseIDForm(fresh) {
		t.Fatalf("fresh draw is phrase-form, got %s", fresh)
	}
}

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
		// The fixed-UUID workspace row must go too — a second run on the
		// same database collides on workspaces_pkey otherwise (the
		// run-twice flake; CI's fresh container hid it).
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id='11111111-1111-4111-8111-111111111111'`)
		_, _ = pool.Exec(ctx, `INSERT INTO users (id, username, email, password_hash, active, role)
			VALUES ('phrase-arb-user', 'phrase-arb-user', 'phrase-arb@example.com', 'hash', true, 'user')
			ON CONFLICT (id) DO NOTHING`)
	}
	cleanup()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects CASCADE`)
	})

	const owner = "phrase-arb-user"
	// Fixed VALID uuids (the ids are uuid columns — the first CI run
	// reddened on literal seeds like "phrase-arb-ws", SQLSTATE 22P02).
	const arbWS = "11111111-1111-4111-8111-111111111111"
	const arbP1 = "22222222-2222-4222-8222-222222222221"
	const arbP2 = "22222222-2222-4222-8222-222222222222"
	const arbE1 = "44444444-4444-4444-8444-444444444441" // epic in p1 (tasks.epic_id NOT NULL)
	const arbE2 = "44444444-4444-4444-8444-444444444442" // epic in p2 (FK: epic must sit in the task's own project)
	_, err := pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1, $2, 'phrase-arb', 'default', now(), now())`, arbWS, owner)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	// Two sibling projects of the SAME owner (the shape that makes
	// per-project uniqueness insufficient for the resolution perimeter).
	for _, pid := range []string{arbP1, arbP2} {
		_, err := pool.Exec(ctx, `
			INSERT INTO projects (id, short_id, user_id, name, target_workspace_id, worker_slots, tags)
			VALUES ($1::uuid, $2, $3, $1::text, $4::uuid, 2, '{}')`,
			pid, "prj-arb0000"+pid[len(pid)-1:], owner, arbWS)
		if err != nil {
			t.Fatalf("seed project %s: %v", pid, err)
		}
	}
	for _, e := range []struct{ id, project, shortID string }{
		{arbE1, arbP1, "epc-arb00001"}, {arbE2, arbP2, "epc-arb00002"},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, project_name)
			VALUES ($1::uuid, $2::uuid, $3, 'arb epic', 'p')`, e.id, e.project, e.shortID)
		if err != nil {
			t.Fatalf("seed epic %s: %v", e.shortID, err)
		}
	}
	// The project mints must not collide with these seeds — arbitrary
	// fixed prj- ids are fine (arbitration targets tasks here).
	seedTask := func(id, project, epic, shortID string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description, project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'seed', 'd', 'p', 'e', '{}', '[]')`, id, project, epic, shortID)
		if err != nil {
			t.Fatalf("seed task %s: %v", shortID, err)
		}
	}
	const base = PrefixTask + "amber-falcon-glides"
	seedTask("33333333-3333-4333-8333-333333333331", arbP1, arbE1, base)      // owner holds the base
	seedTask("33333333-3333-4333-8333-333333333332", arbP2, arbE2, base+"-2") // …and the -2 rung (sibling project)

	// The deterministic ladder pin: arbitrateMint walks base → base-2 →
	// base-3 against the owner-wide scope directly (mintUnique's draw
	// is random and cannot be steered to the seeded base — the review
	// round caught the original mintUnique-call asserting a forced
	// base it would essentially never draw).
	got, err := arbitrateMint(ctx, pool, "tasks", base, taskMintOwnerScope, owner)
	if err != nil {
		t.Fatalf("arbitrateMint: %v", err)
	}
	if want := base + "-3"; got != want {
		t.Fatalf("owner-wide ladder: forced base %s with base and -2 held (sibling project) must arbitrate to %s, got %s", base, want, got)
	}
	// A per-project-scoped check would return base-2 — the rung the
	// owner ALREADY holds in the sibling project (the collision is
	// invisible inside p1; that silently-wrong id is the exact
	// regression the owner-wide scope exists to prevent).
	gotScoped, err := arbitrateMint(ctx, pool, "tasks", base, ` AND project_id=$2::uuid`, arbP1)
	if err != nil {
		t.Fatalf("arbitrateMint scoped: %v", err)
	}
	if gotScoped != base+"-2" {
		t.Fatalf("sanity: p1-scoped arbitration must hand out the taken base-2 (collision invisible), got %s", gotScoped)
	}

	// The full mint path on the same scope draws fresh bases (never the
	// seeded rungs) and every draw is phrase-form.
	for i := 0; i < 5; i++ {
		fresh, err := mintUnique(ctx, pool, "tasks", PrefixTask, taskMintOwnerScope, owner)
		if err != nil {
			t.Fatalf("mintUnique fresh: %v", err)
		}
		if fresh == base || fresh == base+"-2" || fresh == base+"-3" {
			t.Fatalf("fresh draw hit the seeded ladder impossibly: %s", fresh)
		}
		if !IsPhraseIDForm(fresh) {
			t.Fatalf("fresh draw is phrase-form, got %s", fresh)
		}
	}
}

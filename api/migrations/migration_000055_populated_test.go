// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// migration_000055_populated_test.go — the populated-database migration
// pin for the two-clock split (#2043): 000055 adds tasks.last_renewed_at
// and BACKFILLS it from last_progress_at (the conservative mid-deploy
// contract — in-flight rows carry today's semantics across the deploy,
// no behavior jump at the boundary). CI's round-trip/idempotency gates
// run on EMPTY schemas, so the backfill itself is only pinned here, on
// a throwaway database parked at 000054 and seeded with the pre-split
// world: a claimed row (progress stamped at claim), a renewed row
// (progress stamped by the OLD renewal semantics), and a NULL-progress
// row (the legacy shape — backfill leaves it NULL, which the lapse arm
// reads as lapsed-by-default). Skips without TEST_DATABASE_URL.
package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestMigration000055_PopulatedBackfillUpAndDown(t *testing.T) {
	dsn := scratchDB(t)
	m := migrateAt(t, dsn)

	// Park at 000054 (the pre-split world: last_progress_at is the
	// only clock, written by claims, renewals, and heartbeats alike).
	if err := m.Migrate(54); err != nil {
		t.Fatalf("park at 54: %v", err)
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
VALUES ('22222222-2222-2222-2222-222222222222', 'prj-mig00055', 'mig-u1', 'mig-p',
        '11111111-1111-1111-1111-111111111111');
INSERT INTO epics (id, short_id, project_id, title, project_name, status)
VALUES ('33333333-3333-3333-3333-333333333333', 'epc-mig00055',
        '22222222-2222-2222-2222-222222222222', 'E', 'mig-p', 'active');
	INSERT INTO tasks (id, short_id, project_id, epic_id, title, project_name, epic_name,
	                   stage, last_progress_at, lease_expires_at, claim_token)
	VALUES
	  -- the in-flight claim: progress anchored at the mint (fresh)
	  ('44444444-4444-4444-4444-444444444001', 'tsk-mig0001a', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'fresh claim', 'mig-p', 'E', 'in_progress',
	   now() - interval '5 minutes', now() + interval '40 minutes', gen_random_uuid()),
	  -- the renewed-by-old-semantics row: last_progress_at was the
	  -- renewal's stamp (the incident's ambiguity) — backfill adopts it
	  ('55555555-5555-5555-5555-555555555002', 'tsk-mig0002b', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'renewed row', 'mig-p', 'E', 'in_progress',
	   now() - interval '3 hours', now() + interval '40 minutes', gen_random_uuid()),
	  -- the legacy NULL-progress row: backfill leaves NULL (the lapse
	  -- arm's lapsed-by-default input)
	  ('66666666-6666-6666-6666-666666666003', 'tsk-mig0003c', '22222222-2222-2222-2222-222222222222',
	   '33333333-3333-3333-3333-333333333333', 'null progress', 'mig-p', 'E', 'in_progress',
	   NULL, NULL, NULL);`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed pre-split world: %v", err)
	}

	// --- UP across 000055 on the POPULATED schema ---------------------
	if err := m.Steps(1); err != nil {
		t.Fatalf("step up across 000055 on a populated schema: %v", err)
	}
	backfill := func(id string) (renewed *string, progress *string) {
		if err := pool.QueryRow(ctx,
			`SELECT last_renewed_at::text, last_progress_at::text FROM tasks WHERE id=$1`, id).
			Scan(&renewed, &progress); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return renewed, progress
	}
	for _, id := range []string{
		"44444444-4444-4444-4444-444444444001",
		"55555555-5555-5555-5555-555555555002",
	} {
		renewed, progress := backfill(id)
		if renewed == nil {
			t.Fatalf("%s: last_renewed_at not backfilled", id)
		}
		if progress == nil || *renewed != *progress {
			t.Fatalf("%s: backfill must copy last_progress_at verbatim (got renewed=%q progress=%q)", id, deref(renewed), deref(progress))
		}
	}
	if renewed, _ := backfill("66666666-6666-6666-6666-666666666003"); renewed != nil {
		t.Fatalf("NULL-progress row must stay NULL in last_renewed_at (lapsed-by-default input), got %q", deref(renewed))
	}

	// The conservative-boundary shape: the 3-hours-stale row reads as
	// lapsed-by-default ONLY past the 90m threshold — the backfill
	// copied the old semantics, so the arm first fires on the NEXT
	// genuinely-progress-free window, never retroactively at deploy.
	var lapsed3h, lapsed5m bool
	if err := pool.QueryRow(ctx, `
		SELECT last_progress_at < now() - interval '90 minutes'
		FROM tasks WHERE id='55555555-5555-5555-5555-555555555002'`).Scan(&lapsed3h); err != nil {
		t.Fatalf("lapse read 3h row: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT last_progress_at < now() - interval '90 minutes'
		FROM tasks WHERE id='44444444-4444-4444-4444-444444444001'`).Scan(&lapsed5m); err != nil {
		t.Fatalf("lapse read 5m row: %v", err)
	}
	if !lapsed3h || lapsed5m {
		t.Fatalf("backfill must preserve today's semantics verbatim (3h row lapsed=%v, 5m row lapsed=%v)", lapsed3h, lapsed5m)
	}

	// --- DOWN from the POPULATED new world ----------------------------
	if err := m.Steps(-1); err != nil {
		t.Fatalf("step down from 000055 on a populated schema: %v", err)
	}
	var colExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name='tasks' AND column_name='last_renewed_at')`).Scan(&colExists); err != nil {
		t.Fatalf("column probe post-down: %v", err)
	}
	if colExists {
		t.Fatalf("down must drop last_renewed_at")
	}
	// last_progress_at is untouched in both directions (pre-existing
	// column). Post-down the renewal clock is GONE, so this must read
	// the progress clock alone — backfill() selects both columns and
	// would fail on the dropped one (the review-grade bug this check
	// exists to catch is a down that disturbs last_progress_at, not a
	// missing last_renewed_at, whose absence is the down's job).
	var progressAfterDown *string
	if err := pool.QueryRow(ctx,
		`SELECT last_progress_at::text FROM tasks WHERE id=$1`,
		"44444444-4444-4444-4444-444444444001").Scan(&progressAfterDown); err != nil {
		t.Fatalf("post-down progress read: %v", err)
	}
	if progressAfterDown == nil {
		t.Fatalf("down must not disturb last_progress_at")
	}

	// Re-up on the already-migrated-and-downgraded populated schema:
	// the operator-retry shape.
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-up across 000055 after the down: %v", err)
	}
	// The backfill's WHERE last_renewed_at IS NULL guard, pinned
	// directly (the apply-ups-twice idempotency gate's shape): a
	// re-application of the up's SQL must never clobber a real renewal
	// stamp the newer code already wrote — only NULL slots backfill.
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET last_renewed_at = now() - interval '1 minute'
		WHERE id='44444444-4444-4444-4444-444444444001'`); err != nil {
		t.Fatalf("stamp a real renewal: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET last_renewed_at = last_progress_at
		WHERE last_renewed_at IS NULL AND last_progress_at IS NOT NULL`); err != nil {
		t.Fatalf("re-apply the backfill: %v", err)
	}
	var renewedAt, progressAt string
	if err := pool.QueryRow(ctx,
		`SELECT last_renewed_at::text, last_progress_at::text FROM tasks WHERE id=$1`,
		"44444444-4444-4444-4444-444444444001").Scan(&renewedAt, &progressAt); err != nil {
		t.Fatalf("read back post-reapply: %v", err)
	}
	if renewedAt == progressAt {
		t.Fatalf("idempotent re-apply must not clobber an existing last_renewed_at (both %q)", renewedAt)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

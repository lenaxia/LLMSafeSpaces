// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// migration_000057_populated_test.go — the populated-database migration
// pin for the #2047 alert payload: 000057 adds session_alerts.payload
// (JSONB, nullable — the worker_silent arm's task facts) and the
// session-scoped resolution partial index. CI's round-trip gates run
// on EMPTY schemas; this pin holds on a throwaway database parked at
// 000055 with seeded fact-less rows (the pre-#2047 world) and asserts:
// up keeps existing rows NULL-payload (the conservative mid-deploy
// contract — a session_hung row never gains facts), new rows round-trip
// facts byte-for-byte, and the session-scoped resolve UPDATE moves
// ONLY the named session's unresolved row. Skips without
// TEST_DATABASE_URL.
package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
)

func TestMigration000057_PayloadAndSessionResolveUpAndDown(t *testing.T) {
	dsn := scratchDB(t)
	m := migrateAt(t, dsn)

	// Park at 000055 (the pre-#2047 world: fact-less alerts only).
	if err := m.Migrate(56); err != nil {
		t.Fatalf("park at 56: %v", err)
	}
	ctx := context.Background()
	pool, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = pool.Close(ctx) }()

	// Seed the pre-#2047 world: two live alerts in one workspace (two
	// sessions — the sibling shape the session-scoped resolve must NOT
	// touch) and one already-resolved row.
	seed := `
	INSERT INTO session_alerts (workspace_id, session_id, alert, oldest_busy_seconds, created_at, resolved_at)
	VALUES
	  ('mig-2047-ws', 'mig-2047-ses-a', 'session_hung', 960, now() - interval '10 minutes', NULL),
	  ('mig-2047-ws', 'mig-2047-ses-b', 'session_hung', 1200, now() - interval '10 minutes', NULL),
	  ('mig-2047-ws', 'mig-2047-ses-a', 'session_hung', 800, now() - interval '3 hours', now() - interval '2 hours');`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// UP.
	if err := m.Migrate(57); err != nil {
		t.Fatalf("up to 57: %v", err)
	}

	// Existing rows keep NULL payload — the conservative mid-deploy
	// contract (a session_hung row never gains facts).
	var nExisting int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_alerts WHERE payload IS NULL`).Scan(&nExisting); err != nil {
		t.Fatalf("count null payloads: %v", err)
	}
	assert.Equal(t, 3, nExisting, "pre-existing rows carry no facts")

	// New worker_silent rows round-trip facts byte-for-byte.
	if _, err := pool.Exec(ctx,
		`INSERT INTO session_alerts (workspace_id, session_id, alert, oldest_busy_seconds, payload)
		 VALUES ('mig-2047-ws', 'mig-2047-ses-c', 'worker_silent', 2701,
		         '{"lease_state":"lapsed","project_id":"prj-x","task_short_id":"tsk-quiet-one"}'::jsonb)`); err != nil {
		t.Fatalf("insert facts row: %v", err)
	}
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT payload::text FROM session_alerts
		 WHERE session_id='mig-2047-ses-c' AND resolved_at IS NULL`).Scan(&payload); err != nil {
		t.Fatalf("select payload: %v", err)
	}
	assert.JSONEq(t,
		`{"lease_state":"lapsed","project_id":"prj-x","task_short_id":"tsk-quiet-one"}`,
		payload, "facts round-trip byte-for-byte")

	// The session-scoped resolve moves ONLY the named session's
	// unresolved row (#2047's isolation contract).
	tag, err := pool.Exec(ctx,
		`UPDATE session_alerts SET resolved_at = now()
		 WHERE workspace_id=$1 AND session_id=$2 AND resolved_at IS NULL`,
		"mig-2047-ws", "mig-2047-ses-a")
	if err != nil {
		t.Fatalf("session resolve: %v", err)
	}
	assert.Equal(t, int64(1), tag.RowsAffected(),
		"only ses-a's live row resolves — the sibling (ses-b) and the already-resolved row stay put")

	var liveB int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_alerts WHERE workspace_id=$1 AND session_id='mig-2047-ses-b' AND resolved_at IS NULL`,
		"mig-2047-ws").Scan(&liveB); err != nil {
		t.Fatalf("count sibling: %v", err)
	}
	assert.Equal(t, 1, liveB, "the sibling silent worker's alert is untouched")

	// DOWN: the column and index drop; the fact-less shape survives.
	if err := m.Migrate(56); err != nil {
		t.Fatalf("down to 56: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name='session_alerts' AND column_name='payload'`).Scan(&cols); err != nil {
		t.Fatalf("column check: %v", err)
	}
	assert.Zero(t, cols, "down drops the payload column")
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_alerts`).Scan(&rows); err != nil {
		t.Fatalf("rows check: %v", err)
	}
	assert.Equal(t, 4, rows, "the alert history itself survives the down (append-only feed)")
}

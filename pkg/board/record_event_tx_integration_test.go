// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board

// record_event_tx_integration_test.go — the r1-review conflict-path
// pins, in-package so the internal seams are reachable against a real
// Postgres: a replayed deterministic event id produces a ZERO Event
// (nothing to publish), and replayOf discriminates probe errors from
// new keys (a transient failure is an error, never a silent
// reclassification into the state guards).

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
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

// TestRecordEventTx_ConflictYieldsZeroEvent: the same deterministic id
// inserted twice — the second (replay) conflicts, returns a zero Event
// (no row, no view, nothing to publish), and no error.
func TestRecordEventTx_ConflictYieldsZeroEvent(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	const id = "record-event-tx-replay-fixture"
	if _, err := pool.Exec(ctx, `DELETE FROM board_events WHERE id=$1`, id); err != nil {
		t.Fatalf("clean fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM board_events WHERE id=$1`, id)
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	evt, err := recordEventTx(ctx, tx, "00000000-0000-0000-0000-000000000001", id, "reclaim",
		"task", "00000000-0000-0000-0000-000000000002", Actor{Kind: ActorSystem}, map[string]any{"k": 1})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if evt.ID != id || evt.CreatedAt.IsZero() {
		t.Fatalf("first insert returns the committed view: %+v", evt)
	}

	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin replay: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	replay, err := recordEventTx(ctx, tx2, "00000000-0000-0000-0000-000000000001", id, "reclaim",
		"task", "00000000-0000-0000-0000-000000000002", Actor{Kind: ActorSystem}, map[string]any{"k": 1})
	if err != nil {
		t.Fatalf("replayed deterministic id must be inert convergence, got error: %v", err)
	}
	if replay.ID != "" {
		t.Fatalf("a replayed deterministic id yields a ZERO Event (nothing to publish), got %+v", replay)
	}
}

// TestReplayOf_ErrorPath: a probe failure (dead pool) is an ERROR —
// never a silent nil that would reclassify a genuine replay as a new
// key and route it into the state guards.
func TestReplayOf_ErrorPath(t *testing.T) {
	pool := integrationPool(t)
	s := NewService(pool, DefaultEngineConfig())
	pool.Close() // every probe on the closed pool now errors

	dup, err := s.replayOf(context.Background(), "00000000-0000-0000-0000-000000000003", "some-key", "epic_activate")
	if err == nil {
		t.Fatalf("a probe failure must surface as an error, got dup=%v err=nil", dup)
	}
	if dup != nil {
		t.Fatalf("an errored probe classifies as neither replay nor new key, got %v", dup)
	}
	t.Logf("probe error class: %T %v", err, err)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_sse_bus_phrase_integration_test.go — the r1 review's parity
// gap, closed (the function name rides the
// TestIntegrationBoardSSELiveProjection prefix so the CI step's -run
// filter executes it): the bus-join render parity pin ran with a nil pool, so
// the phrase-translation step was a no-op on BOTH legs — exactly the
// hole the dead-ctx regression slipped through (the v1 leg's
// translation silently degraded raw while the bus leg translated
// fine). This pin runs BOTH legs against a REAL pool with a WARM
// session: the v1 delivery and the bus delivery must translate the
// actor's session id to its phrase identically.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

func TestIntegrationBoardSSELiveProjectionBusJoinPhraseParity(t *testing.T) {
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
	ctx := context.Background()

	const wsID = "33333333-3333-3333-3333-333333333304"
	warmRaw := "ses_" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-parity', 'user-parity', 'parity@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'user-parity', $1::text, 'default', now(), now()) ON CONFLICT (id) DO NOTHING`, wsID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, short_id, title, message_count)
		VALUES ($1, $2, 'amber-heron-waits', 'warm', 1)`, wsID, warmRaw)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM session_index WHERE workspace_id=$1`, wsID)
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO projects (id, user_id, target_workspace_id, name, short_id) VALUES
		('11111111-1111-1111-1111-111111111104', 'user-parity', $1::uuid, 'parity', 'prj-phrase01')`, wsID)
	require.NoError(t, err)

	log, _ := logger.NewObserved()
	evt := board.Event{
		ID: "evt-parity-warm", ProjectID: "11111111-1111-1111-1111-111111111104",
		ItemKind: "task", ItemID: "task-1", Action: "move_stage",
		Actor:     []byte(`{"kind":"user","user_id":"user-parity","session_id":"` + warmRaw + `"}`),
		Payload:   []byte(`{"task_id":"task-1","from_stage":"backlog","resulting_stage":"in_progress"}`),
		CreatedAt: time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC),
	}

	// The v1 leg: the drainer's own delivery (lookup + translate +
	// fan-out), captured on its broker.
	v1Broker := eventbroker.NewUserEventBroker()
	v1Sub, err := v1Broker.SubscribeUser("user-parity")
	require.NoError(t, err)
	h := &ProxyHandler{userBroker: v1Broker, logger: log}
	h.boardEventDeliver(pool, log, evt)
	var v1Actor []byte
	select {
	case f := <-v1Sub.Ch:
		v1Actor = f.BoardActor
	case <-time.After(5 * time.Second):
		t.Fatal("the v1 leg delivered nothing")
	}

	// The bus leg: the same row carried as a bus message through the
	// join's render, on its own broker.
	busBroker := eventbroker.NewUserEventBroker()
	busSub, err := busBroker.SubscribeUser("user-parity")
	require.NoError(t, err)
	msg := eventbus.Message{ID: "1-1", Event: eventbus.Event{
		Topic: eventbus.BoardEventTopic(evt.ProjectID), At: evt.CreatedAt,
		Payload: eventbus.BoardEventPayload(evt.ID, evt.ItemKind, evt.ItemID, evt.Action,
			string(evt.Actor), string(evt.Payload), evt.CreatedAt.UTC().Format(time.RFC3339Nano),
			"user-parity", wsID, ""),
	}}
	BoardEventBusHandler(busBroker, pool, log)(ctx, msg)
	select {
	case f := <-busSub.Ch:
		assert.JSONEq(t, string(v1Actor), string(f.BoardActor),
			"THE PHRASE-PARITY PIN: the bus leg translates the warm actor exactly like the v1 leg")
		assert.Contains(t, string(f.BoardActor), "amber-heron-waits", "the warm session's PHRASE, not the raw id")
		assert.NotContains(t, string(f.BoardActor), warmRaw, "the raw harness id is gone (the deprecated twin may remain by contract)")
	case <-time.After(5 * time.Second):
		t.Fatal("the bus leg delivered nothing")
	}
}

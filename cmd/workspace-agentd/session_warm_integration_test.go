// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package main

// session_warm_integration_test.go — #1817 piece 3's agentd replica warm
// against the REAL control-plane contract over a REAL Postgres. The
// registry endpoint here is a contract mirror (cmd cannot import
// api/internal/handlers — the Go internal rule documented in
// board_manager_e2e_integration_test.go), but it speaks the exact wire
// shape the handler serves and reads the same session_index table.
//
// Everything on the agentd side is real: withShortIDStore (the replica),
// setupPodSessionEnv (the pod identity env), emissionIDTranslator, and
// the single sync.Once warm worker with its per-id backoff. Pinned: a
// UI-created session is unknown at first emission (heal window), the
// worker pulls the registry and later emissions carry the phrase; a
// registry 500 leaves the replica cold and never panics.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/migrations"
)

var sessionWarmSeq uint64

func sessionWarmDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
}

func sessionWarmID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&sessionWarmSeq, 1))
}

func newSessionWarmPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := sessionWarmDSN()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("skipping session-warm integration test: pgx pool: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("skipping session-warm integration test: Postgres unreachable at %s — set TEST_DATABASE_URL: %v", dsn, err)
	}
	t.Cleanup(pool.Close)
	applySessionWarmMigrations(t, dsn)
	return pool
}

func applySessionWarmMigrations(t *testing.T, dsn string) {
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

func seedSessionWarmRow(t *testing.T, pool *pgxpool.Pool, workspaceID, harnessID, phrase string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)
		 VALUES ($1, $2, $3, 0, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET short_id = EXCLUDED.short_id`,
		workspaceID, harnessID, phrase)
	require.NoError(t, err)
}

func newSessionWarmRegistry(t *testing.T, pool *pgxpool.Pool, pulls *int32, fail bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/session-registry" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(pulls, 1)
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rows, err := pool.Query(r.Context(),
			`SELECT session_id, short_id FROM session_index
			 WHERE workspace_id = $1 AND short_id IS NOT NULL`, r.URL.Query().Get("workspaceID"))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		sessions := make([]map[string]string, 0)
		for rows.Next() {
			var sessionID, shortID string
			if err := rows.Scan(&sessionID, &shortID); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			sessions = append(sessions, map[string]string{"sessionID": sessionID, "shortID": shortID})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": sessions})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resetSessionWarmGate(t *testing.T) {
	t.Helper()
	shortIDPullMu.Lock()
	shortIDLastPull = time.Time{}
	shortIDPullMu.Unlock()
}

func TestIntegrationEmissionWarm_WarmsReplicaFromRegistry(t *testing.T) {
	pool := newSessionWarmPool(t)
	workspace := "ws-" + sessionWarmID()
	harnessID := "ses_" + sessionWarmID()
	phrase := "amber-falcon-glides"
	seedSessionWarmRow(t, pool, workspace, harnessID, phrase)

	var pulls int32
	srv := newSessionWarmRegistry(t, pool, &pulls, false)
	setupPodSessionEnv(t, srv)
	t.Setenv("WORKSPACE_ID", workspace)

	store := withShortIDStore(t)
	resetSessionWarmGate(t)

	require.Equal(t, "", emissionIDTranslator(harnessID),
		"an unknown session must stay in the heal window on first emission")
	require.Eventually(t, func() bool { return store.shortIDFor(harnessID) == phrase },
		5*time.Second, 20*time.Millisecond, "the warm worker never learned the registry row")
	require.Equal(t, phrase, emissionIDTranslator(harnessID),
		"a warmed replica must translate subsequent emissions to the phrase")
}

func TestIntegrationEmissionWarm_UnhealthyRegistryStaysCold(t *testing.T) {
	pool := newSessionWarmPool(t)
	workspace := "ws-" + sessionWarmID()
	harnessID := "ses_" + sessionWarmID()

	var pulls int32
	srv := newSessionWarmRegistry(t, pool, &pulls, true)
	setupPodSessionEnv(t, srv)
	t.Setenv("WORKSPACE_ID", workspace)

	store := withShortIDStore(t)
	resetSessionWarmGate(t)

	require.Equal(t, "", emissionIDTranslator(harnessID))
	require.Eventually(t, func() bool { return atomic.LoadInt32(&pulls) >= 1 },
		5*time.Second, 20*time.Millisecond, "the warm worker never attempted a pull")
	require.Equal(t, "", store.shortIDFor(harnessID),
		"an unhealthy registry must leave the replica cold")
	require.Equal(t, "", emissionIDTranslator(harnessID),
		"emissions must keep the heal window while the registry is down")
}

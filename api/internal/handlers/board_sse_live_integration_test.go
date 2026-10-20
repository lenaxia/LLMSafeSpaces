// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_sse_live_integration_test.go — the §4.6 LIVE-path e2e pin (r2
// review's remaining minor): a real board write, through the real
// publisher wiring (project lookup + broker fan-out), surfacing as a
// flat board.event frame on the REAL /api/v1/events SSE endpoint. No
// fakes on the delivery path — only the auth middleware is stood in
// (the JWT shape, not a stub of the handler).

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestIntegrationBoardSSELiveProjection(t *testing.T) {
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
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-sse', 'user-sse', 'boardsse@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	const wsID = "33333333-3333-3333-3333-333333333303"
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'user-sse', $1::text, 'default', now(), now()) ON CONFLICT (id) DO NOTHING`, wsID)
	require.NoError(t, err)

	// The production wiring shape: broker-backed ProxyHandler + the
	// board service with BoardEventPublisher installed. The observed
	// logger stands in for the production logger (the stream handler
	// logs its lifecycle; the snapshot goroutine skips on nil k8s).
	log, _ := logger.NewObserved()
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker(), logger: log}
	cfg := board.DefaultEngineConfig()
	svc := board.NewService(pool, cfg)
	pub := h.BoardEventPublisher(pool, log)
	require.NotNil(t, pub, "broker-mounted handler installs the publisher")
	svc.SetEventPublisher(pub)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-sse"); c.Next() })
	r.GET("/api/v1/events", h.StreamUserEvents)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	// The SSE connection must be client-canceled BEFORE srv.Close (LIFO:
	// registered after srv's cleanup) — a live stream otherwise blocks
	// Close's wait on outstanding requests (the 2nd CI run's 5m hang).
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)

	// Subscribe first (the frame must arrive on the live stream, not via
	// replay), then write through the real service — the subscription is
	// ready once the response headers land (the handler subscribes
	// before writing them).
	frames := make(chan string, 64)
	ready := make(chan struct{})
	var seenMu sync.Mutex
	var seen []string
	go func() {
		req, _ := http.NewRequestWithContext(subCtx, http.MethodGet, srv.URL+"/api/v1/events", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			close(ready)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		first := true
		for sc.Scan() {
			if first {
				first = false
				close(ready)
			}
			line := sc.Text()
			seenMu.Lock()
			seen = append(seen, line)
			seenMu.Unlock()
			if strings.HasPrefix(line, "data: ") {
				frames <- strings.TrimPrefix(line, "data: ")
			}
		}
		if first {
			close(ready)
		}
	}()
	dumpSeen := func() {
		seenMu.Lock()
		defer seenMu.Unlock()
		t.Logf("stream lines seen (%d):", len(seen))
		for _, l := range seen {
			t.Logf("  %s", l)
		}
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("SSE subscription did not establish within 5s")
	}

	owner := board.Actor{Kind: board.ActorUser, UserID: "user-sse", Surface: board.SurfaceREST}
	p, err := svc.CreateProject(ctx, owner,
		board.ProjectInput{Name: "sse-live", TargetWorkspaceID: wsID, WorkerSlots: 2, Tags: []string{}}, "")
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "E"}, "")
	require.NoError(t, err)
	_, err = svc.CreateTask(ctx, owner, e.ID, "", board.TaskInput{Title: "live feed"}, "")
	require.NoError(t, err)

	var frame string
	deadline := time.After(30 * time.Second)
	for {
		select {
		case frame = <-frames:
			if strings.Contains(frame, `"board.event"`) && strings.Contains(frame, `"create_task"`) {
				goto found
			}
		case <-deadline:
			dumpSeen()
			t.Fatalf("no board.event create_task frame on the live user stream within 30s")
		}
	}
found:
	assert.Contains(t, frame, `"project_id":"`+p.ID, "flat project_id sibling of type")
	assert.Contains(t, frame, `"item_kind":"task"`)
	assert.Contains(t, frame, `"action":"create_task"`)
	assert.Contains(t, frame, `"actor":{"kind":"user","user_id":"user-sse","surface":"rest"}`)
	assert.Contains(t, frame, `"created_at"`)
	assert.NotContains(t, frame, `"data":`, "board.event rides flat fields, never the data envelope")
}

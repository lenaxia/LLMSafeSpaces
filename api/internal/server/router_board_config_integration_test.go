// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package server

// router_board_config_integration_test.go — #1832 item 6: the config
// REST surface through the PRODUCTION router — the real NewRouter
// registration (route table + middleware wiring as shipped), a REAL
// board.Service over Postgres, and an auth middleware stand-in that
// models the AuthMiddleware contract faithfully for this surface:
// no Authorization header → 401 before any handler; a Bearer identity
// → the userID context key. (The JWT machinery itself is the auth
// service's own test surface; router tests mock the seam, the
// router_admin_* precedent.) Pins: 401 on all four unauthenticated
// routes, the authenticated user round-trip + project
// inherit→override→clear, the user-level key ruling, and the scoped
// 404 — all through the production route table.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func newBoardConfigProductionRouter(t *testing.T, pool *pgxpool.Pool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	svc := board.NewService(pool, board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: time.Hour,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	})

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		// The contract the real middleware provides on this surface:
		// unauthenticated requests never reach a handler (401); an
		// authenticated identity arrives as the userID context key.
		hdr := c.GetHeader("Authorization")
		if len(hdr) <= len("Bearer ") || hdr[:len("Bearer ")] != "Bearer " {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("userID", hdr[len("Bearer "):])
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Maybe().Return("router-cfg")

	s := &mockServices{auth: auth, metrics: met}
	cfg := RouterConfig{BoardHandler: handlers.NewBoardHandler(svc)}
	return NewRouter(s, log, nil, cfg)
}

func TestIntegrationRouterBoardConfig(t *testing.T) {
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

	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects,
			board_config_defaults, board_project_config_overrides
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('router-cfg', 'router-cfg', 'routercfg@example.com', 'hash', true, 'user'),
		       ('router-cfg-other', 'router-cfg-other', 'routercfgother@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	var wsID string
	err = pool.QueryRow(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES (gen_random_uuid(), 'router-cfg', 'ws-router-cfg', 'default', now(), now())
		RETURNING id
	`).Scan(&wsID)
	require.NoError(t, err)

	r := newBoardConfigProductionRouter(t, pool)

	// The project under test, created through the real service.
	bsvc := board.NewService(pool, board.EngineConfig{AttemptsCap: 3, LeaseDuration: 15 * time.Minute, ThrottleBackoff: time.Hour, DRRQuantum: 1, SweepInterval: time.Second})
	owned, err := bsvc.CreateProject(ctx, board.Actor{Kind: board.ActorUser, UserID: "router-cfg", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "router-cfg-proj", TargetWorkspaceID: wsID, Tags: []string{}}, "")
	require.NoError(t, err)

	call := func(bearer, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// --- unauthenticated: 401 on every config route (production wiring) -----------
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me/board/config"},
		{http.MethodPut, "/api/v1/me/board/config"},
		{http.MethodGet, "/api/v1/me/projects/" + owned.ID + "/config"},
		{http.MethodPut, "/api/v1/me/projects/" + owned.ID + "/config"},
	} {
		w := call("", tc.method, tc.path, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s must 401 before any handler", tc.method, tc.path)
	}

	// --- authenticated round-trips through the production route table -------------
	w := call("router-cfg", http.MethodGet, "/api/v1/me/board/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":2}`, w.Body.String())

	w = call("router-cfg", http.MethodPut, "/api/v1/me/board/config", `{"reviewColumn":"off","defaultWorkerSlots":6}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = call("router-cfg", http.MethodGet, "/api/v1/me/projects/"+owned.ID+"/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"reviewColumn":"off"`, "the project inherits the user defaults")

	w = call("router-cfg", http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{"reviewColumn":"auto"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"reviewColumn":"auto"`, "the override wins")

	w = call("router-cfg", http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"reviewColumn":"off"`, "clearing re-inherits")

	// --- the user-level key ruling + scoped 404 through production -----------------
	w = call("router-cfg", http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{"defaultWorkerSlots":5}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "user-level key")

	w = call("router-cfg-other", http.MethodGet, "/api/v1/me/projects/"+owned.ID+"/config", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "foreign project: scoped 404 through the production perimeter")

	// --- malformed bodies through the production route table (round-3) ------------
	// These 400s were previously pinned only through the hand-built
	// handler router; the production route table must render the same
	// strict gate (middleware wiring cannot soften it).
	for _, tc := range []struct{ name, body string }{
		{"unknown key", `{"reviewQueue":"off"}`},
		{"per-key null", `{"autoAssignReviewer":null}`},
		{"top-level null", `null`},
		{"empty body", ``},
	} {
		w := call("router-cfg", http.MethodPut, "/api/v1/me/board/config", tc.body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s must 400 through the production route table: %s", tc.name, w.Body.String())
	}

	// Storage drift renders 5xx through the production route table
	// (round-3): a drifted stored row is the server's problem — a GET
	// must not render 400 invalid_input on any wiring.
	_, err = pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ('router-cfg', 'autoAssignReviewer', '"not-a-bool"')`)
	require.NoError(t, err)
	w = call("router-cfg", http.MethodGet, "/api/v1/me/board/config", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "drift GET must render 5xx through production, never 400: %s", w.Body.String())
	_, err = pool.Exec(ctx,
		`DELETE FROM board_config_defaults WHERE user_id='router-cfg' AND key='autoAssignReviewer'`)
	require.NoError(t, err)
}

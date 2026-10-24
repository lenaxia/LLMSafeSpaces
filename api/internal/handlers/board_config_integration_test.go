// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_config_integration_test.go — #1832 item 6: the config REST
// surface over the REAL wiring — BoardHandler → board.Service →
// Postgres (the migrated schema, migrations applied by the CI job).
// The pkg/board suite pins the service→store seam; THIS suite pins the
// router-tier adapter over it: the user-scoped defaults round-trip,
// the project override→inherit loop through real handlers, the strict
// body gate (unknown key / malformed value / null / empty body), and
// the scoped-404 project perimeter. The auth perimeter itself (real
// AuthMiddleware) is pinned by the route-table scan + the router's
// middleware wiring, not re-pinned here (no admin tier exists anymore
// — the config surface is user-scoped per the R7 ruling on PR #1840).

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestIntegrationBoardConfig(t *testing.T) {
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
		VALUES ('user-cfg', 'user-cfg', 'boardcfg@example.com', 'hash', true, 'user'),
		       ('user-cfg-other', 'user-cfg-other', 'boardcfgother@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "users rows must exist (FK target)")
	var wsCfg string
	err = pool.QueryRow(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES (gen_random_uuid(), 'user-cfg', 'ws-cfg', 'default', now(), now())
		RETURNING id
	`).Scan(&wsCfg)
	require.NoError(t, err)

	cfg := board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: time.Hour,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	}
	svc := board.NewService(pool, cfg)

	// The project both users read/write (owned by user-cfg).
	owned, err := svc.CreateProject(ctx, board.Actor{Kind: board.ActorUser, UserID: "user-cfg", Surface: board.SurfaceREST},
		board.ProjectInput{Name: "cfg-proj", TargetWorkspaceID: wsCfg, Tags: []string{}}, "")
	require.NoError(t, err)

	// The handler tier over the real service, mounted exactly as the
	// router mounts the config routes (the userID middleware is the
	// stand-in for AuthMiddleware's context-key contract).
	mkRouter := func(userID string) *gin.Engine {
		gin.SetMode(gin.TestMode)
		h := NewBoardHandler(svc)
		r := gin.New()
		g := r.Group("/api/v1/me")
		g.Use(func(c *gin.Context) {
			c.Set("userID", userID)
			c.Next()
		})
		p := g.Group("/projects")
		p.GET("/:id/config", h.GetProjectBoardConfig)
		p.PUT("/:id/config", h.SetProjectBoardConfig)
		bb := g.Group("/board")
		bb.GET("/config", h.GetUserBoardConfig)
		bb.PUT("/config", h.SetUserBoardConfig)
		return r
	}
	owner := mkRouter("user-cfg")

	// --- 1: user defaults round-trip through the real handler+service+PG ----------
	w := doBoardREST(t, owner, http.MethodGet, "/api/v1/me/board/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":2}`, w.Body.String())

	w = doBoardREST(t, owner, http.MethodPut, "/api/v1/me/board/config", `{"reviewColumn":"off","defaultWorkerSlots":6}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"off","autoAssignReviewer":true,"defaultWorkerSlots":6}`, w.Body.String())

	// --- 2: project inherits → overrides → clears, all through the real seam -------
	w = doBoardREST(t, owner, http.MethodGet, "/api/v1/me/projects/"+owned.ID+"/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{
		"project_id":"`+owned.ID+`",
		"overrides":{},
		"effective":{"reviewColumn":"off","autoAssignReviewer":true,"defaultWorkerSlots":6}
	}`, w.Body.String(), "no overrides yet: the project inherits the user's defaults")

	w = doBoardREST(t, owner, http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{"reviewColumn":"auto"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{
		"project_id":"`+owned.ID+`",
		"overrides":{"reviewColumn":"auto"},
		"effective":{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":6}
	}`, w.Body.String(), "the set key wins; unset keys keep inheriting")

	// A LATER user-default flip re-flips the project's inherited value
	// (read-time merge, never materialized).
	w = doBoardREST(t, owner, http.MethodPut, "/api/v1/me/board/config", `{"reviewColumn":"off","defaultWorkerSlots":9}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = doBoardREST(t, owner, http.MethodGet, "/api/v1/me/projects/"+owned.ID+"/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"defaultWorkerSlots":9`, "unset key tracks later user-default writes")
	assert.Contains(t, w.Body.String(), `"reviewColumn":"auto"`, "the set override is untouched")

	// Clearing the overrides re-inherits fully.
	w = doBoardREST(t, owner, http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"reviewColumn":"off"`, "cleared override re-inherits the user default")

	// --- 3: the strict body gate through the real adapter -------------------------
	for name, body := range map[string]string{
		"unknown key":    `{"reviewQueue":"off"}`,
		"malformed enum": `{"reviewColumn":"maybe"}`,
		"null value":     `{"autoAssignReviewer":null}`,
		"top-level null": `null`,
		"empty body":     ``,
		"malformed json": `{reviewColumn`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			w := doBoardREST(t, owner, http.MethodPut, "/api/v1/me/board/config", body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}

	// --- 4: the scoped-404 project perimeter through the real service -------------
	foreign := mkRouter("user-cfg-other")
	w = doBoardREST(t, foreign, http.MethodGet, "/api/v1/me/projects/"+owned.ID+"/config", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "a foreign project renders the scoped 404, never disclosed")
	w = doBoardREST(t, foreign, http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{"reviewColumn":"auto"}`)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	// --- 4b: the user-level key ruling renders 400 through the real seam ----------
	w = doBoardREST(t, owner, http.MethodPut, "/api/v1/me/projects/"+owned.ID+"/config", `{"defaultWorkerSlots":5}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "user-level key", "the rejection names the ruling")

	// --- 4c: storage drift renders a SERVER error (5xx), never 400 ---------------
	_, err = pool.Exec(context.Background(),
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ('user-cfg', 'autoAssignReviewer', '"not-a-bool"')`)
	require.NoError(t, err)
	w = doBoardREST(t, owner, http.MethodGet, "/api/v1/me/board/config", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"a drifted stored row is the server's problem — a GET must not render 400 invalid_input: %s", w.Body.String())
	_, err = pool.Exec(context.Background(),
		`DELETE FROM board_config_defaults WHERE user_id='user-cfg' AND key='autoAssignReviewer'`)
	require.NoError(t, err)

	// --- 5: user isolation through the real seam -----------------------------------
	w = doBoardREST(t, foreign, http.MethodGet, "/api/v1/me/board/config", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":2}`,
		w.Body.String(), "another user's defaults map neither leaks nor collides")
}

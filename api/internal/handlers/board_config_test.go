// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_config_test.go — RED-first pins for the #1832 item 6 Task Board
// Config REST surface: the admin-scoped global defaults read/write, the
// owner-scoped per-project override read/write (both rendering the
// effective merged config), strict unknown-key/malformed-value rejection
// at the adapter, and the ownership perimeter on project config reads.
// Landed BEFORE handlers/board_config.go (house red-first rule); the red
// state at this commit is the compile error (undefined handler methods).

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// --- fake seam arms (config) ---------------------------------------------------

func (f *fakeBoardManager) GetGlobalBoardConfig(_ context.Context) (*board.BoardConfig, error) {
	f.call("get_global_board_config")
	if err := f.errFor["get_global_board_config"]; err != nil {
		return nil, err
	}
	cfg := board.BoardConfig{ReviewColumn: "manual", AutoAssignReviewer: false, DefaultWorkerSlots: 2}
	return &cfg, nil
}

func (f *fakeBoardManager) SetGlobalBoardConfig(_ context.Context, in *board.BoardConfigOverride) (*board.BoardConfig, error) {
	f.call("set_global_board_config")
	f.lastGlobalConfigSet = in
	if err := f.errFor["set_global_board_config"]; err != nil {
		return nil, err
	}
	cfg := board.BoardConfig{ReviewColumn: "auto", AutoAssignReviewer: false, DefaultWorkerSlots: 2}
	return &cfg, nil
}

func (f *fakeBoardManager) GetProjectBoardConfig(_ context.Context, _, idOrShort string) (*board.ProjectBoardConfig, error) {
	f.call("get_project_board_config")
	_ = idOrShort
	if err := f.errFor["get_project_board_config"]; err != nil {
		return nil, err
	}
	return &board.ProjectBoardConfig{
		ProjectID: "p1",
		Overrides: board.BoardConfigOverride{ReviewColumn: strPtr("off")},
		Effective: board.BoardConfig{ReviewColumn: "off", AutoAssignReviewer: false, DefaultWorkerSlots: 2},
	}, nil
}

func (f *fakeBoardManager) SetProjectBoardConfig(_ context.Context, actor board.Actor, _, idOrShort string, in *board.BoardConfigOverride) (*board.ProjectBoardConfig, error) {
	f.call("set_project_board_config")
	f.lastActor = actor
	f.lastProjectConfigSet = in
	if err := f.errFor["set_project_board_config"]; err != nil {
		return nil, err
	}
	echo := board.BoardConfigOverride{}
	if in != nil {
		echo = *in
	}
	return &board.ProjectBoardConfig{
		ProjectID: "p1",
		Overrides: echo,
		Effective: board.BoardConfig{ReviewColumn: "auto", AutoAssignReviewer: false, DefaultWorkerSlots: 5},
	}, nil
}

// newBoardConfigRouter mounts exactly the config routes the router
// mounts (admin group + project group), with the userID-stamping
// AuthMiddleware stand-in the real middleware provides.
func newBoardConfigRouter(t *testing.T, svc *fakeBoardManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewBoardHandler(svc)
	r := gin.New()
	g := r.Group("/api/v1/me")
	g.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	p := g.Group("/projects")
	p.GET("/:id/config", h.GetProjectBoardConfig)
	p.PUT("/:id/config", h.SetProjectBoardConfig)
	ag := r.Group("/api/v1/admin/board")
	ag.Use(func(c *gin.Context) {
		c.Set("userID", "admin-1")
		c.Next()
	})
	ag.GET("/config", h.GetGlobalBoardConfig)
	ag.PUT("/config", h.SetGlobalBoardConfig)
	return r
}

// TestBoardConfigREST_GlobalRoundTrip: GET renders the effective global
// defaults; PUT decodes a partial map, hands the typed override to the
// service, and renders the new effective row.
func TestBoardConfigREST_GlobalRoundTrip(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardConfigRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/admin/board/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"reviewColumn":"manual","autoAssignReviewer":false,"defaultWorkerSlots":2}`, w.Body.String())

	w = doBoardREST(t, r, http.MethodPut, "/api/v1/admin/board/config", `{"reviewColumn":"auto"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":false,"defaultWorkerSlots":2}`, w.Body.String())
	require.NotNil(t, svc.lastGlobalConfigSet)
	require.NotNil(t, svc.lastGlobalConfigSet.ReviewColumn)
	assert.Equal(t, "auto", *svc.lastGlobalConfigSet.ReviewColumn)
	assert.Nil(t, svc.lastGlobalConfigSet.AutoAssignReviewer, "absent key stays unset (inherit)")
	assert.Nil(t, svc.lastGlobalConfigSet.DefaultWorkerSlots)

	// The empty map is a legal clear-all (every key back to default).
	w = doBoardREST(t, r, http.MethodPut, "/api/v1/admin/board/config", `{}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, svc.lastGlobalConfigSet)
	assert.Nil(t, svc.lastGlobalConfigSet.ReviewColumn, "empty map clears every override (all inherit)")
	assert.Nil(t, svc.lastGlobalConfigSet.AutoAssignReviewer)
	assert.Nil(t, svc.lastGlobalConfigSet.DefaultWorkerSlots)
}

// TestBoardConfigREST_GlobalRejectsUnknownAndMalformed: unknown keys and
// malformed values never reach the service (adapter rejects; calls stay 0).
func TestBoardConfigREST_GlobalRejectsUnknownAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown key", `{"reviewQueue":"off"}`},
		{"unknown key alongside known", `{"reviewColumn":"auto","bogus":1}`},
		{"bad enum", `{"reviewColumn":"maybe"}`},
		{"wrong type bool", `{"autoAssignReviewer":"true"}`},
		{"zero slots", `{"defaultWorkerSlots":0}`},
		{"negative slots", `{"defaultWorkerSlots":-1}`},
		{"fractional slots", `{"defaultWorkerSlots":2.5}`},
		{"string slots", `{"defaultWorkerSlots":"3"}`},
		{"malformed json", `{reviewColumn`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			r := newBoardConfigRouter(t, svc)
			w := doBoardREST(t, r, http.MethodPut, "/api/v1/admin/board/config", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, 0, svc.calls["set_global_board_config"], "rejected writes never reach the service")
		})
	}
}

// TestBoardConfigREST_ProjectRoundTrip: GET renders overrides + effective;
// PUT stamps the REST actor, threads the resolved project, and renders the
// fresh merge.
func TestBoardConfigREST_ProjectRoundTrip(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", WorkerSlots: 2, Status: board.ProjectActive}}
	r := newBoardConfigRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/p1/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{
		"project_id":"p1",
		"overrides":{"reviewColumn":"off"},
		"effective":{"reviewColumn":"off","autoAssignReviewer":false,"defaultWorkerSlots":2}
	}`, w.Body.String())

	w = doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"defaultWorkerSlots":5,"autoAssignReviewer":true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, svc.lastProjectConfigSet)
	require.NotNil(t, svc.lastProjectConfigSet.DefaultWorkerSlots)
	require.NotNil(t, svc.lastProjectConfigSet.AutoAssignReviewer)
	assert.Equal(t, 5, *svc.lastProjectConfigSet.DefaultWorkerSlots)
	assert.True(t, *svc.lastProjectConfigSet.AutoAssignReviewer)
	assert.Nil(t, svc.lastProjectConfigSet.ReviewColumn)
	// REST actor stamping: the JWT user, user kind, rest surface, no token.
	assert.Equal(t, board.Actor{Kind: board.ActorUser, UserID: "user-1", Surface: board.SurfaceREST}, svc.lastActor)
}

// TestBoardConfigREST_ProjectPerimeter: a foreign project renders the same
// plain 404 a miss renders (R7 read perimeter — never disclosed).
func TestBoardConfigREST_ProjectPerimeter(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "someone-else", WorkerSlots: 2, Status: board.ProjectActive}}
	r := newBoardConfigRouter(t, svc)

	for _, tc := range []struct{ name, method string }{
		{"get foreign project config", http.MethodGet},
		{"put foreign project config", http.MethodPut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doBoardREST(t, r, tc.method, "/api/v1/me/projects/p1/config", `{"reviewColumn":"auto"}`)
			assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			assert.Equal(t, 0, svc.calls["set_project_board_config"], "foreign writes never reach the service")
		})
	}
}

// TestBoardConfigREST_ProjectRejectsUnknownAndMalformed: the same strict
// key-gate + per-key validation on the project surface, plus the service's
// GuardInput rendering (400, code invalid_input).
func TestBoardConfigREST_ProjectRejectsUnknownAndMalformed(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", WorkerSlots: 2, Status: board.ProjectActive}}
	r := newBoardConfigRouter(t, svc)

	t.Run("adapter rejects unknown key before service", func(t *testing.T) {
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"reviewColumn":"auto","bogus":1}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, 0, svc.calls["set_project_board_config"])
	})

	t.Run("adapter rejects malformed value", func(t *testing.T) {
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"reviewColumn":"maybe"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, 0, svc.calls["set_project_board_config"])
	})

	t.Run("service GuardInput renders 400 invalid_input", func(t *testing.T) {
		svc.errFor["set_project_board_config"] = &board.GuardError{Code: board.GuardInput, Detail: "defaultWorkerSlots must be > 0"}
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"defaultWorkerSlots":5}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_input")
	})

	t.Run("missing project renders 404", func(t *testing.T) {
		svc2 := newFakeBoardManager() // no projects
		r2 := newBoardConfigRouter(t, svc2)
		w := doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/nope/config", "")
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	})
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_config_test.go — RED-first pins for the #1832 item 6 Task Board
// Config REST surface: the user-scoped board-wide defaults read/write
// (R7 ruling on PR #1840), the owner-scoped per-project override
// read/write (both rendering the effective merged config), strict
// unknown-key/malformed-value/NULL rejection at the adapter, and the
// ownership perimeter on project config reads. Landed BEFORE
// handlers/board_config.go (house red-first rule); the red state at
// this commit is the compile error (undefined handler methods).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// --- fake seam arms (config) ---------------------------------------------------

func (f *fakeBoardManager) GetUserBoardConfig(_ context.Context, actor board.Actor) (*board.BoardConfig, error) {
	f.call("get_user_board_config")
	_ = actor
	if err := f.errFor["get_user_board_config"]; err != nil {
		return nil, err
	}
	cfg := board.DefaultBoardConfig()
	return &cfg, nil
}

func (f *fakeBoardManager) SetUserBoardConfig(_ context.Context, actor board.Actor, in *board.BoardConfigOverride) (*board.BoardConfig, error) {
	f.call("set_user_board_config")
	f.lastActor = actor
	f.lastGlobalConfigSet = in
	if err := f.errFor["set_user_board_config"]; err != nil {
		return nil, err
	}
	cfg := board.DefaultBoardConfig()
	cfg.ReviewColumn = "auto"
	return &cfg, nil
}

func (f *fakeBoardManager) GetProjectBoardConfig(_ context.Context, actor board.Actor, _, idOrShort string) (*board.ProjectBoardConfig, error) {
	f.call("get_project_board_config")
	// The real service resolves the project scoped to the acting user
	// (R7, design 0067:37 guards-once): a foreign or unknown project
	// renders the scoped 404 — the fake models that perimeter.
	var owned *board.Project
	for i := range f.projects {
		if f.projects[i].ID == idOrShort || f.projects[i].ShortID == idOrShort {
			if f.projects[i].UserID == actor.UserID {
				owned = &f.projects[i]
			}
			break
		}
	}
	if owned == nil {
		return nil, &board.NotFoundError{Item: "project", Identifier: idOrShort}
	}
	if err := f.errFor["get_project_board_config"]; err != nil {
		return nil, err
	}
	eff := board.DefaultBoardConfig()
	eff.ReviewColumn = "off"
	return &board.ProjectBoardConfig{
		ProjectID: owned.ID,
		Overrides: board.BoardConfigOverride{ReviewColumn: strPtr("off")},
		Effective: eff,
	}, nil
}

func (f *fakeBoardManager) SetProjectBoardConfig(_ context.Context, actor board.Actor, _, idOrShort string, in *board.BoardConfigOverride) (*board.ProjectBoardConfig, error) {
	f.call("set_project_board_config")
	f.lastActor = actor
	f.lastProjectConfigSet = in
	if err := f.errFor["set_project_board_config"]; err != nil {
		return nil, err
	}
	// Model the real service's user-level-key ruling (round-3): the
	// project surface rejects defaultWorkerSlots — the fake must not
	// mask a rejection the real seam renders as 400 invalid_input.
	if in != nil && in.DefaultWorkerSlots != nil {
		return nil, &board.GuardError{Code: board.GuardInput, Detail: "defaultWorkerSlots is a user-level key — set it in the board-wide defaults (PUT /api/v1/me/board/config); a project's worker slots are fixed at create"}
	}
	echo := board.BoardConfigOverride{}
	if in != nil {
		echo = *in
	}
	eff := board.DefaultBoardConfig()
	eff.ReviewColumn, eff.AutoAssignReviewer = "auto", true
	return &board.ProjectBoardConfig{
		ProjectID: "p1",
		Overrides: echo,
		Effective: eff,
	}, nil
}

// newBoardConfigRouter mounts exactly the config routes the router
// mounts (the /me/board user-scoped group + the project group), with
// the userID-stamping AuthMiddleware stand-in the real middleware
// provides.
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
	bb := g.Group("/board")
	bb.GET("/config", h.GetUserBoardConfig)
	bb.PUT("/config", h.SetUserBoardConfig)
	return r
}

// TestBoardConfigREST_UserRoundTrip: GET renders the acting user's
// effective board-wide defaults; PUT decodes a partial map, hands the
// typed override + actor to the service, and renders the new
// effective row.
func TestBoardConfigREST_UserRoundTrip(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardConfigRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/config", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":2,"stagePersonas":{}}`, w.Body.String())

	w = doBoardREST(t, r, http.MethodPut, "/api/v1/me/board/config", `{"reviewColumn":"auto"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"reviewColumn":"auto","autoAssignReviewer":true,"defaultWorkerSlots":2,"stagePersonas":{}}`, w.Body.String())
	require.NotNil(t, svc.lastGlobalConfigSet)
	require.NotNil(t, svc.lastGlobalConfigSet.ReviewColumn)
	assert.Equal(t, "auto", *svc.lastGlobalConfigSet.ReviewColumn)
	assert.Nil(t, svc.lastGlobalConfigSet.AutoAssignReviewer, "absent key stays unset (inherit)")
	assert.Nil(t, svc.lastGlobalConfigSet.DefaultWorkerSlots)
	// REST actor stamping: the JWT user, user kind, rest surface, no token.
	assert.Equal(t, board.Actor{Kind: board.ActorUser, UserID: "user-1", Surface: board.SurfaceREST}, svc.lastActor)

	// The empty map is a legal clear-all (every key back to default).
	w = doBoardREST(t, r, http.MethodPut, "/api/v1/me/board/config", `{}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, svc.lastGlobalConfigSet)
	assert.Nil(t, svc.lastGlobalConfigSet.ReviewColumn, "empty map clears every override (all inherit)")
	assert.Nil(t, svc.lastGlobalConfigSet.AutoAssignReviewer)
	assert.Nil(t, svc.lastGlobalConfigSet.DefaultWorkerSlots)
}

// TestBoardConfigREST_StagePersonasRoundTrip: the stagePersonas key
// decodes through the adapter's key-gate on BOTH tiers (the typed map
// reaches the service; an explicitly empty object is the opt-out
// override), and the ineligible-stage / bad-shape rejections render
// 400 before the service.
func TestBoardConfigREST_StagePersonasRoundTrip(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", WorkerSlots: 2, Status: board.ProjectActive}}
	r := newBoardConfigRouter(t, svc)

	t.Run("user tier decodes the typed map", func(t *testing.T) {
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/board/config",
			`{"stagePersonas":{"in_review":{"name":"auditor","instructions":"Verify."},"in_progress":{"name":"implementer"}}}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, svc.lastGlobalConfigSet.StagePersonas)
		m := *svc.lastGlobalConfigSet.StagePersonas
		require.Len(t, m, 2)
		assert.Equal(t, board.PersonaConfig{Name: "auditor", Instructions: "Verify."}, m["in_review"])
		assert.Equal(t, board.PersonaConfig{Name: "implementer"}, m["in_progress"])
	})

	t.Run("project tier accepts the key (the composition lever)", func(t *testing.T) {
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config",
			`{"stagePersonas":{}}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, svc.lastProjectConfigSet.StagePersonas)
		assert.Empty(t, *svc.lastProjectConfigSet.StagePersonas, "{} is the explicit opt-out override")
	})

	for _, tc := range []struct{ name, body string }{
		{"ineligible stage rejects", `{"stagePersonas":{"backlog":{"name":"p"}}}`},
		{"empty persona name rejects", `{"stagePersonas":{"in_review":{"name":""}}}`},
		{"null stage value rejects", `{"stagePersonas":{"in_review":null}}`},
		{"unknown persona field rejects", `{"stagePersonas":{"in_review":{"name":"r","promt":"x"}}}`},
		{"non-object rejects", `{"stagePersonas":["in_review"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh fake per arm: the accepted arms above already
			// reached the service; a rejection must land 0 calls on a
			// clean counter.
			svc2 := newFakeBoardManager()
			r2 := newBoardConfigRouter(t, svc2)
			w := doBoardREST(t, r2, http.MethodPut, "/api/v1/me/board/config", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, 0, svc2.calls["set_user_board_config"], "rejections never reach the service")
		})
	}
}

// TestBoardConfigREST_UserRejectsUnknownMalformedNull: unknown keys,
// malformed values, JSON nulls, a null body, and an empty body never
// reach the service (adapter rejects; calls stay 0). Null is never a
// value: an unset key is absent, never null — a null must not
// masquerade as an explicit false/zero and flip inheritance.
func TestBoardConfigREST_UserRejectsUnknownMalformedNull(t *testing.T) {
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
		{"null bool value", `{"autoAssignReviewer":null}`},
		{"null string value", `{"reviewColumn":null}`},
		{"null int value", `{"defaultWorkerSlots":null}`},
		{"null alongside known", `{"reviewColumn":"auto","autoAssignReviewer":null}`},
		{"top-level null body", `null`},
		{"empty body", ``},
		{"whitespace-only body", `   `},
		{"array body", `["reviewColumn"]`},
		{"malformed json", `{reviewColumn`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			r := newBoardConfigRouter(t, svc)
			w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/board/config", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, 0, svc.calls["set_user_board_config"], "rejected writes never reach the service")
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
		"effective":{"reviewColumn":"off","autoAssignReviewer":true,"defaultWorkerSlots":2,"stagePersonas":{}}
	}`, w.Body.String())

	w = doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"reviewColumn":"auto","autoAssignReviewer":true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, svc.lastProjectConfigSet)
	require.NotNil(t, svc.lastProjectConfigSet.ReviewColumn)
	require.NotNil(t, svc.lastProjectConfigSet.AutoAssignReviewer)
	assert.Equal(t, "auto", *svc.lastProjectConfigSet.ReviewColumn)
	assert.True(t, *svc.lastProjectConfigSet.AutoAssignReviewer)
	assert.Nil(t, svc.lastProjectConfigSet.DefaultWorkerSlots, "defaultWorkerSlots stays off legal project PUTs (user-level key)")
	// REST actor stamping: the JWT user, user kind, rest surface, no token.
	assert.Equal(t, board.Actor{Kind: board.ActorUser, UserID: "user-1", Surface: board.SurfaceREST}, svc.lastActor)
}

// TestBoardConfigREST_BodyCap: the 64 KiB MaxBytesReader gate in
// bindBoardConfigOverride — an over-cap body rejects 400 before any
// decode, never reaching the service (the round-3 review's missing
// pin).
func TestBoardConfigREST_BodyCap(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardConfigRouter(t, svc)
	huge := `{"reviewColumn":"` + strings.Repeat("a", 64<<10) + `"}`
	w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/board/config", huge)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "too large", "the cap names the MaxBytesReader error")
	assert.Equal(t, 0, svc.calls["set_user_board_config"], "an over-cap body never reaches the service")
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

	t.Run("adapter rejects null value", func(t *testing.T) {
		w := doBoardREST(t, r, http.MethodPut, "/api/v1/me/projects/p1/config", `{"autoAssignReviewer":null}`)
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

	t.Run("user-level key ruling renders 400 through the modeled seam", func(t *testing.T) {
		// Round-3: the fake now models the real service's ruling, so
		// the project-PUT defaultWorkerSlots 400 is pinned here too —
		// no fake-200 masking the real seam's rejection (the
		// handler-integration suite pins the same against the REAL
		// service + Postgres). Fresh fake: no errFor leakage from the
		// subtest above.
		svc2 := newFakeBoardManager()
		svc2.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", WorkerSlots: 2, Status: board.ProjectActive}}
		r2 := newBoardConfigRouter(t, svc2)
		w := doBoardREST(t, r2, http.MethodPut, "/api/v1/me/projects/p1/config", `{"defaultWorkerSlots":5}`)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_input")
		assert.Contains(t, w.Body.String(), "user-level key", "the rejection names the ruling")
	})

	t.Run("missing project renders 404", func(t *testing.T) {
		svc2 := newFakeBoardManager() // no projects
		r2 := newBoardConfigRouter(t, svc2)
		w := doBoardREST(t, r2, http.MethodGet, "/api/v1/me/projects/nope/config", "")
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	})
}

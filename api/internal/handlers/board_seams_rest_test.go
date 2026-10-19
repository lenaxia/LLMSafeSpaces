// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_seams_rest_test.go — red-first pins for the #1699 seam gap
// closings at the REST adapter: the three board_read projections (epic
// listing, board search, board health) and the requestId threading the
// #1728 review flagged (lifecycle event writers + remove-dependency's
// query-param form). RED until the adapter threads and the engine seams
// exist (the fake here pins the ADAPTER contract; the engine seams are
// pinned in pkg/board's integration suite).

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardREST_ListProjectEpics: the epic listing renders the seam's
// rows verbatim under the read perimeter (owner project required).
func TestBoardREST_ListProjectEpics(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.epics = []board.Epic{
		{ID: "e1", ShortID: "epc-bbbb1111", ProjectID: "p1", Title: "First", Status: board.EpicActive, ProgressDone: 1, ProgressTotal: 2, ProgressDerived: true},
		{ID: "e2", ShortID: "epc-bbbb2222", ProjectID: "p1", Title: "Second", Status: board.EpicDraft},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/epics", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["list_epics"])
	assert.Equal(t, "p1", svc.lastProjectID, "listing rides the RESOLVED project (perimeter first)")

	var body struct {
		Epics []board.Epic `json:"epics"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Epics, 2)
	assert.True(t, body.Epics[0].ProgressDerived, "derived progress rides verbatim")
	assert.Contains(t, w.Body.String(), "epc-bbbb1111")

	// Foreign project: plain 404, never disclosed.
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "someone-else", Status: board.ProjectActive}}
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/epics", "")
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Empty project renders an empty array, not null.
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.epics = nil
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/projects/prj-aaaa1111/epics", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"epics":[]}`, w.Body.String())
}

// TestBoardREST_BoardSearch: q + filters thread to the seam; the seam's
// results + suggestions render verbatim; the adapter's own validation
// (stage filter vocabulary) rejects unknown stages.
func TestBoardREST_BoardSearch(t *testing.T) {
	svc := newFakeBoardManager()
	svc.searchResult = &board.SearchResult{
		Results:     []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Title: "Fix login bug", Stage: board.StageBacklog}},
		Suggestions: []string{"tsk-cccc3334"},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/search?q=login&tag=auth&tag=urgent&stage=backlog&project=prj-aaaa1111&epic=epc-bbbb1111", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["search_tasks"])
	assert.Equal(t, "login", svc.lastSearchQuery)
	assert.Equal(t, []string{"auth", "urgent"}, svc.lastSearchFilters.Tags)
	assert.Equal(t, board.StageBacklog, svc.lastSearchFilters.Stage)
	assert.Equal(t, "prj-aaaa1111", svc.lastSearchFilters.Project)
	assert.Equal(t, "epc-bbbb1111", svc.lastSearchFilters.Epic)

	var res board.SearchResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	require.Len(t, res.Results, 1)
	assert.Equal(t, "tsk-cccc3333", res.Results[0].ShortID)
	assert.Equal(t, []string{"tsk-cccc3334"}, res.Suggestions)

	// Missing q is a 400 (the adapter validates presence).
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/search", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Unknown stage filter: the enumerated-set validation.
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/search?q=x&stage=bogus", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// Seam input error renders through the shared error mapping.
	svc.errFor["search_tasks"] = &board.GuardError{Code: board.GuardInput, Detail: "q too long"}
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/search?q=x", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "q too long")
}

// TestBoardREST_BoardHealth: the health snapshot renders verbatim for
// the JWT user.
func TestBoardREST_BoardHealth(t *testing.T) {
	svc := newFakeBoardManager()
	svc.health = &board.Health{
		ActiveLeases:         []board.HealthLease{{TaskID: "t1", ShortID: "tsk-cccc3333", LeaseExpiresAt: time.Now().Add(time.Hour)}},
		PendingInputByOrigin: map[string]int{"escalated": 1},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/board/health", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["board_health"])
	assert.Equal(t, "user-1", svc.lastHealthUser, "health is scoped to the JWT user")

	var h board.Health
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &h))
	require.Len(t, h.ActiveLeases, 1)
	assert.Equal(t, "tsk-cccc3333", h.ActiveLeases[0].ShortID)
	assert.EqualValues(t, 1, h.PendingInputByOrigin["escalated"])
}

// TestBoardREST_LifecycleRequestID: the six lifecycle actions accept an
// optional {requestId} body and thread it to the seam (the #1728-flagged
// engine follow-up — replay protection at every tier). Absent bodies
// stay fine (both fields optional).
func TestBoardREST_LifecycleRequestID(t *testing.T) {
	cases := []struct {
		name string
		path string
		call string
	}{
		{"archive_project", "/api/v1/me/projects/prj-aaaa1111/archive", "archive_project"},
		{"unarchive_project", "/api/v1/me/projects/prj-aaaa1111/unarchive", "unarchive_project"},
		{"epic_activate", "/api/v1/me/epics/epc-bbbb2222/activate", "epic_activate"},
		{"epic_pause", "/api/v1/me/epics/epc-bbbb2222/pause", "epic_pause"},
		{"epic_archive", "/api/v1/me/epics/epc-bbbb2222/archive", "epic_archive"},
		{"epic_unarchive", "/api/v1/me/epics/epc-bbbb2222/unarchive", "epic_unarchive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newFakeBoardManager()
			svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
			r := newBoardRESTRouter(t, svc)

			w := doBoardREST(t, r, http.MethodPost, tc.path, `{"requestId":"lifecycle-key-1"}`)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, 1, svc.calls[tc.call])
			assert.Equal(t, "lifecycle-key-1", svc.lastRequestID, "%s must thread the body's requestId", tc.name)

			// No body: still fine, requestId empty.
			w = doBoardREST(t, r, http.MethodPost, tc.path, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "", svc.lastRequestID)
		})
	}
}

// TestBoardREST_RemoveDependencyRequestID: DELETE carries no body — the
// optional requestId rides a query parameter and threads to the seam.
func TestBoardREST_RemoveDependencyRequestID(t *testing.T) {
	svc := newFakeBoardManager()
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageBacklog}}
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-cccc4444?requestId=rm-key-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "rm-key-1", svc.lastRequestID, "remove-dependency threads the query-param requestId")

	w = doBoardREST(t, r, http.MethodDelete, "/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-cccc4444", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "", svc.lastRequestID)
}

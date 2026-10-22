// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_events_rest_test.go — #1832 item 4 red-first pins for the task
// audit-timeline read: GET /api/v1/me/tasks/:id/events renders the
// task's board_events rows (chronological ascending, newest-N window)
// behind the SAME owner perimeter every task read enforces (foreign
// rows are plain 404s — never disclosed). RED until the seam exists.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func testTaskEvents() []board.Event {
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	mk := func(id string, action string, at time.Time, payload []byte) board.Event {
		return board.Event{
			ID: id, ProjectID: "p1", ItemKind: "task", ItemID: "t1",
			Action: action, Actor: []byte(`{"kind":"system","surface":"engine"}`),
			Payload: payload, CreatedAt: at,
		}
	}
	return []board.Event{
		mk("evt-1", "create_task", base, []byte(`{"task_id":"t1"}`)),
		mk("evt-2", "claim", base.Add(time.Minute), []byte(`{"task_id":"t1","worker":"sess-1","attempts":1}`)),
		mk("evt-3", "move_stage", base.Add(2*time.Minute), []byte(`{"task_id":"t1","from_stage":"backlog","requested_to":"in_review","resulting_stage":"in_review","reclaimed":false}`)),
	}
}

// TestBoardREST_TaskEvents: the timeline read renders {events:[…]}
// in chronological (oldest→newest) order, threads the task ref through
// the seam, and applies the newest-N limit window server-side.
func TestBoardREST_TaskEvents(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageInProgress}}
	svc.events = testTaskEvents()
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Events []board.Event `json:"events"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Events, 3)
	// Chronological ascending — the timeline's order (a drawer renders
	// it top-to-bottom as history).
	assert.Equal(t, "evt-1", body.Events[0].ID)
	assert.Equal(t, "evt-3", body.Events[2].ID)
	assert.Equal(t, "claim", body.Events[1].Action)
	assert.Equal(t, "task", body.Events[0].ItemKind)
	assert.Equal(t, "t1", body.Events[0].ItemID)
	// The claim row carries its payload (who claimed — the drawer's
	// "claims (who/when/lease state)" arm reads exactly this).
	assert.Contains(t, string(body.Events[1].Payload), `"worker":"sess-1"`)
	assert.Equal(t, "tsk-cccc3333", svc.lastTaskEventsRef)
	assert.Equal(t, 1, svc.calls["task_events"], "the seam fired exactly once")
}

// TestBoardREST_TaskEventsLimit: the optional ?limit= threads through
// to the seam (the newest-N window); an unusable limit is a 400, never
// a silent default.
func TestBoardREST_TaskEventsLimit(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageInProgress}}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events?limit=50", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 50, svc.lastTaskEventsLimit)

	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events?limit=0", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events?limit=cheese", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestBoardREST_TaskEventsOwnerScoped: the timeline sits behind the
// same ownership perimeter as every task read — a foreign project's
// task renders a plain 404 and the seam NEVER fires (no disclosure).
func TestBoardREST_TaskEventsOwnerScoped(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "someone-else", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageInProgress}}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.NotContains(t, w.Body.String(), "someone-else")
	assert.Equal(t, 0, svc.calls["task_events"], "foreign rows never reach the timeline seam")
}

// TestBoardREST_TaskEventsErrorClass: service failures render through
// the shared error mapping (NotFoundError → 404) — the timeline read
// keeps the REST column of the parity matrix.
func TestBoardREST_TaskEventsErrorClass(t *testing.T) {
	svc := newFakeBoardManager()
	svc.projects = []board.Project{{ID: "p1", ShortID: "prj-aaaa1111", UserID: "user-1", Status: board.ProjectActive}}
	svc.tasks = []board.Task{{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1", Stage: board.StageInProgress}}
	svc.errFor["task_events"] = &board.NotFoundError{Item: "task", Identifier: "t1"}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodGet, "/api/v1/me/tasks/tsk-cccc3333/events", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

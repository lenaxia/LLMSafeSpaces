// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_bulk_rest_test.go — #1836's bulk claim/complete REST projection
// pins (red-first): POST /api/v1/me/tasks/bulk-start and
// POST /api/v1/me/tasks/bulk-move-stage are thin adapters over the
// engine's BulkTaskStart/BulkMoveStage — the adapter owns binding +
// presence validation (taskIds non-empty, toStage present and in the
// five-stage vocabulary), the engine owns the batch semantics (cap,
// per-item results, partial failure). The per-item result array
// renders verbatim; guard classes map through renderBoardError like
// every other board write.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardREST_BulkStart is the bulk claim adapter: binding, actor
// stamping, requestID threading, per-item result rendering.
func TestBoardREST_BulkStart(t *testing.T) {
	svc := newFakeBoardManager()
	svc.bulkStartResults = []board.BulkItemResult{
		{Ref: "tsk-aaa", Status: board.BulkDone},
		{Ref: "tsk-bbb", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.BulkItemNotFound, Detail: "nope"}},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-start",
		`{"taskIds":["tsk-aaa","tsk-bbb"],"requestId":"bulk-req-9"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["bulk_start"], "the engine seam is reached once per batch, not per item")
	assert.Equal(t, []string{"tsk-aaa", "tsk-bbb"}, svc.lastBulkStartRefs)
	assert.Equal(t, "bulk-req-9", svc.lastBulkStartRequestID)
	assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface, "the REST actor stamp")
	assert.Empty(t, svc.lastActor.ClaimToken, "the owner surface holds no claim token")

	var body struct {
		Results []board.BulkItemResult `json:"results"`
		Count   int                    `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body.Results, 2, "the per-item array renders verbatim")
	assert.Equal(t, board.BulkDone, body.Results[0].Status)
	assert.Equal(t, board.BulkFailed, body.Results[1].Status)
	assert.Equal(t, board.BulkItemNotFound, body.Results[1].Error.Code)
	assert.Equal(t, 2, body.Count)
}

// TestBoardREST_BulkStartBinding: the adapter's own validation — a
// missing or empty taskIds is a 400 before the seam is reached.
func TestBoardREST_BulkStartBinding(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-start", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-start", `{"taskIds":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-start", `not-json`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Zero(t, svc.calls["bulk_start"])
}

// TestBoardREST_BulkMoveStage is the bulk complete adapter: toStage
// presence + five-stage vocabulary, evidence/requestID threading,
// per-item rendering.
func TestBoardREST_BulkMoveStage(t *testing.T) {
	svc := newFakeBoardManager()
	svc.bulkMoveResults = []board.BulkItemResult{
		{Ref: "tsk-aaa", Status: board.BulkDone},
		{Ref: "tsk-bbb", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.GuardDone, Detail: "criteria unmet"}},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-move-stage",
		`{"taskIds":["tsk-aaa","tsk-bbb"],"toStage":"done","evidenceUrl":"https://example/p","requestId":"bulk-move-9"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["bulk_move_stage"])
	assert.Equal(t, []string{"tsk-aaa", "tsk-bbb"}, svc.lastBulkMoveRefs)
	assert.Equal(t, board.StageDone, svc.lastBulkMoveTo)
	assert.Equal(t, "https://example/p", svc.lastBulkMoveEvidence)
	assert.Equal(t, "bulk-move-9", svc.lastBulkMoveRequestID)
	assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface)

	var body struct {
		Results []board.BulkItemResult `json:"results"`
		Count   int                    `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body.Results, 2)
	assert.Equal(t, 2, body.Count)
}

// TestBoardREST_BulkMoveStageBinding: toStage is required and must be
// one of the five stages; taskIds presence mirrors bulk-start.
func TestBoardREST_BulkMoveStageBinding(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-move-stage",
		`{"taskIds":["tsk-aaa"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "toStage")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-move-stage",
		`{"taskIds":["tsk-aaa"],"toStage":"cancel"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "five stages", "the eliminated stage is just a misspelling now")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-move-stage",
		`{"toStage":"done"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")
	assert.Zero(t, svc.calls["bulk_move_stage"])
}

// TestBoardREST_BulkEngineGuardPassThrough: the engine's batch-level
// guard (over-cap, off-vocabulary — invalid_input) renders through the
// shared error mapper, engine code intact.
func TestBoardREST_BulkEngineGuardPassThrough(t *testing.T) {
	svc := newFakeBoardManager()
	svc.errFor["bulk_start"] = &board.GuardError{Code: board.GuardInput, Detail: "bulk batch capped at 50 items (got 51) — split the batch"}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-start",
		`{"taskIds":["tsk-aaa"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), board.GuardInput)
	assert.Contains(t, w.Body.String(), "capped at 50")
}

// compile-time: the fake keeps satisfying the seam as it grows.
var _ boardManager = (*fakeBoardManager)(nil)

// TestBoardREST_BulkDelete is the bulk delete adapter (#1836
// delete_task(s) sibling): binding, reconnect flag threading (ONE
// shared flag across the batch), actor stamping, requestID threading,
// per-item result rendering with the orphan echo verbatim.
func TestBoardREST_BulkDelete(t *testing.T) {
	svc := newFakeBoardManager()
	svc.bulkDeleteResults = []board.BulkItemResult{
		{Ref: "tsk-aaa", Status: board.BulkDone, Orphaned: []string{"tsk-orph"}},
		{Ref: "tsk-bbb", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.BulkItemNotFound, Detail: "nope"}},
	}
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-delete",
		`{"taskIds":["tsk-aaa","tsk-bbb"],"reconnect":true,"requestId":"bulk-del-9"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["bulk_delete"], "the engine seam is reached once per batch, not per item")
	assert.Equal(t, []string{"tsk-aaa", "tsk-bbb"}, svc.lastBulkDeleteRefs)
	assert.True(t, svc.lastBulkDeleteReconnect, "the shared reconnect flag threads through")
	assert.Equal(t, "bulk-del-9", svc.lastBulkDeleteRequestID)
	assert.Equal(t, board.SurfaceREST, svc.lastActor.Surface, "the REST actor stamp")
	assert.Empty(t, svc.lastActor.ClaimToken, "the owner surface holds no claim token")

	var body struct {
		Results []board.BulkItemResult `json:"results"`
		Count   int                    `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body.Results, 2, "the per-item array renders verbatim")
	assert.Equal(t, board.BulkDone, body.Results[0].Status)
	assert.Equal(t, []string{"tsk-orph"}, body.Results[0].Orphaned, "the orphan echo renders")
	assert.Equal(t, board.BulkFailed, body.Results[1].Status)
	assert.Equal(t, board.BulkItemNotFound, body.Results[1].Error.Code)
	assert.Equal(t, 2, body.Count)
}

// TestBoardREST_BulkDeleteBinding: the adapter's own validation — a
// missing or empty taskIds is a 400 before the seam is reached.
func TestBoardREST_BulkDeleteBinding(t *testing.T) {
	svc := newFakeBoardManager()
	r := newBoardRESTRouter(t, svc)

	w := doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-delete", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-delete", `{"taskIds":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")

	w = doBoardREST(t, r, http.MethodPost, "/api/v1/me/tasks/bulk-delete", `not-json`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Zero(t, svc.calls["bulk_delete"])
}

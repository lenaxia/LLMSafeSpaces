// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_manager_bulk_test.go — #1836's bulk complete on the pod seam:
// POST /internal/v1/automation/board/tasks/move-stage-bulk is the
// manager-grade batch stage move (the #1832 WS2 single move's bulk
// twin), open to ANY in-workspace session (the #1747 §5 manager-grant
// gate is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates — the resolved sessionId is audit
// attribution only). The bulk CLAIM variant deliberately does NOT
// exist on this surface — task_start is owner-surface only (round-3
// #2), and the bulk form follows the single form's boundary.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestBoardAutomation_ManagerMoveStageBulk(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.bulkMoveResults = []board.BulkItemResult{
		{Ref: "tsk-bulk1111", Status: board.BulkDone},
		{Ref: "tsk-nope-nope", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.BulkItemNotFound, Detail: "miss"}},
	}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId → the batch PROCEEDS (attribution-only identity; the
	// omitted-identity 403 is gone with the repealed gate).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","taskIds":["tsk-bulk1111"],"toStage":"done"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["bulk_move_stage"], "the manager projection is reached without a session identity")
	require.NotNil(t, mgr.lastActor)
	assert.Empty(t, mgr.lastActor.SessionID, "no identity supplied — unattributed audit actor")

	// A live worker session → the batch PROCEEDS too (the grant belt is
	// repealed; workers can manager-move in bulk like any session).
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","taskIds":["tsk-bulk1111"],"toStage":"done"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, mgr.calls["bulk_move_stage"])

	// Any other session → 200; the actor carries the manager-grade
	// surface + session identity; per-item array renders.
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","taskIds":["tsk-bulk1111","tsk-nope-nope"],"toStage":"done","evidenceUrl":"https://example/p","requestId":"bm-1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 3, mgr.calls["bulk_move_stage"])
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface)
	assert.Equal(t, "ses_orchestrator", mgr.lastActor.SessionID)
	assert.Empty(t, mgr.lastActor.ClaimToken)
	assert.Equal(t, []string{"tsk-bulk1111", "tsk-nope-nope"}, mgr.lastBulkMoveRefs)
	assert.Equal(t, board.StageDone, mgr.lastBulkMoveTo)
	assert.Equal(t, "https://example/p", mgr.lastBulkMoveEvidence)
	assert.Equal(t, "bm-1", mgr.lastBulkMoveRequestID)
	assert.Contains(t, w.Body.String(), `"status":"failed"`, "partial failures render per-item, not as a batch error")
	assert.Contains(t, w.Body.String(), board.BulkItemNotFound)
}

// Binding: toStage presence + vocabulary, taskIds presence — the same
// adapter-owned validation as the REST column (parity).
func TestBoardAutomation_ManagerMoveStageBulkBinding(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","taskIds":["tsk-bulk1111"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "toStage")

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","toStage":"cancel","taskIds":["tsk-bulk1111"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "wire stages")
	assert.Contains(t, w.Body.String(), "failed")

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","toStage":"done","taskIds":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")
	assert.Zero(t, mgr.calls["bulk_move_stage"])
}

// TestBoardAutomation_ManagerDeleteTasksBulk: #1836's bulk delete on
// the pod seam (board_delete_task's batch twin — delete IS a manager
// action; only task_start/task_create carry the owner-only boundary).
// Open to ANY in-workspace session (the #1747 §5 manager-grant gate is
// repealed, ruling 2026-10-27 / tsk-copper-refectory-generates); the
// resolved sessionId is audit attribution only; per-item array renders
// with the orphan echo.
func TestBoardAutomation_ManagerDeleteTasksBulk(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.bulkDeleteResults = []board.BulkItemResult{
		{Ref: "tsk-bulk1111", Status: board.BulkDone, Orphaned: []string{"tsk-orph"}},
		{Ref: "tsk-nope-nope", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.BulkItemNotFound, Detail: "miss"}},
	}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId → the batch PROCEEDS (attribution-only identity).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","taskIds":["tsk-bulk1111"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["bulk_delete"], "the manager projection is reached without a session identity")
	require.NotNil(t, mgr.lastActor)
	assert.Empty(t, mgr.lastActor.SessionID, "no identity supplied — unattributed audit actor")

	// A live worker session → the batch PROCEEDS too (the grant belt is
	// repealed).
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","taskIds":["tsk-bulk1111"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, mgr.calls["bulk_delete"])

	// Any other session → 200; the actor carries the manager-grade
	// surface + session identity; per-item array renders.
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","taskIds":["tsk-bulk1111","tsk-nope-nope"],"reconnect":true,"requestId":"bd-1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 3, mgr.calls["bulk_delete"])
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface)
	assert.Equal(t, "ses_orchestrator", mgr.lastActor.SessionID)
	assert.Empty(t, mgr.lastActor.ClaimToken)
	assert.Equal(t, []string{"tsk-bulk1111", "tsk-nope-nope"}, mgr.lastBulkDeleteRefs)
	assert.True(t, mgr.lastBulkDeleteReconnect)
	assert.Equal(t, "bd-1", mgr.lastBulkDeleteRequestID)
	assert.Contains(t, w.Body.String(), `"status":"failed"`, "partial failures render per-item, not as a batch error")
	assert.Contains(t, w.Body.String(), board.BulkItemNotFound)
	assert.Contains(t, w.Body.String(), "tsk-orph", "the orphan echo renders on the pod seam too")
}

// Binding: taskIds presence — the same adapter-owned validation as the
// REST column (parity).
func TestBoardAutomation_ManagerDeleteTasksBulkBinding(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","taskIds":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk", `not-json`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["bulk_delete"])
}

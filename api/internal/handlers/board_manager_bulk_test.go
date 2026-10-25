// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_manager_bulk_test.go — #1836's bulk complete on the pod seam:
// POST /internal/v1/automation/board/tasks/move-stage-bulk is the
// manager-grade batch stage move (the #1832 WS2 single move's bulk
// twin), behind the same resolveManager belt (harness-attested session
// identity, live-assignee worker refusal, fail-closed omission). The
// bulk CLAIM variant deliberately does NOT exist on this surface —
// task_start is owner-surface only (round-3 #2), and the bulk form
// follows the single form's boundary.

import (
	"fmt"
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

	// No sessionId → 403 fail-closed (the belt's omission rule).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","taskIds":["tsk-bulk1111"],"toStage":"done"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "an unidentified caller holds no manager grant")
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)
	assert.Zero(t, mgr.calls["bulk_move_stage"])

	// A live worker session → 403 (the grant belt).
	mgr.workerSession = "ses_liveworker"
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","taskIds":["tsk-bulk1111"],"toStage":"done"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["bulk_move_stage"])

	// The orchestrator-class session → 200; the actor carries the
	// manager-grade surface + session identity; per-item array renders.
	mgr.workerSession = ""
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","taskIds":["tsk-bulk1111","tsk-nope-nope"],"toStage":"done","evidenceUrl":"https://example/p","requestId":"bm-1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["bulk_move_stage"])
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

// The belt ERROR is a refusal on the bulk route too (fail-closed).
func TestBoardAutomation_ManagerMoveStageBulkBeltFailClosed(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.beltErr = fmt.Errorf("db blip")
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","taskIds":["tsk-bulk1111"],"toStage":"in_progress"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["bulk_move_stage"])
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
	assert.Contains(t, w.Body.String(), "five stages")

	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","toStage":"done","taskIds":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "taskIds")
	assert.Zero(t, mgr.calls["bulk_move_stage"])
}

// TestBoardAutomation_ManagerDeleteTasksBulk: #1836's bulk delete on
// the pod seam (board_delete_task's batch twin — delete IS a manager
// action; only task_start/task_create carry the owner-only boundary).
// Same resolveManager belt: fail-closed identity, live-assignee worker
// refusal, belt-error 503; per-item array renders with the orphan echo.
func TestBoardAutomation_ManagerDeleteTasksBulk(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.bulkDeleteResults = []board.BulkItemResult{
		{Ref: "tsk-bulk1111", Status: board.BulkDone, Orphaned: []string{"tsk-orph"}},
		{Ref: "tsk-nope-nope", Status: board.BulkFailed, Error: &board.BulkItemError{Code: board.BulkItemNotFound, Detail: "miss"}},
	}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId → 403 fail-closed (the belt's omission rule).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","taskIds":["tsk-bulk1111"]}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "an unidentified caller holds no manager grant")
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)
	assert.Zero(t, mgr.calls["bulk_delete"])

	// A live worker session → 403 (the grant belt).
	mgr.workerSession = "ses_liveworker"
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","taskIds":["tsk-bulk1111"]}`)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["bulk_delete"])

	// The orchestrator-class session → 200; the actor carries the
	// manager-grade surface + session identity; per-item array renders.
	mgr.workerSession = ""
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","taskIds":["tsk-bulk1111","tsk-nope-nope"],"reconnect":true,"requestId":"bd-1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["bulk_delete"])
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

// The belt ERROR is a refusal on the bulk delete route too (fail-closed).
func TestBoardAutomation_ManagerDeleteTasksBulkBeltFailClosed(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.beltErr = fmt.Errorf("db blip")
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/delete-bulk",
		`{"workspaceID":"ws-1","sessionId":"ses_x","taskIds":["tsk-bulk1111"]}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["bulk_delete"])
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

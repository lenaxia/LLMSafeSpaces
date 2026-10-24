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

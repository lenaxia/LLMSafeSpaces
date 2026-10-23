// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS2 — the manager-grade move on the pod seam: the pending_input
// exits become serviceable by orchestrator-class sessions (the
// tsk-3do6n458 freeze class), behind the same resolveManager belt as
// every manager route (harness-attested identity, live-assignee worker
// refusal, fail-closed omission).

func TestBoardAutomation_ManagerMoveStage(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId → 403 fail-closed (the belt's omission rule).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","toStage":"backlog"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "an unidentified caller holds no manager grant")
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)
	assert.Zero(t, mgr.calls["move_stage"], "the manager projection is never reached")

	// A live worker session → 403 (the grant belt: spawned workers
	// never reach the manager move, token or not).
	mgr.workerSession = "ses_liveworker"
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","toStage":"backlog"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["move_stage"])

	// The orchestrator-class session → 200; the actor carries the
	// manager-grade surface + the session identity (audit attribution).
	mgr.workerSession = ""
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","toStage":"backlog","evidenceUrl":"https://example/why"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["move_stage"])
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface,
		"the manager-grade surface is what the stage machine's pending_input guard reads")
	assert.Equal(t, "ses_orchestrator", mgr.lastActor.SessionID)
	assert.Empty(t, mgr.lastActor.ClaimToken, "manager actions hold no claim token")
	assert.Equal(t, board.StageBacklog, mgr.lastMoveStageTo)
	assert.Equal(t, "https://example/why", mgr.lastMoveStageEvidence)
}

// The belt ERROR is a refusal on the move route too (fail-closed,
// never a pass-through).
func TestBoardAutomation_ManagerMoveStageBeltFailClosed(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.beltErr = fmt.Errorf("db blip")
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","sessionId":"ses_x","toStage":"in_progress"}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, mgr.calls["move_stage"])
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 WS2 — the manager-grade move on the pod seam: the pending_input
// exits are serviceable by ANY in-workspace session (the #1747 §5
// manager-grant gate — session-identity requirement + live-assignee
// worker refusal — is REPEALED, ruling 2026-10-27 /
// tsk-copper-refectory-generates). The resolved sessionId, when
// supplied, rides the actor as audit attribution only.

func TestBoardAutomation_ManagerMoveStage(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	// No sessionId → the call PROCEEDS (attribution-only identity; the
	// omitted-identity 403 is gone with the repealed gate).
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","toStage":"backlog"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, mgr.calls["move_stage"], "the manager projection is reached without a session identity")
	require.NotNil(t, mgr.lastActor)
	assert.Empty(t, mgr.lastActor.SessionID, "no identity supplied — unattributed audit actor")

	// A live worker session → the call PROCEEDS too (the grant belt is
	// gone; workers can manager-move — the claim-token fence, not
	// session RBAC, is the in-flight protection).
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","sessionId":"ses_liveworker","toStage":"backlog"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 2, mgr.calls["move_stage"])

	// Any other session → 200; the actor carries the manager-grade
	// surface + the session identity (audit attribution).
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/tsk-move111/move-stage",
		`{"workspaceID":"ws-1","sessionId":"ses_orchestrator","toStage":"backlog","evidenceUrl":"https://example/why"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 3, mgr.calls["move_stage"])
	assert.Equal(t, board.SurfaceAgentManager, mgr.lastActor.Surface,
		"the manager-grade surface is what the stage machine's pending_input guard reads")
	assert.Equal(t, "ses_orchestrator", mgr.lastActor.SessionID)
	assert.Empty(t, mgr.lastActor.ClaimToken, "manager actions hold no claim token")
	assert.Equal(t, board.StageBacklog, mgr.lastMoveStageTo)
	assert.Equal(t, "https://example/why", mgr.lastMoveStageEvidence)
}

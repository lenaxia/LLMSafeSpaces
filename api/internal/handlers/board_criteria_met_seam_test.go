// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// board_criteria_met_seam_test.go — the #1946 scoped grant's worker
// route: the reviewer persona's per-criterion flag write. The handler
// is a thin adapter (the mcpAutomation discipline): actor stamping via
// boardActor, the surface's LOUD narrow vocabulary, and the guard-class
// HTTP mapping through renderBoardError.

// TestBoardSetCriteriaMet_ActorStampingAndEcho: the happy path stamps
// the worker delegation (owner, agent surface, the presented claim
// token, the workspace binding) and echoes the mutated criterion.
func TestBoardSetCriteriaMet_ActorStampingAndEcho(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","shortId":"tsk-aaaa1111","claimToken":"tok-rev","setCriteriaMet":{"index":1,"met":true}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["criteria_met"])
	assert.Equal(t, "user-7", svc.lastActor.UserID, "the pod's resolved owner delegates (R7)")
	assert.Equal(t, board.SurfaceAgent, svc.lastActor.Surface, "the grant rides the WORKER surface")
	assert.Equal(t, "tok-rev", svc.lastActor.ClaimToken)
	assert.Equal(t, "ws-1", svc.lastActor.WorkspaceID, "the resolved workspace rides the actor (S1)")
	assert.Equal(t, "tsk-aaaa1111", svc.lastShortID)
	require.NotNil(t, svc.lastCheck)
	assert.Equal(t, 1, svc.lastCheck.Index)
	assert.True(t, svc.lastCheck.Met)

	var body taskEchoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.MutatedCriterion, "the #1837 P2b echo: index, flag, text")
	assert.Equal(t, 1, body.MutatedCriterion.Index)
	assert.True(t, body.MutatedCriterion.Met)
	assert.Equal(t, "criterion 1", body.MutatedCriterion.Text)
}

// TestBoardSetCriteriaMet_LoudVocabulary: the grant covers setCriteriaMet
// ONLY — any other field refuses by name; the injected identity key is
// the one tolerated rider (the plugin stamps it unconditionally).
func TestBoardSetCriteriaMet_LoudVocabulary(t *testing.T) {
	svc := newFakeBoardService()
	r := newBoardAutomationRouter(t, svc)

	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","taskId":"t-1","claimToken":"tok-rev","title":"sneaky","setCriteriaMet":{"index":0,"met":true}}`)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "title")
	assert.Contains(t, w.Body.String(), board.GuardInput)
	assert.Equal(t, 0, svc.calls["criteria_met"], "no service call for a refused shape")

	// The harness-injected session key never trips the vocabulary check.
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","taskId":"t-1","claimToken":"tok-rev","lsp_injected_session":"ses_rev1","setCriteriaMet":{"index":0,"met":false}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, svc.calls["criteria_met"])

	// Missing the check itself is loud too.
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","taskId":"t-1","claimToken":"tok-rev"}`)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "setCriteriaMet is required")
	assert.Equal(t, 1, svc.calls["criteria_met"], "unchanged: the refusal never reaches the service")
}

// TestBoardSetCriteriaMet_GuardClassMapping: the grant's guard classes
// render through the shared parity mapping — the #1747 §5 refusal
// carries its class (manager_surface) and text verbatim (the same
// render as the pending_input worker-exit §5 refusal), the claim-token
// class 409s.
func TestBoardSetCriteriaMet_GuardClassMapping(t *testing.T) {
	svc := newFakeBoardService()
	svc.setCritErr = &board.GuardError{Code: board.GuardManagerSurface,
		Detail: "criteria-met recording is the reviewer-persona claim's scoped grant (issue #1946) — manager board tools attach to user-collaborator sessions only (ruling #1747 §5)"}
	r := newBoardAutomationRouter(t, svc)
	w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","taskId":"t-1","claimToken":"tok-impl","setCriteriaMet":{"index":0,"met":true}}`)
	require.Equal(t, http.StatusConflict, w.Code, "the §5 class renders through the shared guard mapping (the pending_input §5 refusal renders the same)")
	assert.Contains(t, w.Body.String(), board.GuardManagerSurface)
	assert.Contains(t, w.Body.String(), "#1747")

	svc.setCritErr = &board.GuardError{Code: board.GuardClaimToken, Detail: "criteria-met writes require the row's live persona-claim token"}
	w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/set-criteria-met",
		`{"workspaceID":"ws-1","taskId":"t-1","claimToken":"stale","setCriteriaMet":{"index":0,"met":true}}`)
	require.Equal(t, http.StatusConflict, w.Code, "the claim-token class renders 409")
	assert.Contains(t, w.Body.String(), board.GuardClaimToken)
}

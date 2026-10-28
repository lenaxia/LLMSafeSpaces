// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation_config_test.go — board_get_config / board_set_config
// over the internal automation seam: the read renders the
// source-annotated view (either tier, the #1747 workspace binding on
// the project arm), the write decodes the config keys through the REAL
// key-gate (the platform validates — unknown keys and malformed values
// refuse 400 before any fake arm fires), and the tier selector's
// refusal shapes are pinned.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// view captures for the fake's GetBoardConfigView arm (a minimal
// literal — the handler seam is what these tests pin; the builder's
// annotation logic is pinned in pkg/board).
func (f *fakeBoardManager) GetBoardConfigView(_ context.Context, actor board.Actor, projectID, shortIDOrID string) (*board.BoardConfigView, error) {
	f.call("get_board_config_view")
	f.lastActor = actor
	f.lastProjectID = projectID
	if projectID == "" {
		f.lastProjectID = shortIDOrID
	}
	if err := f.errFor["get_board_config_view"]; err != nil {
		return nil, err
	}
	view := board.BoardConfigView{
		Scope:     "user",
		Effective: board.DefaultBoardConfig(),
		Sources:   map[string]string{board.ConfigKeyReviewColumn: board.ConfigSourceCodeDefault},
		Personas:  map[string]board.BoardPersonaView{},
	}
	if f.lastProjectID != "" {
		view.Scope = "project"
		view.ProjectID = f.lastProjectID
	}
	return &view, nil
}

func TestBoardAutomationConfig_GetConfig(t *testing.T) {
	mgr := newFakeBoardManager()
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	t.Run("user tier: no projectId, read-open (no session required)", func(t *testing.T) {
		w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/config?workspaceID=ws-1", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1, mgr.calls["get_board_config_view"])
		assert.Equal(t, "user-7", mgr.lastActor.UserID, "the pod's resolved owner delegates (R7)")
		assert.Equal(t, "ws-1", mgr.lastActor.WorkspaceID)
		assert.Empty(t, mgr.lastProjectID, "no projectId → the user tier")
		// The rendered shape: source annotations + persona provenance.
		assert.Contains(t, w.Body.String(), `"sources"`)
		assert.Contains(t, w.Body.String(), `"personas"`)
		assert.Contains(t, w.Body.String(), `"scope":"user"`)
	})

	t.Run("project tier: the projectId rides the view read", func(t *testing.T) {
		w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/config?workspaceID=ws-1&projectId=prj-aaaa1111", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "prj-aaaa1111", mgr.lastProjectID)
		assert.Contains(t, w.Body.String(), `"scope":"project"`)
	})

	t.Run("a view error renders through the board error map", func(t *testing.T) {
		mgr := newFakeBoardManager()
		mgr.errFor["get_board_config_view"] = &board.GuardError{Code: board.GuardWorkspaceBinding, Detail: "agent manager actions are workspace-scoped"}
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/config?workspaceID=ws-1&projectId=prj-other", "")
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, w.Body.String(), board.GuardWorkspaceBinding)
	})
}

func TestBoardAutomationConfig_SetConfig(t *testing.T) {
	t.Run("tier=user: config keys pass through verbatim, the response is the annotated view", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","sessionId":"ses_caller","tier":"user","reviewColumn":"manual","stagePersonas":{"in_review":{"name":"auditor","instructions":"Verify."}}}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1, mgr.calls["set_user_board_config"])
		require.NotNil(t, mgr.lastGlobalConfigSet)
		require.NotNil(t, mgr.lastGlobalConfigSet.ReviewColumn)
		assert.Equal(t, "manual", *mgr.lastGlobalConfigSet.ReviewColumn)
		require.NotNil(t, mgr.lastGlobalConfigSet.StagePersonas)
		assert.Contains(t, *mgr.lastGlobalConfigSet.StagePersonas, "in_review")
		assert.Equal(t, "ses_caller", mgr.lastActor.SessionID, "the caller's session rides the audit attribution")
		assert.Contains(t, w.Body.String(), `"sources"`, "the set renders the view, not a bare echo")
	})

	t.Run("tier=project: projectId routes the override write", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","tier":"project","projectId":"prj-aaaa1111","autoAssignReviewer":false}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1, mgr.calls["set_project_board_config"])
		require.NotNil(t, mgr.lastProjectConfigSet)
		require.NotNil(t, mgr.lastProjectConfigSet.AutoAssignReviewer)
		assert.False(t, *mgr.lastProjectConfigSet.AutoAssignReviewer)
	})

	t.Run("the REAL key-gate: an unknown config key refuses 400 before any write", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","tier":"user","reviewColumn":"sideways"}`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "reviewColumn")
		assert.Zero(t, mgr.calls["set_user_board_config"], "no write fired")
	})

	t.Run("the REAL key-gate: a persona on a non-eligible stage refuses 400", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","tier":"project","projectId":"prj-aaaa1111","stagePersonas":{"backlog":{"name":"x"}}}`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "persona-eligible stages")
	})

	t.Run("refusal shapes: tier missing / bogus, projectId on the user tier, projectId missing on the project tier", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		for _, body := range []string{
			`{"workspaceID":"ws-1","reviewColumn":"manual"}`,
			`{"workspaceID":"ws-1","tier":"instance","reviewColumn":"manual"}`,
			`{"workspaceID":"ws-1","tier":"user","projectId":"prj-aaaa1111","reviewColumn":"manual"}`,
			`{"workspaceID":"ws-1","tier":"project","reviewColumn":"manual"}`,
		} {
			w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config", body)
			assert.Equal(t, http.StatusBadRequest, w.Code, body)
			assert.Contains(t, w.Body.String(), `"code":"`+board.GuardInput+`"`, body)
			assert.Contains(t, w.Body.String(), "tier", body)
		}
		assert.Zero(t, mgr.calls["set_user_board_config"])
		assert.Zero(t, mgr.calls["set_project_board_config"])
	})

	t.Run("an empty config object is the legal full-inheritance reset", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","tier":"user"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, mgr.lastGlobalConfigSet)
		assert.Nil(t, mgr.lastGlobalConfigSet.ReviewColumn)
		assert.Nil(t, mgr.lastGlobalConfigSet.StagePersonas)
	})

	t.Run("routing keys never ride the decoded config (stamped fields pop first)", func(t *testing.T) {
		mgr := newFakeBoardManager()
		r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)
		w := doBoard(t, r, http.MethodPut, "/internal/v1/automation/board/config",
			`{"workspaceID":"ws-1","tier":"user","sessionId":"ses_x"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.NotNil(t, mgr.lastGlobalConfigSet)
		assert.Nil(t, mgr.lastGlobalConfigSet.DefaultWorkerSlots)
	})
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_list_health_seam_test.go — #1837 P2a: the manager seam's two
// read routes (per-project task list with stage/claim columns + ready
// set; workspace health) — manager-gated, workspace-scoped. Red-first:
// behavioral RED against the seam router (404s until the routes land).

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestBoardSeam_ListTasks(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.wsTasks = []board.Task{
		{ID: "t1", ShortID: "tsk-list00001", Title: "one", Stage: board.StageInProgress,
			AssigneeSession: "ses-w1", LeaseExpiresAt: nil, Tags: []string{}},
		{ID: "t2", ShortID: "tsk-list00002", Title: "two", Stage: board.StageBacklog,
			IsBlocked: true, Tags: []string{}},
		{ID: "t3", ShortID: "tsk-list00003", Title: "three", Stage: board.StageBacklog, Tags: []string{}},
	}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/projects/prj-aaaa1111/tasks?workspaceID=ws-1&sessionId=ses-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Tasks     []board.Task `json:"tasks"`
		ReadySet  []string     `json:"ready_set"`
		ProjectID string       `json:"project_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Tasks, 3, "all tasks in ONE call with stage+claim columns")
	assert.Equal(t, "ses-w1", resp.Tasks[0].AssigneeSession, "claim column (assignee)")
	assert.True(t, resp.Tasks[1].IsBlocked, "blocked column")
	assert.Equal(t, []string{"tsk-list00003"}, resp.ReadySet, "ready set: unblocked+unpaused backlog rows (claimed and blocked rows excluded)")
	assert.Equal(t, "ws-1", mgr.lastListWorkspace, "workspace-scoped resolve")
}

func TestBoardSeam_ListTasksManagerGate(t *testing.T) {
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), newFakeBoardManager())

	// No session identity: the manager gate refuses (the #1747 §5 belt).
	w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/projects/prj-aaaa1111/tasks?workspaceID=ws-1", "")
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestBoardSeam_Health(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.wsHealth = &board.Health{ActiveLeases: []board.HealthLease{
		{TaskID: "t1", ShortID: "tsk-lease0001", AssigneeSession: "ses-w1"},
	}}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/health?workspaceID=ws-1&sessionId=ses-1", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Health *board.Health `json:"health"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Health)
	require.Len(t, resp.Health.ActiveLeases, 1)
	assert.Equal(t, "tsk-lease0001", resp.Health.ActiveLeases[0].ShortID)
	assert.Equal(t, "ws-1", mgr.lastHealthWorkspace, "health is workspace-scoped on the agent surface")
}

func TestBoardSeam_SetCriteriaMetObjectForm(t *testing.T) {
	// #1837 P2b (seam tier): the object form reaches the service; the
	// response echoes the mutated criterion text; combined use with a
	// criteria replace renders the service's refusal (400).
	mgr := newFakeBoardManager()
	mgr.driftTask = &board.Task{ID: "t1", ShortID: "tsk-cccc3333", ProjectID: "p1", EpicID: "e1",
		Title: "T", Stage: board.StageBacklog, Tags: []string{},
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c0"}, {Text: "c1", Met: true}, {Text: "c2"}}}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPatch, "/internal/v1/automation/board/tasks/tsk-cccc3333",
		`{"sessionId":"ses-1","workspaceID":"ws-1","setCriteriaMet":{"index":1,"met":false}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		MutatedCriterion *struct {
			Index int    `json:"index"`
			Met   bool   `json:"met"`
			Text  string `json:"text"`
		} `json:"mutated_criterion"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.MutatedCriterion, "the mutated criterion text is echoed")
	assert.Equal(t, 1, resp.MutatedCriterion.Index)
	assert.False(t, resp.MutatedCriterion.Met)
	assert.Equal(t, "c1", resp.MutatedCriterion.Text)
	require.NotNil(t, mgr.lastTaskInput.SetCriteriaMet)
	assert.Equal(t, 1, mgr.lastTaskInput.SetCriteriaMet.Index)
	assert.False(t, mgr.lastTaskInput.SetCriteriaMet.Met)
}

func TestBoardSeam_SetCriteriaMetCombinedRefused(t *testing.T) {
	mgr := newFakeBoardManager()
	mgr.errFor["update_task"] = &board.GuardError{Code: board.GuardInput,
		Detail: "acceptanceCriteria and setCriteriaMet are mutually exclusive — send one or the other"}
	r := newBoardAutomationRouterWithManager(t, newFakeBoardService(), mgr)

	w := doBoard(t, r, http.MethodPatch, "/internal/v1/automation/board/tasks/tsk-cccc3333",
		`{"sessionId":"ses-1","workspaceID":"ws-1","setCriteriaMet":{"index":0,"met":true},"acceptanceCriteria":[{"text":"r"}]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "one or the other")
}

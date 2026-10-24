// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_list_health_test.go — #1837 P2a: board_list + board_health on
// the agent manager surface (the orchestrator's overview reads; today
// board_read is per-task only). Red-first: this file landed BEFORE the
// registry entries, dispatcher cases, and loopback client methods.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoardListHealth_Registry(t *testing.T) {
	tools := mcpToolsList(t)
	for _, name := range []string{"board_list", "board_health"} {
		tool, ok := tools[name]
		require.True(t, ok, "%s must be registered (#1837 P2a)", name)
		schema := tool["inputSchema"].(map[string]any)
		if req, ok := schema["required"]; ok {
			assert.NotContains(t, req, "claimToken", "%s is a manager read, not a claim-token write", name)
		}
	}
	list := tools["board_list"]
	props := list["inputSchema"].(map[string]any)["properties"].(map[string]any)
	assert.Contains(t, props, "projectId", "board_list addresses a project")
	assert.Contains(t, props, "stage", "board_list filters by stage")
}

func TestBoardList_WireContract(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"tasks":[{"short_id":"tsk-a","stage":"in_progress","assignee_session":"ses-w"}],"ready_set":["tsk-b"]}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_list", map[string]any{
		"projectId": "prj-aaaa1111", "stage": "in_progress", "failed": "false",
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "tsk-a")
	assert.Equal(t, http.MethodGet, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/projects/prj-aaaa1111/tasks", rec.path)
	assert.Contains(t, rec.query, "workspaceID=ws-1")
	assert.Contains(t, rec.query, "sessionId=ses-1")
	assert.Contains(t, rec.query, "stage=in_progress")
	assert.Contains(t, rec.query, "failed=false")
}

func TestBoardHealth_WireContract(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"active_leases":[]}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_health", map[string]any{
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "active_leases")
	assert.Equal(t, http.MethodGet, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/health", rec.path)
	assert.Contains(t, rec.query, "workspaceID=ws-1")
	assert.Contains(t, rec.query, "sessionId=ses-1")
}

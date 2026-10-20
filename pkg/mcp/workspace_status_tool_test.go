// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// workspace_status_tool_test.go — #1760: the workspace_status MCP tool
// rides the same REST service layer (GET /workspaces/:id/status) and
// surfaces the supervisor restart slice.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkspaceStatus_ToolDescriptionCopy — #1796: the tool description
// must attribute restarts to the supervisor (the mechanism rides the
// reason field), never claim the watchdog performed a crash/oom
// restart. Pins the literal — a silent revert of the copy fails here.
func TestWorkspaceStatus_ToolDescriptionCopy(t *testing.T) {
	desc := workspaceStatusTool.Description
	assert.Contains(t, desc, "an agent restarted by the supervisor explains interrupted work",
		"the description's restart attribution must be the supervisor")
	assert.NotContains(t, desc, "supervisor health watchdog",
		"the description must not claim the watchdog performed the restart (crash/oom ride the reason field)")
}

func TestWorkspaceStatus_SupervisorRestartsSurface(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	raw := json.RawMessage(`{
		"phase": "Active",
		"supervisor": {
			"agentRestarts": 1,
			"lastAgentRestart": {
				"reason": "health_watchdog",
				"verdict": "hung",
				"lastError": "context deadline exceeded",
				"at": "2026-10-19T17:08:14Z"
			},
			"suppressedRestarts": 78,
			"lastSuppressedVerdict": "starved"
		}
	}`)
	mockClient.On("GetWorkspaceStatus", ctx, "ws-123").Return(raw, nil)

	result, err := h.workspaceStatus(ctx, makeReq("workspace_status", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	text := result.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "agentRestarts")
	assert.Contains(t, text, "health_watchdog")
	assert.Contains(t, text, "hung")
	assert.Contains(t, text, "suppressedRestarts")
	assert.Contains(t, text, "starved")
	assert.NotContains(t, text, "opencode", "MCP output stays agent-neutral (Rule 12)")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceStatus_NoSupervisorBlock(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	raw := json.RawMessage(`{"phase": "Active", "agentHealth": {"status": "Healthy"}}`)
	mockClient.On("GetWorkspaceStatus", ctx, "ws-123").Return(raw, nil)

	result, err := h.workspaceStatus(ctx, makeReq("workspace_status", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "Active")
	mockClient.AssertExpectations(t)
}

func TestWorkspaceStatus_MissingID(t *testing.T) {
	h, _ := newTestHandlers()

	result, err := h.workspaceStatus(context.Background(), makeReq("workspace_status", map[string]any{}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestWorkspaceStatus_APIError(t *testing.T) {
	h, mockClient := newTestHandlers()
	ctx := context.Background()

	mockClient.On("GetWorkspaceStatus", ctx, "ws-123").
		Return(nil, assert.AnError)

	result, err := h.workspaceStatus(ctx, makeReq("workspace_status", map[string]any{"workspace_id": "ws-123"}))

	require.NoError(t, err)
	assert.True(t, result.IsError)
	mockClient.AssertExpectations(t)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestIntegration_FullWorkflow tests the complete MCP flow:
// workspace_create → workspace_activate → session_create → session_message → session_history → workspace_stop
func TestIntegration_FullWorkflow(t *testing.T) {
	mockClient := &MockAPIClient{}
	srv := NewServer(mockClient, 30*time.Second)

	// Set up mock expectations for the full workflow
	mockClient.On("CreateWorkspace", mock.Anything, CreateWorkspaceReq{Runtime: "python:3.10", Name: "test"}).
		Return(&WorkspaceResp{ID: "ws-1", Name: "test", Runtime: "python:3.10", Phase: "Active"}, nil)
	mockClient.On("ActivateWorkspace", mock.Anything, "ws-1").
		Return(&ActivateResp{Resumed: "ws-1"}, nil)
	mockClient.On("CreateSession", mock.Anything, "ws-1").
		Return(&SessionResp{ID: "sess-1"}, nil)
	mockClient.On("SendMessage", mock.Anything, "ws-1", "sess-1", "write hello world in python", 30*time.Second).
		Return("```python\nprint('hello world')\n```", nil)
	mockClient.On("GetHistory", mock.Anything, "ws-1", "sess-1").
		Return([]Message{
			{Role: "user", Content: "write hello world in python"},
			{Role: "assistant", Content: "```python\nprint('hello world')\n```"},
		}, nil)
	mockClient.On("SuspendWorkspace", mock.Anything, "ws-1").Return(nil)

	// Create in-process MCP client
	mcpClient, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	defer mcpClient.Close()

	ctx := context.Background()
	require.NoError(t, mcpClient.Start(ctx))

	// Initialize
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1.0"}
	_, err = mcpClient.Initialize(ctx, initReq)
	require.NoError(t, err)

	// 1. List tools - verify all are registered (12 + 1 run_resolve + 11 Epic 64 = 24).
	toolsResp, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Len(t, toolsResp.Tools, 24)

	toolNames := make(map[string]bool)
	toolSchemas := make(map[string]mcp.Tool)
	for _, tool := range toolsResp.Tools {
		toolNames[tool.Name] = true
		toolSchemas[tool.Name] = tool
	}

	// #880/#905 schema pin: workspace_create must REQUIRE name — the API
	// validation (Epic 46) rejects empty names with a guaranteed 422, so
	// an optional-name schema advises clients into an error. Catches a
	// silent revert to optional.
	var requiredNames []string
	for _, r := range toolSchemas["workspace_create"].InputSchema.Required {
		requiredNames = append(requiredNames, r)
	}
	require.Contains(t, requiredNames, "name",
		"workspace_create schema must require name (API validation 422s on empty)")
	assert.True(t, toolNames["workspace_create"])
	assert.True(t, toolNames["workspace_activate"])
	assert.True(t, toolNames["workspace_stop"])
	assert.True(t, toolNames["workspace_refresh_compute"])
	assert.True(t, toolNames["session_create"])
	assert.True(t, toolNames["session_message"])
	assert.True(t, toolNames["session_history"])
	assert.True(t, toolNames["run_resolve"])

	// 2. workspace_create
	result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Request: mcp.Request{},
		Params: mcp.CallToolParams{
			Name:      "workspace_create",
			Arguments: map[string]any{"runtime": "python:3.10", "name": "test"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "ws-1")

	// 3. workspace_activate
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "workspace_activate",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// 4. session_create
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_create",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "sess-1")

	// 5. session_message
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_message",
			Arguments: map[string]any{"workspace_id": "ws-1", "session_id": "sess-1", "message": "write hello world in python"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "print('hello world')")

	// 6. session_history
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_history",
			Arguments: map[string]any{"workspace_id": "ws-1", "session_id": "sess-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "hello world")

	// 7. workspace_stop
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "workspace_stop",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	mockClient.AssertExpectations(t)
}

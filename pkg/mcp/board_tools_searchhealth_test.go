// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// board_tools_searchhealth_test.go — red-first pins for the #1699 seam
// gap closings on the external MCP projection: board_read's search and
// epic-listing modes (the tool's own description delegates them to the
// #1699 seams) and the board_health tool (§4.8's health panel feed).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestBoardTools_SearchHealthRegistered: the external surface carries
// board_health; board_read's schema grows the q and kind parameters.
func TestBoardTools_SearchHealthRegistered(t *testing.T) {
	srv := NewServer(&MockAPIClient{}, 30*time.Second)
	tools := srv.ListTools()
	_, hasHealth := tools["board_health"]
	assert.True(t, hasHealth, "the health projection is an external read tool")

	read, ok := tools["board_read"]
	require.True(t, ok, "board_read registered")
	props, err := json.Marshal(read.Tool.InputSchema.Properties)
	require.NoError(t, err)
	assert.Contains(t, string(props), `"q"`, "board_read grows the search parameter")
	assert.Contains(t, string(props), `"kind"`, "board_read grows the epic-listing kind")
}

// TestBoardTools_BoardReadSearch: board_read + q routes to the search
// projection with the caller's filters; the REST rendering passes
// through verbatim.
func TestBoardTools_BoardReadSearch(t *testing.T) {
	m := &MockAPIClient{}
	m.On("BoardSearch", mock.Anything, BoardSearchQuery{
		Q: "login", Project: "prj-aaaa1111", Epic: "epc-bbbb2222", Stage: "backlog", Tags: []string{"auth"},
	}).Return(`{"results":[{"short_id":"tsk-cccc3333"}],"suggestions":[]}`, nil).Once()
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_read", map[string]any{
		"q": "login", "project_id": "prj-aaaa1111", "epic_id": "epc-bbbb2222",
		"stage": "backlog", "tags": []string{"auth"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, boardResultText(res))
	assert.Contains(t, boardResultText(res), "tsk-cccc3333")
	m.AssertExpectations(t)
}

// TestBoardTools_BoardReadEpics: board_read kind=epics routes to the
// epic listing for the project.
func TestBoardTools_BoardReadEpics(t *testing.T) {
	m := &MockAPIClient{}
	m.On("BoardListEpics", mock.Anything, "prj-aaaa1111").
		Return(`{"epics":[{"short_id":"epc-bbbb2222","progress_derived":true}]}`, nil).Once()
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_read", map[string]any{
		"project_id": "prj-aaaa1111", "kind": "epics",
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, boardResultText(res))
	assert.Contains(t, boardResultText(res), "epc-bbbb2222")
	m.AssertExpectations(t)
}

// TestBoardTools_Health: board_health carries no arguments and renders
// the REST health snapshot verbatim; errors pass through.
func TestBoardTools_Health(t *testing.T) {
	m := &MockAPIClient{}
	m.On("BoardHealth", mock.Anything).
		Return(`{"active_leases":[],"pending_input_by_origin":{}}`, nil).Once()
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_health", map[string]any{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, boardResultText(res))
	assert.Contains(t, boardResultText(res), "active_leases")
	m.AssertExpectations(t)
}

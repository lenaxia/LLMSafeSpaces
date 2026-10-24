// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// board_tools_bulk_test.go — #1836's external bulk pair, red-first:
// board_start_tasks (bulk claim) and board_move_stage_bulk (bulk
// complete) ride the REST bulk routes through the client, validate
// adapter-owned arguments BEFORE any wire call (required task_ids,
// non-empty, the five-stage vocabulary, the batch cap front-run — the
// #1904 discipline of refusing a doomed round-trip at the tier that
// can), and render the engine's per-item result array verbatim. No
// claim-token parameter exists on either schema (§4.3b).

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestBoardTools_BulkAllowArms(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		mock func(m *MockAPIClient)
	}{
		{
			name: "board_start_tasks", tool: "board_start_tasks",
			args: map[string]any{"task_ids": []any{"tsk-aaaa1111", "tsk-bbbb2222"}, "request_id": "br-1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardStartTasks", mock.Anything, []string{"tsk-aaaa1111", "tsk-bbbb2222"}, "br-1").
					Return(`{"results":[{"id":"tsk-aaaa1111","status":"done"}],"count":1}`, nil)
			},
		},
		{
			name: "board_move_stage_bulk", tool: "board_move_stage_bulk",
			args: map[string]any{"task_ids": []any{"tsk-aaaa1111"}, "to_stage": "done", "evidence_url": "https://example/p", "request_id": "br-2"},
			mock: func(m *MockAPIClient) {
				m.On("BoardMoveStageBulk", mock.Anything, []string{"tsk-aaaa1111"}, "done", "https://example/p", "br-2").
					Return(`{"results":[{"id":"tsk-aaaa1111","status":"done"}],"count":1}`, nil)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{}
			tc.mock(m)
			h := &handlers{client: m, timeout: 0}
			res, err := callBoardTool(t, h, tc.tool, tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.False(t, res.IsError, boardResultText(res))
			m.AssertExpectations(t)
		})
	}
}

func TestBoardTools_BulkRequiredArgs(t *testing.T) {
	var many []any
	for i := 0; i < board.BulkMaxItems+1; i++ {
		many = append(many, fmt.Sprintf("tsk-filler-%02d", i))
	}
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"start_no_ids", "board_start_tasks", map[string]any{}},
		{"start_empty_ids", "board_start_tasks", map[string]any{"task_ids": []any{}}},
		{"start_over_cap", "board_start_tasks", map[string]any{"task_ids": many}},
		{"move_no_ids", "board_move_stage_bulk", map[string]any{"to_stage": "done"}},
		{"move_empty_ids", "board_move_stage_bulk", map[string]any{"task_ids": []any{}, "to_stage": "done"}},
		{"move_over_cap", "board_move_stage_bulk", map[string]any{"task_ids": many, "to_stage": "done"}},
		{"move_no_stage", "board_move_stage_bulk", map[string]any{"task_ids": []any{"tsk-aaaa1111"}}},
		{"move_bad_stage", "board_move_stage_bulk", map[string]any{"task_ids": []any{"tsk-aaaa1111"}, "to_stage": "cancel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{} // no expectations: any wire call is a failure
			h := &handlers{client: m, timeout: 0}
			res, err := callBoardTool(t, h, tc.tool, tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.True(t, res.IsError, "%s: the doomed call refuses at the tool tier", tc.name)
			m.AssertNotCalled(t, "BoardStartTasks", mock.Anything, mock.Anything, mock.Anything)
			m.AssertNotCalled(t, "BoardMoveStageBulk", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// callBoardTool's bulk dispatch arms live in the shared switch
// (board_tools_test.go); the Registration pin there carries the set.

// #1836 sibling: the bulk delete tool. The allow arm rides the REST
// bulk-delete route with the ONE shared reconnect flag; the refusals
// (missing/empty/over-cap/malformed entries) front-run any wire call.
func TestBoardTools_BulkDeleteAllowArm(t *testing.T) {
	m := &MockAPIClient{}
	m.On("BoardDeleteTasksBulk", mock.Anything, []string{"tsk-aaaa1111", "tsk-bbbb2222"}, true, "br-del-1").
		Return(`{"results":[{"id":"tsk-aaaa1111","status":"done","orphaned_dependents":["tsk-orph"]}],"count":1}`, nil)
	h := &handlers{client: m, timeout: 0}
	res, err := callBoardTool(t, h, "board_delete_tasks",
		map[string]any{"task_ids": []any{"tsk-aaaa1111", "tsk-bbbb2222"}, "reconnect": true, "request_id": "br-del-1"})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, boardResultText(res))
	m.AssertExpectations(t)
}

func TestBoardTools_BulkDeleteRequiredArgs(t *testing.T) {
	var many []any
	for i := 0; i < board.BulkMaxItems+1; i++ {
		many = append(many, fmt.Sprintf("tsk-filler-%02d", i))
	}
	cases := []struct {
		name string
		args map[string]any
	}{
		{"delete_no_ids", map[string]any{}},
		{"delete_empty_ids", map[string]any{"task_ids": []any{}}},
		{"delete_over_cap", map[string]any{"task_ids": many}},
		{"delete_bad_entry", map[string]any{"task_ids": []any{"tsk-aaaa1111", 7}}},
		{"delete_empty_entry", map[string]any{"task_ids": []any{"tsk-aaaa1111", ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{} // no expectations: any wire call is a failure
			h := &handlers{client: m, timeout: 0}
			res, err := callBoardTool(t, h, "board_delete_tasks", tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.True(t, res.IsError, "%s: the doomed call refuses at the tool tier", tc.name)
			m.AssertNotCalled(t, "BoardDeleteTasksBulk", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

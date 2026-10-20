// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// board_tools_test.go — the external board.* tool pins (design 0067
// §4.3b, issue #1700). Red-first: landed before the handler
// implementation (the stub handlers fail every allow arm).
//
// Pin families:
//   - REGISTRATION: the exact tool set (18 tools), the declared
//     asymmetries (no heartbeat/throttle/claim tool), and the claim-token
//     refusal (design §4.3b: external actors hold NO claim tokens — no
//     board_* tool schema carries one, and no tool exposes
//     user/org-scope params: R7 user-level, the API key IS the owner).
//   - ALLOW ARMS: each tool's happy path against the mocked client,
//     asserting the exact client call + arguments (route parity with the
//     REST column, whose handlers the client hits).
//   - GUARD PASS-THROUGH: the API's guard rendering (the same REST
//     handlers) surfaces verbatim in the tool error — status class,
//     engine code, suggestions — never re-interpreted.
//   - NON-DISCLOSURE: a foreign row's plain 404 body passes through
//     exactly; the tool adds nothing (no owner identity, no suggestions).
//   - KIND ROUTING: the unified tools (board_get/board_update_item/
//     board_delete) dispatch on the prj-/epc-/tsk- prefix; a bare UUID
//     is refused with the prefix rule; board_update_item rejects fields
//     foreign to the addressed kind (no silent drops).
//   - OR4 ANCHOR: tags ride verbatim — a well-formed pr:N anchor and a
//     MALFORMED one (pr:0, pr:abc) both pass; the tool never parses,
//     validates, or invents anchors (the engine's extraction is
//     conservative; a projection owning validation would violate §4.2).
//   - REQUEST-ID THREADING: the clientMessageID discipline — one
//     request_id per logical operation rides to the REST tier.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// boardToolNames is the exact external surface (design §4.3b): 19 tools.
var boardToolNames = []string{
	"board_list_projects", "board_get", "board_read", "board_health",
	"board_create_project", "board_create_epic", "board_create_task",
	"board_update_item", "board_move_stage", "board_start_task",
	"board_epic_activate", "board_epic_pause", "board_epic_archive", "board_epic_unarchive",
	"board_project_archive", "board_project_unarchive",
	"board_project_pause", "board_project_resume",
	"board_pause_task", "board_resume_task",
	"board_delete", "board_import_plan",
	"board_add_dependency", "board_remove_dependency",
}

// TestBoardTools_Registration: NewServer registers exactly the design's
// external tool set — and NOTHING from the worker column: no
// board_heartbeat, no board_report_throttle, no claim tool of any name
// (claim_next is engine-internal, R9; worker actions live on the agent
// registry only, §4.3c).
func TestBoardTools_Registration(t *testing.T) {
	srv := NewServer(&MockAPIClient{}, 30*time.Second)
	registered := map[string]bool{}
	for name := range srv.ListTools() {
		registered[name] = true
	}
	for _, name := range boardToolNames {
		assert.True(t, registered[name], "tool %s must be registered", name)
	}
	assert.NotContains(t, registered, "board_heartbeat", "worker-only action (§4.3c) — external surface refuses it")
	assert.NotContains(t, registered, "board_report_throttle", "worker-only action (§4.3c) — external surface refuses it")
	for name := range registered {
		if len(name) > 6 && name[:6] == "board_" {
			assert.NotContains(t, name, "claim", "claim_next is engine-internal (R9) — no board_* claim tool exists")
		}
	}
}

// TestBoardTools_NoClaimTokenParams: the design withholds claim tokens
// from external actors (§4.3b — "no claim/heartbeat here"; §4.7: "no
// owner surface delivers these"). No board_* tool schema may carry a
// claim-token parameter, and none may carry user/org scoping params
// (R7: the API key IS the owner; no scope switcher exists).
func TestBoardTools_NoClaimTokenParams(t *testing.T) {
	forbidden := []string{"claim_token", "claimToken", "claim_id", "user_id", "userId", "org_id", "owner_id", "scope"}
	srv := NewServer(&MockAPIClient{}, 30*time.Second)
	for name, tool := range srv.ListTools() {
		if len(name) < 6 || name[:6] != "board_" {
			continue
		}
		b, err := json.Marshal(tool.Tool.InputSchema.Properties)
		require.NoError(t, err)
		for _, f := range forbidden {
			assert.NotContains(t, string(b), f, "tool %s schema must not carry %q (external actors hold no claim tokens; R7 user-level)", name, f)
		}
	}
}

// boardToolCase is one allow arm: name is the subtest label, tool the
// dispatch name.
type boardToolCase struct {
	name string
	tool string
	args map[string]any
	mock func(m *MockAPIClient)
}

// boardAllowCases: one allow arm per tool — the exact client call with
// the exact arguments (route parity: these client methods hit the REST
// routes the parity matrix's REST column pins).
func boardAllowCases() []boardToolCase {
	return []boardToolCase{
		{
			name: "board_list_projects", tool: "board_list_projects",
			args: map[string]any{},
			mock: func(m *MockAPIClient) {
				m.On("BoardListProjects", mock.Anything).Return(`{"projects":[]}`, nil)
			},
		},
		{
			name: "board_get_project", tool: "board_get", args: map[string]any{"item_id": "prj-aaaa1111"},
			mock: func(m *MockAPIClient) {
				m.On("BoardGetProject", mock.Anything, "prj-aaaa1111").Return(`{"short_id":"prj-aaaa1111"}`, nil)
			},
		},
		{
			name: "board_get_epic", tool: "board_get", args: map[string]any{"item_id": "epc-bbbb2222"},
			mock: func(m *MockAPIClient) {
				m.On("BoardGetEpic", mock.Anything, "epc-bbbb2222").Return(`{"short_id":"epc-bbbb2222"}`, nil)
			},
		},
		{
			name: "board_get_task", tool: "board_get", args: map[string]any{"item_id": "tsk-cccc3333"},
			mock: func(m *MockAPIClient) {
				m.On("BoardGetTask", mock.Anything, "tsk-cccc3333").Return(`{"short_id":"tsk-cccc3333"}`, nil)
			},
		},
		{
			name: "board_read", tool: "board_read", args: map[string]any{"project_id": "prj-aaaa1111", "stage": "backlog"},
			mock: func(m *MockAPIClient) {
				m.On("BoardListTasks", mock.Anything, "prj-aaaa1111", "backlog").Return(`{"tasks":[]}`, nil)
			},
		},
		{
			name: "board_create_project", tool: "board_create_project",
			args: map[string]any{"name": "Board", "target_workspace_id": "ws-1", "worker_slots": 3,
				"tags": []any{"alpha"}, "request_id": "req-p1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardCreateProject", mock.Anything, BoardProjectBody{
					Name: "Board", TargetWorkspaceID: "ws-1", WorkerSlots: 3, Tags: []string{"alpha"}, RequestID: "req-p1",
				}).Return(`{"short_id":"prj-aaaa1111"}`, nil)
			},
		},
		{
			name: "board_create_epic", tool: "board_create_epic",
			args: map[string]any{"project_id": "prj-aaaa1111", "title": "E1", "outcome": "ship",
				"tags": []any{"beta"}, "request_id": "req-e1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardCreateEpic", mock.Anything, "prj-aaaa1111", BoardEpicBody{
					Title: "E1", Outcome: "ship", Tags: []string{"beta"}, RequestID: "req-e1",
				}).Return(`{"short_id":"epc-bbbb2222"}`, nil)
			},
		},
		{
			name: "board_create_task", tool: "board_create_task",
			args: map[string]any{"epic_id": "epc-bbbb2222", "title": "T1", "description": "do it",
				"tags": []any{"pr:123"}, "request_id": "req-t1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardCreateTask", mock.Anything, "epc-bbbb2222", BoardTaskBody{
					Title: "T1", Description: "do it", Tags: strPtr([]string{"pr:123"}), RequestID: "req-t1",
				}).Return(`{"short_id":"tsk-cccc3333"}`, nil)
			},
		},
		{
			name: "board_update_project", tool: "board_update_item",
			args: map[string]any{"item_id": "prj-aaaa1111", "name": "Renamed", "worker_slots": 4},
			mock: func(m *MockAPIClient) {
				m.On("BoardUpdateProject", mock.Anything, "prj-aaaa1111", BoardProjectBody{
					Name: "Renamed", WorkerSlots: 4,
				}).Return(`{"short_id":"prj-aaaa1111"}`, nil)
			},
		},
		{
			name: "board_update_epic", tool: "board_update_item",
			args: map[string]any{"item_id": "epc-bbbb2222", "outcome": "steer"},
			mock: func(m *MockAPIClient) {
				m.On("BoardUpdateEpic", mock.Anything, "epc-bbbb2222", BoardEpicBody{Outcome: "steer"}).
					Return(`{"short_id":"epc-bbbb2222"}`, nil)
			},
		},
		{
			name: "board_update_task", tool: "board_update_item",
			args: map[string]any{"item_id": "tsk-cccc3333", "set_criteria_met": 0},
			mock: func(m *MockAPIClient) {
				zero := 0
				m.On("BoardUpdateTask", mock.Anything, "tsk-cccc3333", BoardTaskBody{SetCriteriaMet: &zero}).
					Return(`{"short_id":"tsk-cccc3333"}`, nil)
			},
		},
		{
			name: "board_move_stage", tool: "board_move_stage",
			args: map[string]any{"task_id": "tsk-cccc3333", "to_stage": "done", "evidence_url": "https://pr/123", "request_id": "req-m1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardMoveStage", mock.Anything, "tsk-cccc3333", "done", "https://pr/123", "req-m1").
					Return(`{"short_id":"tsk-cccc3333","stage":"done"}`, nil)
			},
		},
		{
			name: "board_start_task", tool: "board_start_task",
			args: map[string]any{"task_id": "tsk-cccc3333", "request_id": "req-s1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardStartTask", mock.Anything, "tsk-cccc3333", "req-s1").
					Return(`{"short_id":"tsk-cccc3333","escalated":false}`, nil)
			},
		},
		{
			name: "board_epic_activate", tool: "board_epic_activate", args: map[string]any{"epic_id": "epc-bbbb2222"},
			mock: func(m *MockAPIClient) {
				m.On("BoardEpicActivate", mock.Anything, "epc-bbbb2222", mock.Anything, mock.Anything).Return(`{"status":"active"}`, nil)
			},
		},
		{
			name: "board_epic_pause", tool: "board_epic_pause", args: map[string]any{"epic_id": "epc-bbbb2222"},
			mock: func(m *MockAPIClient) {
				m.On("BoardEpicPause", mock.Anything, "epc-bbbb2222", mock.Anything).Return(`{"status":"paused"}`, nil)
			},
		},
		{
			name: "board_epic_archive", tool: "board_epic_archive", args: map[string]any{"epic_id": "epc-bbbb2222"},
			mock: func(m *MockAPIClient) {
				m.On("BoardEpicArchive", mock.Anything, "epc-bbbb2222", mock.Anything).Return(`{"status":"archived"}`, nil)
			},
		},
		{
			name: "board_epic_unarchive", tool: "board_epic_unarchive", args: map[string]any{"epic_id": "epc-bbbb2222"},
			mock: func(m *MockAPIClient) {
				m.On("BoardEpicUnarchive", mock.Anything, "epc-bbbb2222", mock.Anything).Return(`{"status":"active"}`, nil)
			},
		},
		{
			name: "board_project_archive", tool: "board_project_archive", args: map[string]any{"project_id": "prj-aaaa1111"},
			mock: func(m *MockAPIClient) {
				m.On("BoardArchiveProject", mock.Anything, "prj-aaaa1111", mock.Anything).Return(`{"status":"archived"}`, nil)
			},
		},
		{
			name: "board_project_unarchive", tool: "board_project_unarchive", args: map[string]any{"project_id": "prj-aaaa1111"},
			mock: func(m *MockAPIClient) {
				m.On("BoardUnarchiveProject", mock.Anything, "prj-aaaa1111", mock.Anything).Return(`{"status":"active"}`, nil)
			},
		},
		{
			name: "board_delete_task", tool: "board_delete",
			args: map[string]any{"item_id": "tsk-cccc3333", "reconnect": true},
			mock: func(m *MockAPIClient) {
				m.On("BoardDeleteTask", mock.Anything, "tsk-cccc3333", true).
					Return(`{"item_kind":"task","short_id":"tsk-cccc3333","reconnected":["tsk-eeee5555→tsk-aaaa1111"]}`, nil)
			},
		},
		{
			name: "board_delete_epic", tool: "board_delete",
			args: map[string]any{"item_id": "epc-bbbb2222", "force": true},
			mock: func(m *MockAPIClient) {
				m.On("BoardDeleteEpic", mock.Anything, "epc-bbbb2222", true).
					Return(`{"item_kind":"epic","short_id":"epc-bbbb2222"}`, nil)
			},
		},
		{
			name: "board_pause_task", tool: "board_pause_task",
			args: map[string]any{"task_id": "tsk-cccc3333", "request_id": "req-p1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardPauseTask", mock.Anything, "tsk-cccc3333", "req-p1").
					Return(`{"paused":true}`, nil)
			},
		},
		{
			name: "board_resume_task", tool: "board_resume_task",
			args: map[string]any{"task_id": "tsk-cccc3333"},
			mock: func(m *MockAPIClient) {
				m.On("BoardResumeTask", mock.Anything, "tsk-cccc3333", "").
					Return(`{"paused":false}`, nil)
			},
		},
		{
			name: "board_project_pause", tool: "board_project_pause",
			args: map[string]any{"project_id": "prj-aaaa1111"},
			mock: func(m *MockAPIClient) {
				m.On("BoardProjectPause", mock.Anything, "prj-aaaa1111", mock.Anything).Return(`{"paused":true}`, nil)
			},
		},
		{
			name: "board_project_resume", tool: "board_project_resume",
			args: map[string]any{"project_id": "prj-aaaa1111"},
			mock: func(m *MockAPIClient) {
				m.On("BoardProjectResume", mock.Anything, "prj-aaaa1111", mock.Anything).Return(`{"paused":false}`, nil)
			},
		},
		{
			name: "board_import_plan", tool: "board_import_plan",
			args: map[string]any{"project_id": "prj-aaaa1111", "epic_title": "E", "dry_run": true,
				"tasks": []any{map[string]any{"key": "a", "title": "A"}}},
			mock: func(m *MockAPIClient) {
				m.On("BoardImportPlan", mock.Anything, "prj-aaaa1111", BoardPlanBody{
					EpicTitle: "E", DryRun: true,
					Tasks: []BoardPlanTaskBody{{Key: "a", Title: "A"}},
				}).Return(`{"dry_run":true}`, nil)
			},
		},
		{
			name: "board_add_dependency", tool: "board_add_dependency",
			args: map[string]any{"task_id": "tsk-cccc3333", "depends_on": "tsk-dddd4444", "request_id": "req-d1"},
			mock: func(m *MockAPIClient) {
				m.On("BoardAddDependency", mock.Anything, "tsk-cccc3333", "tsk-dddd4444", "req-d1").
					Return(`{"task":{}}`, nil)
			},
		},
		{
			name: "board_remove_dependency", tool: "board_remove_dependency",
			args: map[string]any{"task_id": "tsk-cccc3333", "depends_on": "tsk-dddd4444"},
			mock: func(m *MockAPIClient) {
				m.On("BoardRemoveDependency", mock.Anything, "tsk-cccc3333", "tsk-dddd4444", "").Return(nil)
			},
		},
	}
}

// TestBoardTools_AllowArms: every tool's happy path — the exact client
// call asserted by the mock (wrong route, wrong args, or a dropped
// field reddens here).
func TestBoardTools_AllowArms(t *testing.T) {
	for _, tc := range boardAllowCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{}
			tc.mock(m)
			h := &handlers{client: m, timeout: 30 * time.Second}
			res, err := callBoardTool(t, h, tc.tool, tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.False(t, res.IsError, "allow arm %s: %s", tc.name, boardResultText(res))
			m.AssertExpectations(t)
		})
	}
}

// TestBoardTools_RequiredArgs: the adapter-owned validation — required
// ids/names absent answer a tool error BEFORE any client call.
func TestBoardTools_RequiredArgs(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"board_get_no_id", "board_get", map[string]any{}},
		{"board_get_bare_uuid", "board_get", map[string]any{"item_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6"}},
		{"board_read_no_project", "board_read", map[string]any{}},
		{"board_create_project_no_name", "board_create_project", map[string]any{"target_workspace_id": "ws-1"}},
		{"board_create_project_no_workspace", "board_create_project", map[string]any{"name": "B"}},
		{"board_create_epic_no_title", "board_create_epic", map[string]any{"project_id": "prj-aaaa1111"}},
		{"board_create_task_no_title", "board_create_task", map[string]any{"epic_id": "epc-bbbb2222"}},
		{"board_update_item_no_id", "board_update_item", map[string]any{"name": "x"}},
		{"board_update_item_bare_uuid", "board_update_item", map[string]any{"item_id": "3fa85f64"}},
		{"board_move_stage_no_stage", "board_move_stage", map[string]any{"task_id": "tsk-cccc3333"}},
		{"board_delete_no_id", "board_delete", map[string]any{}},
		{"board_add_dependency_no_depends", "board_add_dependency", map[string]any{"task_id": "tsk-cccc3333"}},
		{"board_remove_dependency_no_depends", "board_remove_dependency", map[string]any{"task_id": "tsk-cccc3333"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{} // no expectations: any call is a failure
			h := &handlers{client: m, timeout: 30 * time.Second}
			res, err := callBoardTool(t, h, tc.tool, tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.True(t, res.IsError, "%s: absent required argument must answer a tool error", tc.name)
			m.AssertNotCalled(t, "BoardListProjects", mock.Anything)
		})
	}
}

// TestBoardTools_GuardPassThrough: the API's rendering surfaces verbatim
// — the guard's HTTP class (409/403/404) and the engine's code and
// suggestions ride inside the tool error, never re-interpreted (the
// clientMessageID of guard parity: same handlers, same outcomes).
func TestBoardTools_GuardPassThrough(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		mockFn   func(m *MockAPIClient)
		contains []string
	}{
		{
			name: "done_guard_409",
			tool: "board_move_stage",
			args: map[string]any{"task_id": "tsk-cccc3333", "to_stage": "done"},
			mockFn: func(m *MockAPIClient) {
				m.On("BoardMoveStage", mock.Anything, "tsk-cccc3333", "done", "", "").
					Return("", fmt__error("API error 409: {\"error\":\"board: guard done_guard: criteria unmet\",\"code\":\"done_guard\"}"))
			},
			contains: []string{"409", "done_guard"},
		},
		{
			name: "claim_token_guard_409",
			tool: "board_move_stage",
			args: map[string]any{"task_id": "tsk-cccc3333", "to_stage": "in_review"},
			mockFn: func(m *MockAPIClient) {
				m.On("BoardMoveStage", mock.Anything, "tsk-cccc3333", "in_review", "", "").
					Return("", fmt__error("API error 409: {\"error\":\"board: guard claim_token: stage writes on a claimed task require its live claim token\",\"code\":\"claim_token\"}"))
			},
			contains: []string{"409", "claim_token"},
		},
		{
			name: "authz_403",
			tool: "board_epic_activate",
			args: map[string]any{"epic_id": "epc-bbbb2222"},
			mockFn: func(m *MockAPIClient) {
				m.On("BoardEpicActivate", mock.Anything, "epc-bbbb2222", mock.Anything, mock.Anything).
					Return("", fmt__error("API error 403: {\"error\":\"board: guard authz: board actions belong to the owning user (ruling R7)\",\"code\":\"authz\"}"))
			},
			contains: []string{"403", "authz"},
		},
		{
			name: "cycle_409_with_suggestions",
			tool: "board_add_dependency",
			args: map[string]any{"task_id": "tsk-cccc3333", "depends_on": "tsk-dddd4444"},
			mockFn: func(m *MockAPIClient) {
				m.On("BoardAddDependency", mock.Anything, "tsk-cccc3333", "tsk-dddd4444", "").
					Return("", fmt__error("API error 409: {\"error\":\"board: dependency cycle: would create a cycle\",\"code\":\"cycle\",\"suggestions\":[\"tsk-eeee5555\"]}"))
			},
			contains: []string{"409", "cycle", "tsk-eeee5555"},
		},
		{
			name: "notfound_404_with_suggestions",
			tool: "board_get",
			args: map[string]any{"item_id": "tsk-nope0000"},
			mockFn: func(m *MockAPIClient) {
				m.On("BoardGetTask", mock.Anything, "tsk-nope0000").
					Return("", fmt__error("API error 404: {\"error\":\"board: task \\\"tsk-nope0000\\\" not found\",\"suggestions\":[\"tsk-cccc3333\"]}"))
			},
			contains: []string{"404", "tsk-cccc3333"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{}
			tc.mockFn(m)
			h := &handlers{client: m, timeout: 30 * time.Second}
			res, err := callBoardTool(t, h, tc.tool, tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.True(t, res.IsError, "%s: a guard denial is a tool error result", tc.name)
			text := boardResultText(res)
			for _, want := range tc.contains {
				assert.Contains(t, text, want, "%s", tc.name)
			}
			m.AssertExpectations(t)
		})
	}
}

// TestBoardTools_ForeignRowNotDisclosed: the read perimeter's plain 404
// for a foreign row passes through EXACTLY — the tool error carries the
// API's body (no owner identity, no suggestions) and is byte-identical
// to what a missing row renders at this tier. The perimeter itself is
// pinned live by the parity harness (real REST router); this pin holds
// the projection's fidelity: add nothing, interpret nothing.
func TestBoardTools_ForeignRowNotDisclosed(t *testing.T) {
	foreign := `{"error":"board: project not found"}`
	m := &MockAPIClient{}
	m.On("BoardGetProject", mock.Anything, "prj-aaaa1111").Return("", fmt__error("API error 404: "+foreign))
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_get", map[string]any{"item_id": "prj-aaaa1111"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := boardResultText(res)
	assert.Contains(t, text, "API error 404")
	assert.Contains(t, text, foreign, "the API's plain-404 body rides verbatim")
	assert.NotContains(t, text, "suggestions", "foreign rows never carry did-you-mean suggestions (0066 §4.7 scoping)")
	assert.NotContains(t, text, "someone-else", "the foreign owner's identity is never disclosed")
	m.AssertExpectations(t)
}

// TestBoardTools_UpdateItemKindFields: board_update_item accepts only
// the fields applying to the addressed kind — a field for another kind
// is an explicit error, never a silent drop (the caller believes the
// write happened otherwise).
func TestBoardTools_UpdateItemKindFields(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{"project_name_ok", map[string]any{"item_id": "prj-aaaa1111", "name": "N"}, false},
		{"project_outcome_rejected", map[string]any{"item_id": "prj-aaaa1111", "outcome": "x"}, true},
		{"project_description_rejected", map[string]any{"item_id": "prj-aaaa1111", "description": "x"}, true},
		{"epic_outcome_ok", map[string]any{"item_id": "epc-bbbb2222", "outcome": "o"}, false},
		{"epic_name_rejected", map[string]any{"item_id": "epc-bbbb2222", "name": "n"}, true},
		{"epic_set_criteria_rejected", map[string]any{"item_id": "epc-bbbb2222", "set_criteria_met": 0}, true},
		{"task_description_ok", map[string]any{"item_id": "tsk-cccc3333", "description": "d"}, false},
		{"task_outcome_rejected", map[string]any{"item_id": "tsk-cccc3333", "outcome": "o"}, true},
		{"task_worker_slots_rejected", map[string]any{"item_id": "tsk-cccc3333", "worker_slots": 5}, true},
		{"tags_ok_all_kinds", map[string]any{"item_id": "prj-aaaa1111", "tags": []any{"t"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &MockAPIClient{}
			m.On("BoardUpdateProject", mock.Anything, "prj-aaaa1111", mock.Anything).
				Return(`{"short_id":"prj-aaaa1111"}`, nil)
			m.On("BoardUpdateEpic", mock.Anything, "epc-bbbb2222", mock.Anything).
				Return(`{"short_id":"epc-bbbb2222"}`, nil)
			m.On("BoardUpdateTask", mock.Anything, "tsk-cccc3333", mock.Anything).
				Return(`{"short_id":"tsk-cccc3333"}`, nil)
			h := &handlers{client: m, timeout: 30 * time.Second}
			res, err := callBoardTool(t, h, "board_update_item", tc.args)
			require.NoError(t, err)
			require.NotNil(t, res)
			assert.Equal(t, tc.wantErr, res.IsError, "%s: %s", tc.name, boardResultText(res))
		})
	}
}

// TestBoardTools_OR4AnchorPassthrough: tags ride verbatim — a well-formed
// pr:N anchor AND malformed ones (pr:0, pr:abc, pr:) all pass through
// unmodified; the tool neither parses nor validates anchors (the
// engine's extraction is conservative by design — a malformed anchor is
// an inert tag, and a projection adding validation the engine lacks
// would own business rules, violating §4.2).
func TestBoardTools_OR4AnchorPassthrough(t *testing.T) {
	for _, tags := range [][]string{
		{"pr:123", "work"},
		{"pr:0"},
		{"pr:abc"},
		{"pr:"},
		{"plain"},
	} {
		m := &MockAPIClient{}
		m.On("BoardCreateTask", mock.Anything, "epc-bbbb2222", BoardTaskBody{Title: "T", Tags: &tags}).
			Return(`{"short_id":"tsk-cccc3333","tags":[]}`, nil).Once()
		h := &handlers{client: m, timeout: 30 * time.Second}
		args := map[string]any{"epic_id": "epc-bbbb2222", "title": "T", "tags": toAnySlice(tags)}
		res, err := callBoardTool(t, h, "board_create_task", args)
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.False(t, res.IsError, "tags %v must ride verbatim (OR4: extraction is the engine's, conservative): %s",
			tags, boardResultText(res))
		m.AssertExpectations(t)
	}
}

// TestBoardTools_RequestIDThreads: the clientMessageID discipline — the
// request_id the caller supplies is the one delivered to the REST tier
// (one per logical operation, reused on retry; the replay outcome
// itself is rendered by the same REST handlers the harness pins).
func TestBoardTools_RequestIDThreads(t *testing.T) {
	const key = "11111111-2222-3333-4444-555555555555"
	m := &MockAPIClient{}
	m.On("BoardMoveStage", mock.Anything, "tsk-cccc3333", "in_review", "", key).
		Return(`{"short_id":"tsk-cccc3333"}`, nil).Once()
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_move_stage",
		map[string]any{"task_id": "tsk-cccc3333", "to_stage": "in_review", "request_id": key})
	require.NoError(t, err)
	assert.False(t, res.IsError, boardResultText(res))
	m.AssertExpectations(t)
}

// --- helpers ---

// callBoardTool dispatches to the (unexported) handler by tool name —
// the same entry AddBoardTools registers.
func callBoardTool(t *testing.T, h *handlers, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	switch name {
	case "board_list_projects":
		return h.boardListProjects(ctx, makeReq(name, args))
	case "board_get":
		return h.boardGet(ctx, makeReq(name, args))
	case "board_read":
		return h.boardRead(ctx, makeReq(name, args))
	case "board_health":
		return h.boardHealth(ctx, makeReq(name, args))
	case "board_create_project":
		return h.boardCreateProject(ctx, makeReq(name, args))
	case "board_create_epic":
		return h.boardCreateEpic(ctx, makeReq(name, args))
	case "board_create_task":
		return h.boardCreateTask(ctx, makeReq(name, args))
	case "board_update_item":
		return h.boardUpdateItem(ctx, makeReq(name, args))
	case "board_move_stage":
		return h.boardMoveStage(ctx, makeReq(name, args))
	case "board_start_task":
		return h.boardStartTask(ctx, makeReq(name, args))
	case "board_epic_activate":
		return h.boardEpicActivate(ctx, makeReq(name, args))
	case "board_epic_pause":
		return h.boardEpicPause(ctx, makeReq(name, args))
	case "board_epic_archive":
		return h.boardEpicArchive(ctx, makeReq(name, args))
	case "board_epic_unarchive":
		return h.boardEpicUnarchive(ctx, makeReq(name, args))
	case "board_project_archive":
		return h.boardProjectArchive(ctx, makeReq(name, args))
	case "board_project_unarchive":
		return h.boardProjectUnarchive(ctx, makeReq(name, args))
	case "board_delete":
		return h.boardDelete(ctx, makeReq(name, args))
	case "board_pause_task":
		return h.boardPauseTask(ctx, makeReq(name, args))
	case "board_resume_task":
		return h.boardResumeTask(ctx, makeReq(name, args))
	case "board_project_pause":
		return h.boardProjectPause(ctx, makeReq(name, args))
	case "board_project_resume":
		return h.boardProjectResume(ctx, makeReq(name, args))
	case "board_import_plan":
		return h.boardImportPlan(ctx, makeReq(name, args))
	case "board_add_dependency":
		return h.boardAddDependency(ctx, makeReq(name, args))
	case "board_remove_dependency":
		return h.boardRemoveDependency(ctx, makeReq(name, args))
	default:
		t.Fatalf("unknown board tool %q", name)
		return nil, nil
	}
}

func boardResultText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func strPtr(s []string) *[]string { return &s }

// fmt__error is a tiny alias so the table cases stay one-lined.
func fmt__error(s string) error { return &simpleError{s} }

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

// TestBoardTools_TaskTagsClear (r2): the tags-clear fidelity pin — an
// explicit empty tags array on a TASK update must reach the client as
// a PRESENT empty slice (wire "tags":[] → the engine clears), while an
// OMITTED tags key reaches it as nil (keep). This is the exact
// distinction the REST binding carries and the r1 projection lost.
func TestBoardTools_TaskTagsClear(t *testing.T) {
	empty := []string{}
	m := &MockAPIClient{}
	m.On("BoardUpdateTask", mock.Anything, "tsk-cccc3333", BoardTaskBody{Tags: &empty}).
		Return(`{"short_id":"tsk-cccc3333","tags":[]}`, nil).Once()
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, err := callBoardTool(t, h, "board_update_item",
		map[string]any{"item_id": "tsk-cccc3333", "tags": []any{}})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError, "explicit [] must ride to the wire: %s", boardResultText(res))
	m.AssertExpectations(t)

	m2 := &MockAPIClient{}
	m2.On("BoardUpdateTask", mock.Anything, "tsk-cccc3333", BoardTaskBody{Title: "x"}).
		Return(`{"short_id":"tsk-cccc3333"}`, nil).Once()
	h2 := &handlers{client: m2, timeout: 30 * time.Second}
	res2, err := callBoardTool(t, h2, "board_update_item",
		map[string]any{"item_id": "tsk-cccc3333", "title": "x"})
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.False(t, res2.IsError, boardResultText(res2))
	m2.AssertExpectations(t) // no Tags pointer: omission keeps
}

// TestBoardTools_TaskTagsClearWire: the wire carries the distinction —
// a present-empty tags pointer serializes "tags":[]; nil omits the key.
func TestBoardTools_TaskTagsClearWire(t *testing.T) {
	empty := []string{}
	b, err := json.Marshal(BoardTaskBody{Tags: &empty})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"tags":[]`)
	b, err = json.Marshal(BoardTaskBody{Title: "x"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"tags"`)
}

// TestBoardTools_AdapterStageValidation (r2, r5): the adapter-owned
// stage branches — board_read's filter takes the FIVE-stage vocabulary
// ("cancelled" is not a stage: cancel is eliminated, delete replaces
// it — the filter rejects it like any misspelling); board_move_stage's
// target takes the LEGAL manager move-target set.
func TestBoardTools_AdapterStageValidation(t *testing.T) {
	readOK := &MockAPIClient{}
	readOK.On("BoardListTasks", mock.Anything, "prj-aaaa1111", "backlog").Return(`{"tasks":[]}`, nil).Once()
	h := &handlers{client: readOK, timeout: 30 * time.Second}
	res, _ := callBoardTool(t, h, "board_read", map[string]any{"project_id": "prj-aaaa1111", "stage": "backlog"})
	assert.False(t, res.IsError)
	m := &MockAPIClient{} // no expectations: validation must precede any call
	h2 := &handlers{client: m, timeout: 30 * time.Second}
	res2, _ := callBoardTool(t, h2, "board_read", map[string]any{"project_id": "prj-aaaa1111", "stage": "wip"})
	assert.True(t, res2.IsError, "unknown read filter rejected at the tool tier")

	mv := &MockAPIClient{}
	mv.On("BoardMoveStage", mock.Anything, "tsk-cccc3333", "in_review", "", "").Return(`{}`, nil).Once()
	h3 := &handlers{client: mv, timeout: 30 * time.Second}
	res3, _ := callBoardTool(t, h3, "board_move_stage", map[string]any{"task_id": "tsk-cccc3333", "to_stage": "in_review"})
	assert.False(t, res3.IsError)
	m2 := &MockAPIClient{}
	h4 := &handlers{client: m2, timeout: 30 * time.Second}
	for _, bad := range []string{"cancelled", "not_a_stage"} {
		res4, _ := callBoardTool(t, h4, "board_move_stage", map[string]any{"task_id": "tsk-cccc3333", "to_stage": bad})
		assert.True(t, res4.IsError, "to_stage=%s must be rejected (cancelled is not a stage — delete replaced cancel)", bad)
	}
	// The read tier collapses with it: stage=cancelled is an invalid
	// FILTER too (the r5 concatenation-dodge fix — six became five).
	m5 := &MockAPIClient{} // no expectations: validation must precede any call
	h5 := &handlers{client: m5, timeout: 30 * time.Second}
	res5, _ := callBoardTool(t, h5, "board_read", map[string]any{"project_id": "prj-aaaa1111", "stage": "cancelled"})
	assert.True(t, res5.IsError, "stage=cancelled must be rejected at the tool tier (five-stage vocabulary)")
	assert.Contains(t, boardResultText(res5), "pending_input", "the error advertises the FIVE live stages")
	assert.NotContains(t, boardResultText(res5), "cancelled", "the error must not advertise the eliminated stage")
}

// TestBoardTools_DeleteProjectRefused (r2): board_delete on a prj- id
// explains the archive path — projects have no terminal-stage value (ruling
// R2's epic/project vocabulary).
func TestBoardTools_DeleteProjectRefused(t *testing.T) {
	m := &MockAPIClient{} // no expectations: no client call may fire
	h := &handlers{client: m, timeout: 30 * time.Second}
	res, _ := callBoardTool(t, h, "board_delete", map[string]any{"item_id": "prj-aaaa1111"})
	assert.True(t, res.IsError)
	text := boardResultText(res)
	assert.Contains(t, text, "archive", "the refusal points at the archive path")
}

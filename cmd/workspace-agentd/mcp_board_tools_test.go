// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// mcp_board_tools_test.go — #1701 + #1747 (design 0067 §4.3c as
// amended): the agent-internal board tools. The WORKER surface keeps
// its four claim-token tools (board_heartbeat, board_report_throttle,
// board_move_stage, board_read); #1747 adds the MANAGER set (project/
// epic/task CRUD incl. pause/resume, delete replacing the eliminated
// cancel, dependencies, import_plan) so ANY in-workspace agent can
// take an approved design to execution (the manager-grant gate is
// repealed, ruling 2026-10-27 / tsk-copper-refectory-generates). The
// platform (never agentd) arbitrates every guard — agentd passes
// bodies through verbatim and
// surfaces the platform's error classes verbatim, the mcpAutomation
// discipline trigger_*/workflow_* established.
//
// Red-first: the #1701 file landed BEFORE the worker implementation;
// the #1747 manager additions below landed BEFORE theirs — every
// registry/wire pin fails on an absent tool surface by construction.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- registry shape ---------------------------------------------------------

// TestMCPBoard_RegistryShape: the agent-internal registry exposes the
// worker tools PLUS the #1747 manager set — and exactly the parity
// matrix's live agent cells: claim_next (engine-internal, R9) and
// task_start (round-3 #2: execution = activation + engine claiming)
// are absent BY DESIGN, as is every cancel spelling (cancel is
// eliminated — delete replaces it).
func TestMCPBoard_RegistryShape(t *testing.T) {

	tools := mcpToolsList(t)
	board := map[string]map[string]any{}
	for _, tool := range tools {
		name := tool["name"].(string)
		if len(name) >= 6 && name[:6] == "board_" {
			board[name] = tool
		}
	}

	want := []string{
		// Worker tools (#1701, design 0067 §4.3c). board_record_verdict
		// (#1947): the spawned reviewer persona's ONE authorized write —
		// claim-token-required, worker projection only.
		"board_heartbeat", "board_move_stage", "board_read", "board_record_verdict", "board_report_throttle",
		// Manager set (#1747): projects.
		"board_create_project", "board_update_project", "board_project_archive",
		"board_project_unarchive", "board_project_pause", "board_project_resume",
		// Manager set (#1747): epics.
		"board_create_epic", "board_update_epic", "board_epic_activate",
		"board_epic_pause", "board_epic_archive", "board_epic_unarchive",
		"board_delete_epic",
		// Manager set (#1747): tasks + dependencies + plan.
		"board_create_story", "board_update_story", "board_add_dependency",
		"board_remove_dependency", "board_delete_story", "board_pause_story",
		"board_resume_story", "board_rearm_story", "board_import_plan",
		// S5 (the DEPOSED directive): the salvage handoff for a lapsed
		// claim — the orchestrator's manager route (canonical story
		// form; board_depose_task rides the one-release alias window).
		"board_depose_story",
		// Manager reads (#1837 P2a): the orchestrator's overview.
		"board_list", "board_health",
		// Config parity (owner ruling 2026-10-28 — api/sdk/mcp/ux are
		// all first-class citizens): the read-open source-annotated
		// view + the tier-selected REPLACE write.
		"board_get_config", "board_set_config",
		// #1836: the bulk complete (the bulk claim follows task_start's
		// owner-surface-only boundary and is deliberately absent).
		"board_move_stage_bulk",
		// #1836 sibling: the bulk delete (delete_task's batch form —
		// delete IS a manager action; only claim/create are owner-only).
		"board_delete_stories",
		// design 0070: the local, advisory pre-submit payload check
		// (the file-crud cutover's companion tool).
		"board_validate_file",
	}
	require.Len(t, board, len(want), "exactly the worker + #1747 manager board tools; got %v", board)
	for _, name := range want {
		assert.Contains(t, board, name, "board tool %s must be registered", name)
	}

	// The declared asymmetries, pinned as absences: claim_next is
	// engine-internal (R9 — absent on ALL surfaces, permanently);
	// task_start is dropped from agent surfaces (#1747 round 3);
	// cancel is eliminated everywhere (#1747 round 3). The bulk claim
	// follows the same boundary (round-3 #2).
	for _, forbidden := range []string{
		"board_claim_next", "board_claim", "board_task_start", "board_start_task",
		"board_start_tasks",
		"board_cancel", "board_cancel_task", "board_cancel_epic", "board_reorder",
	} {
		assert.NotContains(t, board, forbidden, "engine/eliminated action %s must NOT be an agent tool (#1747)", forbidden)
	}
}

// TestMCPBoard_ClaimTokenSchema: the claim-token semantics are visible
// in the tool schemas themselves — every in-flight write marks
// claimToken required on the TOKEN-GUARDED in-flight writes (heartbeat,
// throttle — the fresh worker's lease tools; a worker that loses its
// token is told by the schema, not by a runtime 409 alone).
// board_move_stage is the #1832 WS2 DUAL-ROUTE tool: the token is
// documented but NOT schema-required — a token present routes the
// worker projection, a token absent routes the manager-grade path
// (identity-gated at both tiers). board_read (no token — a read is not
// an in-flight write) must NOT require one.
func TestMCPBoard_ClaimTokenSchema(t *testing.T) {

	tools := mcpToolsList(t)
	for _, name := range []string{"board_heartbeat", "board_report_throttle", "board_record_verdict"} {
		tool, ok := tools[name]
		require.True(t, ok, "%s in tools/list", name)
		schema := tool["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		assert.Contains(t, props, "claimToken", "%s documents claimToken", name)
		assert.Contains(t, schema["required"], "claimToken", "%s REQUIRES claimToken (in-flight write)", name)
	}

	move := tools["board_move_stage"]
	require.NotNil(t, move, "board_move_stage in tools/list")
	moveSchema := move["inputSchema"].(map[string]any)
	moveProps := moveSchema["properties"].(map[string]any)
	assert.Contains(t, moveProps, "claimToken", "board_move_stage documents the worker token")
	assert.Equal(t, []any{"toStage"}, moveSchema["required"],
		"#1832 WS2: claimToken is conditionally required (worker vs manager-grade route), not schema-required")

	read := tools["board_read"]
	require.NotNil(t, read, "board_read in tools/list")
	schema := read["inputSchema"].(map[string]any)
	if req, ok := schema["required"]; ok {
		assert.NotContains(t, req, "claimToken", "board_read never demands a token")
	}
}

// --- wire contract (fake platform, verbatim passthrough) --------------------

// TestMCPBoard_WireContract: each tool drives the documented seam call
// (method, path, SA token, workspace stamp) and carries the caller's
// fields — claimToken above all — to the wire VERBATIM. The platform
// validates; agentd re-models nothing.
func TestMCPBoard_WireContract(t *testing.T) {

	t.Run("board_read item get", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1","stage":"in_progress"}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_read", map[string]any{"shortId": "tsk-aaaa1111"})
		require.NoError(t, err)
		assert.Contains(t, out, `"stage":"in_progress"`, "platform body passes through verbatim")
		assert.Equal(t, http.MethodGet, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111", rec.path)
		assert.Equal(t, "workspaceID=ws-1", rec.query)
		assert.Equal(t, "Bearer sa-token", rec.auth)
		assert.Empty(t, rec.body)
	})

	t.Run("board_read prefers taskId in the path", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_read", map[string]any{
			"taskId": "11111111-1111-1111-1111-111111111111", "shortId": "tsk-aaaa1111"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/11111111-1111-1111-1111-111111111111", rec.path)
	})

	t.Run("board_heartbeat wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"extended":true}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_heartbeat", map[string]any{
			"shortId": "tsk-aaaa1111", "claimToken": "tok-1",
			"observedProgressAt": "2026-10-18T12:00:00Z"})
		require.NoError(t, err)
		assert.Equal(t, `{"extended":true}`, out)
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/heartbeat", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		for key, want := range map[string]string{
			"claimToken":         `"tok-1"`,
			"shortId":            `"tsk-aaaa1111"`,
			"observedProgressAt": `"2026-10-18T12:00:00Z"`,
			"workspaceID":        `"ws-1"`,
		} {
			assert.JSONEq(t, want, string(wire[key]), "%s reaches the wire verbatim", key)
		}
	})

	t.Run("board_report_throttle wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"stage":"backlog"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_report_throttle", map[string]any{
			"shortId": "tsk-aaaa1111", "claimToken": "tok-1", "retryAfterSeconds": 90})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/throttle", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"tok-1"`, string(wire["claimToken"]))
		assert.JSONEq(t, `90`, string(wire["retryAfterSeconds"]))
		assert.JSONEq(t, `"ws-1"`, string(wire["workspaceID"]))
	})

	t.Run("board_move_stage wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"stage":"in_review"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_move_stage", map[string]any{
			"shortId": "tsk-aaaa1111", "claimToken": "tok-1",
			"toStage": "in_review", "evidenceUrl": "https://example.test/pr/9",
			"requestId": "11111111-2222-3333-4444-555555555555"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/move-stage", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"in_review"`, string(wire["toStage"]))
		assert.JSONEq(t, `"https://example.test/pr/9"`, string(wire["evidenceUrl"]))
		assert.JSONEq(t, `"11111111-2222-3333-4444-555555555555"`, string(wire["requestId"]))
		assert.JSONEq(t, `"tok-1"`, string(wire["claimToken"]))
	})

	t.Run("board_record_verdict wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"verdict":{"applied":true}}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_record_verdict", map[string]any{
			"shortId": "tsk-aaaa1111", "claimToken": "tok-1", "verdict": "fail",
			"reviewer": "spawned-reviewer", "evidenceUrl": "https://example.test/pr/9",
			"findings": []any{map[string]any{"severity": "major", "pointer": "pkg/x.go:9"}}})
		require.NoError(t, err)
		assert.Contains(t, out, `"applied":true`)
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/verdict", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"tok-1"`, string(wire["claimToken"]), "the reviewer's claim token reaches the wire verbatim")
		assert.JSONEq(t, `"fail"`, string(wire["verdict"]))
		assert.JSONEq(t, `"spawned-reviewer"`, string(wire["reviewer"]))
		assert.JSONEq(t, `[{"pointer":"pkg/x.go:9","severity":"major"}]`, string(wire["findings"]))
		assert.JSONEq(t, `"ws-1"`, string(wire["workspaceID"]))
	})

	t.Run("board_record_verdict without a token refuses pre-wire", func(t *testing.T) {
		api, _ := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_record_verdict", map[string]any{
			"shortId": "tsk-aaaa1111", "verdict": "fail", "reviewer": "tokless"})
		require.Error(t, err, "the verdict is a claim-guarded in-flight write — the token is required at the seam")
	})

	t.Run("nested task wrapper tolerated, unwrapped on the wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_heartbeat", map[string]any{
			"task": map[string]any{"shortId": "tsk-aaaa1111", "claimToken": "tok-2"}})
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.NotContains(t, wire, "task", "wrapper key must NOT reach the wire")
		assert.JSONEq(t, `"tok-2"`, string(wire["claimToken"]))
	})
}

// TestMCPBoard_ErrorPassthrough: the platform's guard classes are the
// worker's steering — a 409 claim_token ("your claim lapsed or moved")
// and a 404 with did-you-mean suggestions surface verbatim in the tool
// error, never swallowed into a generic failure.
func TestMCPBoard_ErrorPassthrough(t *testing.T) {

	t.Run("claim-token guard", func(t *testing.T) {
		api, _ := newAutomationAPI(t, http.StatusConflict,
			`{"error":"board: guard claim_token: heartbeat requires a live claim (stage + token)","code":"claim_token"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_heartbeat", map[string]any{
			"shortId": "tsk-aaaa1111", "claimToken": "stale", "observedProgressAt": "2026-10-18T12:00:00Z"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "409", "status class visible")
		assert.Contains(t, err.Error(), "claim_token", "guard code visible")
		assert.Contains(t, err.Error(), "live claim", "platform detail visible")
	})

	t.Run("foreign task 404 with suggestions", func(t *testing.T) {
		api, _ := newAutomationAPI(t, http.StatusNotFound,
			`{"error":"board: task \"tsk-aaa\" not found","suggestions":["tsk-aaaa1111"]}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_read", map[string]any{"shortId": "tsk-aaa"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
		assert.Contains(t, err.Error(), "tsk-aaaa1111", "did-you-mean suggestion survives the hop")
	})
}

// TestMCPBoard_RefValidation: the task reference is interpolated into a
// URL path — malformed or traversal-shaped refs are refused BEFORE any
// wire call (the validateAutomationID discipline; board refs are short
// ids/UUIDs, a superset of the automation id charset).
func TestMCPBoard_RefValidation(t *testing.T) {

	api, rec := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)

	for _, ref := range []string{"", "../../etc/passwd", "tsk-a;b", "a b", "tsk/%2e%2e"} {
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_read", map[string]any{"shortId": ref})
		require.Error(t, err, "ref %q must be refused pre-wire", ref)
	}
	assert.Empty(t, rec.method, "no wire call may happen for a refused ref")
}

// TestMCPBoard_DispatcherWiring: the catalog and the dispatcher agree —
// every board_* tool in tools/list routes through callMCPTool (the
// registry is not a brochure), and the call reaches the fake platform.
func TestMCPBoard_DispatcherWiring(t *testing.T) {

	api, rec := newAutomationAPI(t, 200, `{"extended":true}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "board_heartbeat", map[string]any{
		"shortId": "tsk-aaaa1111", "claimToken": "tok-1", "observedProgressAt": "2026-10-18T12:00:00Z"})
	require.NoError(t, err)
	assert.Contains(t, out, `"extended":true`)
	assert.Equal(t, "/internal/v1/automation/board/tasks/heartbeat", rec.path)

	// Shape routing at the dispatcher (#1832 WS2 dual-route): a
	// token-less move_stage rides the MANAGER-GRADE wire path for ANY
	// in-workspace caller — identity or not (the #1747 §5 manager gate
	// is repealed, ruling 2026-10-27 / tsk-copper-refectory-generates:
	// identity is audit attribution, never a fence; the claim token is
	// the in-flight protection).
	rec.path, rec.method = "", ""
	_, err = callMCPTool(context.Background(), mcpTestPassword, "board_move_stage", map[string]any{
		"shortId": "tsk-aaaa1111", "toStage": "in_review"})
	require.NoError(t, err, "a token-less, identity-less move rides the manager path — attribution degrades to empty")
	assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111/move-stage", rec.path,
		"the manager-grade move rides the :id-addressed manager route")

	// A token-less move_stage WITH an (injected) session identity
	// routes the same MANAGER-GRADE wire path (attribution rides).
	rec.path, rec.method = "", ""
	_, err = callMCPTool(context.Background(), mcpTestPassword, "board_move_stage", map[string]any{
		"shortId": "tsk-aaaa1111", "toStage": "backlog",
		"lsp_injected_session": "ses_collab1"})
	require.NoError(t, err)
	assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111/move-stage", rec.path,
		"the manager-grade move rides the :id-addressed manager route")
}

// mcpToolsList fetches tools/list as a name→tool map.
func mcpToolsList(t *testing.T) map[string]map[string]any {
	t.Helper()
	body, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	r := mcpAuthedRequest(body)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, r)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	out := map[string]map[string]any{}
	for _, tool := range resp.Result.Tools {
		out[tool["name"].(string)] = tool
	}
	return out
}

// --- #1747 manager tools: wire contract + grant differentiation ---------------

// TestMCPBoard_ManagerWireContract: each manager tool drives its seam
// route with the pod SA token + the workspace/session stamps, and the
// caller's fields pass through verbatim (the mcpAutomation discipline).
func TestMCPBoard_ManagerWireContract(t *testing.T) {
	t.Run("board_create_story wire (file-backed payload, design 0070)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-cccc3333"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "task.json")
		payload := `{"title":"T1","description":"do","dependsOn":["tsk-aaaa1111"],` +
			`"acceptanceCriteria":[{"text":"done","met":false}]}`
		require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_story", map[string]any{
			"epicId": "epc-bbbb2222", "bodyFile": path,
			"lsp_injected_session": "ses_workercaller1",
		})
		require.NoError(t, err)
		assert.Contains(t, out, "tsk-cccc3333")
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"T1"`, string(wire["title"]), "the payload file's fields reach the wire verbatim")
		assert.JSONEq(t, `"do"`, string(wire["description"]))
		assert.NotEmpty(t, wire["dependsOn"], "dependsOn rides the payload file")
		assert.NotContains(t, wire, "bodyFile")
		assert.JSONEq(t, `"ws-1"`, string(wire["workspaceID"]), "the workspace stamp")
		assert.JSONEq(t, `"ses_workercaller1"`, string(wire["sessionId"]),
			"the injected session identity rides the wire (audit attribution + the seam belt)")
	})

	t.Run("board_epic_activate wire (ack flag)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"epic":{"status":"active"}}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_epic_activate", map[string]any{
			"epicId": "epc-bbbb2222", "acknowledgeInactiveDeps": true, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/activate", rec.path)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `true`, string(wire["acknowledgeInactiveDeps"]))
	})

	t.Run("board_delete_story wire (reconnect query)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"item_kind":"task"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_delete_story", map[string]any{
			"taskId": "tsk-cccc3333", "reconnect": true, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, http.MethodDelete, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-cccc3333", rec.path)
		assert.Contains(t, rec.query, "workspaceID=ws-1")
		assert.Contains(t, rec.query, "reconnect=true")
		assert.Contains(t, rec.query, "sessionId=ses-1", "DELETE carries the session on the query")
	})

	t.Run("board_delete_epic wire (force query)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"item_kind":"epic"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_delete_epic", map[string]any{
			"epicId": "epc-bbbb2222", "force": true, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, http.MethodDelete, rec.method)
		assert.Contains(t, rec.query, "force=true")
	})

	t.Run("board_import_plan wire (dry-run, file-backed plan)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"dry_run":true}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "plan.json")
		payload := `{"epic":{"title":"E"},"tasks":[{"key":"k1","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "dryRun": true, "planFile": path,
			"lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/projects/prj-aaaa1111/import-plan", rec.path)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `true`, string(wire["dryRun"]))
		assert.JSONEq(t, `{"title":"E"}`, string(wire["epic"]), "the unified plan doc's epic arm rides the wire verbatim")
		assert.NotContains(t, wire, "planFile")
	})

	t.Run("board_delete_stories wire (#1836 sibling)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"results":[{"id":"tsk-cccc3333","status":"done","orphaned_dependents":["tsk-eeee5555"]}],"count":1}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_delete_stories", map[string]any{
			"taskIds": []any{"tsk-cccc3333", "tsk-dddd4444"}, "reconnect": true,
			"requestId":            "bulk-del-1",
			"lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Contains(t, out, "tsk-cccc3333")
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/delete-bulk", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `["tsk-cccc3333","tsk-dddd4444"]`, string(wire["taskIds"]),
			"the batch array reaches the wire verbatim")
		assert.JSONEq(t, `true`, string(wire["reconnect"]),
			"the ONE shared reconnect flag reaches the wire")
		assert.JSONEq(t, `"bulk-del-1"`, string(wire["requestId"]))
		assert.JSONEq(t, `"ses-1"`, string(wire["sessionId"]),
			"the injected session identity rides the wire (audit attribution + the seam belt)")
	})

	t.Run("board_move_stage_bulk wire (#1836)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"results":[{"id":"tsk-cccc3333","status":"done"}],"count":1}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_move_stage_bulk", map[string]any{
			"taskIds": []any{"tsk-cccc3333", "tsk-dddd4444"}, "toStage": "done",
			"evidenceUrl": "https://example/p", "requestId": "bulk-1",
			"lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Contains(t, out, "tsk-cccc3333")
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/move-stage-bulk", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `["tsk-cccc3333","tsk-dddd4444"]`, string(wire["taskIds"]),
			"the batch array reaches the wire verbatim")
		assert.JSONEq(t, `"done"`, string(wire["toStage"]))
		assert.JSONEq(t, `"https://example/p"`, string(wire["evidenceUrl"]))
		assert.JSONEq(t, `"ses-1"`, string(wire["sessionId"]),
			"the injected session identity rides the wire (audit attribution + the seam belt)")
	})

	t.Run("board_pause_story / board_pause_project wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"paused":true}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_pause_story", map[string]any{
			"taskId": "tsk-cccc3333", "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-cccc3333/pause", rec.path)

		_, err = mcpAutomation(context.Background(), mcpTestPassword, "board_project_pause", map[string]any{
			"projectId": "prj-aaaa1111", "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/projects/prj-aaaa1111/pause", rec.path)
	})
}

// TestMCPBoard_ManagerOpenToAnySession (re-peals the #1747 §5 manager
// gate, ruling 2026-10-27 / tsk-copper-refectory-generates): manager
// tools are open to ANY in-workspace session — no identity required
// (absent identity degrades to empty audit attribution; the call
// proceeds), spawned board workers INCLUDED. The one surviving
// registry refusal is the #1946 self-certification pin (pinned in
// board_update_task_1946_test.go). Worker tools are unaffected.
func TestMCPBoard_ManagerOpenToAnySession(t *testing.T) {
	t.Run("no session identity → the call proceeds, attribution degrades to empty", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_pause_story", map[string]any{
			"taskId": "tsk-cccc3333"})
		require.NoError(t, err, "an unidentified caller proceeds — identity is attribution, never a grant")
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-cccc3333/pause", rec.path)
		assert.NotEmpty(t, rec.method, "the wire call happens")
	})

	t.Run("spawned-worker session → the call proceeds (workers hold manager tools too)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_spawnedworker")

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_epic_activate", map[string]any{
			"epicId": "epc-bbbb2222", "lsp_injected_session": "ses_spawnedworker"})
		require.NoError(t, err, "a spawned worker holds the manager tools — the impersonation threat is a prompting concern, not RBAC")
		assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/activate", rec.path)
		assert.NotEmpty(t, rec.method)

		// The declared fallback rides the same open path.
		rec.path, rec.method = "", ""
		_, err = mcpAutomation(context.Background(), mcpTestPassword, "board_pause_story", map[string]any{
			"taskId": "tsk-cccc3333", "sessionId": "ses_spawnedworker"})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-cccc3333/pause", rec.path)
		assert.NotEmpty(t, rec.method)
	})

	t.Run("injection wins over a declared worker id", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "epic.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"E"}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_epic", map[string]any{
			"projectId": "prj-aaaa1111", "bodyFile": path,
			"sessionId": "ses_spawnedworker", "lsp_injected_session": "sessi-2feellha"})
		require.NoError(t, err, "the harness-attested injection is authoritative (the send_message pattern)")
		assert.Equal(t, "/internal/v1/automation/board/projects/prj-aaaa1111/epics", rec.path)
	})

	t.Run("worker tools need no session (the in-flight surface is unchanged)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"extended":true}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_heartbeat", map[string]any{
			"shortId": "tsk-cccc3333", "claimToken": "tok", "observedProgressAt": "2026-10-19T12:00:00Z"})
		require.NoError(t, err)
		assert.NotEmpty(t, rec.method)
	})
}

// TestMCPBoard_WorkerRegistry: the spawn-time registry is append-only
// and file-backed (restart-safe), the restart-marker pattern.
func TestMCPBoard_WorkerRegistry(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/workers.jsonl"
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", path)

	fresh := newBoardWorkerRegistry(path)
	assert.False(t, fresh.isWorker("ses_a"), "empty registry")

	fresh.record("ses_a")
	fresh.record("ses_a") // idempotent
	assert.True(t, fresh.isWorker("ses_a"))

	// A NEW process hydrates from the same file.
	rehydrated := newBoardWorkerRegistry(path)
	assert.True(t, rehydrated.isWorker("ses_a"), "the record survives restarts")
	assert.False(t, rehydrated.isWorker("ses_never"))
}

// TestMCPBoard_ManagerDeclaredPhraseResolves (#1816): the DECLARED
// sessionId fallback (degraded pod — no plugin injection) accepts a
// 3-word PHRASE: it resolves through the same §4.4 precedence as
// every other caller-typed session ref, and the RESOLVED harness id
// rides the platform wire (the platform's SessionID column is the
// internal key).
func TestMCPBoard_ManagerDeclaredPhraseResolves(t *testing.T) {
	f := newFakeAgent()
	caller := f.newSession("board-caller")
	withAgentServer(t, f.handler(t))
	store := withShortIDStore(t)
	store.put("amber-falcon-glides", caller)

	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-dddd4444"}`)
	setupRenameWorkspaceEnv(t, api)

	path := filepath.Join(t.TempDir(), "task.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"title":"T-phrase","description":"d","acceptanceCriteria":[{"text":"c"}]}`), 0o600))

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_story", map[string]any{
		"epicId":    "epc-bbbb2222",
		"bodyFile":  path,
		"sessionId": "amber-falcon-glides", // declared fallback, phrase form
	})
	require.NoError(t, err)
	assert.Contains(t, out, "tsk-dddd4444")

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.JSONEq(t, `"`+caller+`"`, string(wire["sessionId"]),
		"the DECLARED phrase resolves to the harness id on the platform wire")
}

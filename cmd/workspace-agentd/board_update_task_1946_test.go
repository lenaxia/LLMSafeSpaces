// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// board_update_task_1946_test.go — the #1946 dual-route on
// board_update_task's dispatcher (mirroring board_move_stage's #1832
// WS2 shape): a claim-token-carrying call rides the worker projection's
// scoped set-criteria-met route; a token-less call keeps the manager
// path byte-identically — open to any in-workspace session (ruling
// 2026-10-27 / tsk-copper-refectory-generates) EXCEPT the registry's
// self-certification pin: a spawned worker's token-less setCriteriaMet
// refuses (#1946's grant does not widen).

// TestMCPBoard_UpdateTaskDualRoute_1946: routing + wire shape.
func TestMCPBoard_UpdateTaskDualRoute_1946(t *testing.T) {
	t.Run("claimToken → the worker set-criteria-met route", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1","stage":"in_review","claim_persona":"reviewer"}`)
		setupRenameWorkspaceEnv(t, api)

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
			"taskId": "tsk-aaaa1111", "claimToken": "tok-rev",
			"setCriteriaMet": map[string]any{"index": 0, "met": true}})
		require.NoError(t, err)
		assert.Contains(t, out, "claim_persona")
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/set-criteria-met", rec.path,
			"the token-carrying call rides the worker projection (#1946)")

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"tsk-aaaa1111"`, string(wire["taskId"]))
		assert.JSONEq(t, `"tok-rev"`, string(wire["claimToken"]), "the charter token rides the worker wire")
		assert.JSONEq(t, `{"index":0,"met":true}`, string(wire["setCriteriaMet"]))
		assert.Nil(t, wire["sessionId"], "no manager identity stamp on the worker route")
	})

	t.Run("nested {task:{…}} wrapper tolerated", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
			"task": map[string]any{"shortId": "tsk-aaaa1111", "claimToken": "tok-rev",
				"setCriteriaMet": map[string]any{"index": 1, "met": false}}})
		require.NoError(t, err)
		assert.Equal(t, "/internal/v1/automation/board/tasks/set-criteria-met", rec.path)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"tok-rev"`, string(wire["claimToken"]))
	})

	t.Run("token-less → the manager route, byte-identical (open to any session)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "patch.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"T"}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
			"taskId": "tsk-aaaa1111", "patchFile": path,
			"lsp_injected_session": "sessi-2feellha"})
		require.NoError(t, err)
		assert.Equal(t, http.MethodPatch, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111", rec.path,
			"the token-less call keeps the manager-grade PATCH")
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"T"`, string(wire["title"]), "the patch file's fields ride the wire verbatim")
		assert.JSONEq(t, `"sessi-2feellha"`, string(wire["sessionId"]), "the manager identity stamp rides")
	})

	t.Run("spawned worker, token-less → the self-certification pin, unchanged", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_spawnedselfcert")

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
			"taskId": "tsk-aaaa1111", "setCriteriaMet": map[string]any{"index": 0, "met": true},
			"lsp_injected_session": "ses_spawnedselfcert"})
		require.Error(t, err, "a task worker self-certifying through the manager shape stays refused")
		assert.Contains(t, err.Error(), "self-certification")
		assert.Contains(t, err.Error(), "#1946")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})
}

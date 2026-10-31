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
// board_update_story's dispatcher (mirroring board_move_stage's #1832
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

		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
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

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
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
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
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

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
			"taskId": "tsk-aaaa1111", "setCriteriaMet": map[string]any{"index": 0, "met": true},
			"lsp_injected_session": "ses_spawnedselfcert"})
		require.Error(t, err, "a task worker self-certifying through the manager shape stays refused")
		assert.Contains(t, err.Error(), "self-certification")
		assert.Contains(t, err.Error(), "#1946")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})

	// PR review r2 finding 2: the pin must read the SAME unwrapped
	// shape the dispatcher honors — {"plan": {…setCriteriaMet…}} from a
	// spawned worker is the wrapper-key bypass of the flat-shape-only
	// pin, and it refuses identically.
	t.Run("spawned worker, {plan:{…}} wrapper → the pin reads through the wrapper", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_wrapshaper")

		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
			"plan":                 map[string]any{"setCriteriaMet": map[string]any{"index": 0, "met": true}},
			"lsp_injected_session": "ses_wrapshaper"})
		require.Error(t, err, "wrapper-key shaping must not slip setCriteriaMet past the pin")
		assert.Contains(t, err.Error(), "self-certification")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})

	// PR review r3 finding A: the criteria-REPLACEMENT door — a
	// worker's patchFile acceptanceCriteria replace rewrites met flags
	// with no persona guard (Service.UpdateTask); the pin reads the
	// file and refuses the worker-sourced shape. A non-worker's
	// replace stays legal.
	t.Run("spawned worker, patchFile acceptanceCriteria replace → refused", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_criteriarepl")

		path := filepath.Join(t.TempDir(), "patch.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"acceptanceCriteria":[{"text":"done","met":true}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
			"taskId": "tsk-aaaa1111", "patchFile": path,
			"lsp_injected_session": "ses_criteriarepl"})
		require.Error(t, err, "a worker re-writing its own criteria (met flags ride the replace) is self-certification")
		assert.Contains(t, err.Error(), "#1946")
		assert.Contains(t, err.Error(), "acceptanceCriteria")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})

	t.Run("non-worker, same patchFile acceptanceCriteria replace → legal", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "patch.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"acceptanceCriteria":[{"text":"done","met":true}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
			"taskId": "tsk-aaaa1111", "patchFile": path,
			"lsp_injected_session": "ses_orchestrator"})
		require.NoError(t, err, "a manager/orchestrator replace is the pin's non-concern")
		assert.Equal(t, http.MethodPatch, rec.method)
		assert.Contains(t, rec.body, "acceptanceCriteria", "the replace rides the wire for non-workers")
	})

	// PR review r4 finding 1: the CREATE shapes — a spawned worker's
	// bodyFile/planFile carrying met:true criteria authors done-ready
	// rows with zero reviewer involvement; the pin (now inside
	// dispatch, post-merge) refuses every met-flag-writing shape.
	t.Run("spawned worker, create_task bodyFile pre-met criteria → refused", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_premetcreate")

		path := filepath.Join(t.TempDir(), "body.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"T","description":"d","acceptanceCriteria":[{"text":"done","met":true}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_story", map[string]any{
			"epicId": "epc-aaaa1111", "bodyFile": path,
			"lsp_injected_session": "ses_premetcreate"})
		require.Error(t, err, "a worker creating its own work pre-certified is self-certification")
		assert.Contains(t, err.Error(), "#1946")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})

	t.Run("spawned worker, import_plan planFile pre-met task → refused", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_premetimport")

		path := filepath.Join(t.TempDir(), "plan.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"epic":{"title":"E"},"tasks":[{"key":"a","title":"T","description":"d","acceptanceCriteria":[{"text":"done","met":true}]}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path,
			"lsp_injected_session": "ses_premetimport"})
		require.Error(t, err, "a worker importing its own work pre-certified is self-certification")
		assert.Contains(t, err.Error(), "#1946")
		assert.Empty(t, rec.method, "no wire call for the refused call")
	})

	t.Run("spawned worker, create_task ordinary unmet criteria → legal", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)
		boardWorkerSessions.record("ses_plaincreate")

		path := filepath.Join(t.TempDir(), "body.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"T","description":"d","acceptanceCriteria":[{"text":"done"}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_story", map[string]any{
			"epicId": "epc-aaaa1111", "bodyFile": path,
			"lsp_injected_session": "ses_plaincreate"})
		require.NoError(t, err, "ordinary backlog criteria (no met flags) are a worker's legal create")
		assert.Equal(t, http.MethodPost, rec.method)
	})

	t.Run("non-worker, create_task pre-met criteria → legal (importing completed work)", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "body.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"T","description":"d","acceptanceCriteria":[{"text":"done","met":true}]}`), 0o600))
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_story", map[string]any{
			"epicId": "epc-aaaa1111", "bodyFile": path,
			"lsp_injected_session": "ses_orchestrator"})
		require.NoError(t, err, "the platform surface deliberately supports manager-sourced pre-met creates")
		assert.Equal(t, http.MethodPost, rec.method)
		assert.Contains(t, rec.body, `"met":true`, "the pre-met create rides the wire for non-workers")
	})
}

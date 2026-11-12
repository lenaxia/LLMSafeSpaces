// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_story_alias_test.go — the Sprint Board story rename's
// one-release compatibility window (owner ruling 2026-10-30 phase 2),
// proven END TO END through the agentd tools/call surface: calling a
// PRE-RENAME tool name behaves exactly as its canonical twin (the
// story operation lands on the platform seam, wire byte-compatible
// with the pre-rename shape) and the answer carries the deprecation
// notice as a second content block. The legacy INPUT-KEY spellings
// (taskId/taskIds) ride the same window under canonical names; the
// canonical spellings (storyId/storyIds) translate onto the wire's
// task term and carry no notice.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aliasWindowCall drives one tools/call through the real mcpHandler.
func aliasWindowCall(t *testing.T, name string, args map[string]any) (texts []string, isError bool) {
	t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	for _, c := range resp.Result.Content {
		texts = append(texts, c.Text)
	}
	return texts, resp.Result.IsError
}

// TestBoardStoryAlias_WindowEndToEnd: the acceptance pin — call the
// OLD tool name, the story IS created (the platform seam sees the
// canonical create) and the deprecation notice names the canonical
// tool and the removal release.
func TestBoardStoryAlias_WindowEndToEnd(t *testing.T) {
	api, rec := newAutomationAPI(t, 201, `{"id":"t-9","short_id":"tsk-alias-window-1"}`)
	setupRenameWorkspaceEnv(t, api)

	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "story.json")
	require.NoError(t, os.WriteFile(bodyFile, []byte(
		`{"title":"Alias window story","description":"charter","acceptanceCriteria":[{"text":"c","met":false}]}`), 0o600))

	texts, isError := aliasWindowCall(t, "board_create_task", map[string]any{
		"epicId":   "epc-bbbb2222",
		"bodyFile": bodyFile,
	})
	require.False(t, isError)
	require.Len(t, texts, 2, "alias answer = platform body + deprecation notice")

	// The story was created: the canonical create seam saw the call.
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", rec.path,
		"the alias path drives the CANONICAL create seam")
	assert.Contains(t, texts[0], "tsk-alias-window-1", "the platform body (the created story) passes through")
	assert.Contains(t, rec.body, "Alias window story", "the payload file's fields rode the wire")

	// The notice names both names and the removal release.
	assert.Contains(t, texts[1], "DEPRECATED")
	assert.Contains(t, texts[1], "board_create_task")
	assert.Contains(t, texts[1], "board_create_story")
	assert.Contains(t, texts[1], "v0.46.0", "the notice names the removal release")
	assert.NotContains(t, rec.body, "storyId", "the wire keeps its task-term body (byte-compatible)")
}

// TestBoardStoryAlias_LegacyInputKey: the pre-rename INPUT KEY under
// the canonical name resolves identically and carries the key notice.
func TestBoardStoryAlias_LegacyInputKey(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-aaaa1111","paused":true}`)
	setupRenameWorkspaceEnv(t, api)

	texts, isError := aliasWindowCall(t, "board_pause_story", map[string]any{"taskId": "tsk-aaaa1111"})
	require.False(t, isError)
	require.Len(t, texts, 2)
	assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111/pause", rec.path,
		"the legacy key resolves — the pause lands")
	assert.Contains(t, texts[1], "DEPRECATED")
	assert.Contains(t, texts[1], "storyId")
}

// TestBoardStoryAlias_CanonicalKeyTranslatesSilently: the canonical
// storyId spelling reaches the same seam (translated onto the wire's
// task term) with NO notice anywhere.
func TestBoardStoryAlias_CanonicalKeyTranslatesSilently(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-aaaa1111","paused":false}`)
	setupRenameWorkspaceEnv(t, api)

	texts, isError := aliasWindowCall(t, "board_resume_story", map[string]any{"storyId": "tsk-aaaa1111"})
	require.False(t, isError)
	require.Len(t, texts, 1, "canonical name + canonical key carry no notice")
	assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111/resume", rec.path,
		"storyId translates onto the wire's task term")
}

// TestBoardStoryAlias_CanonicalMap: every renamed tool resolves
// through the window, and nothing else rewrites.
func TestBoardStoryAlias_CanonicalMap(t *testing.T) {
	want := map[string]string{
		"board_create_task":  "board_create_story",
		"board_update_task":  "board_update_story",
		"board_delete_task":  "board_delete_story",
		"board_delete_tasks": "board_delete_stories",
		"board_rearm_task":   "board_rearm_story",
		"board_pause_task":   "board_pause_story",
		"board_resume_task":  "board_resume_story",
		// S5 (the DEPOSED directive): born task-named on the branch,
		// ported to the story vocabulary with the same window.
		"board_depose_task": "board_depose_story",
	}
	require.Len(t, boardStoryToolAliases, len(want), "the renamed registry tools, exactly")
	for alias, canonical := range want {
		got, notice := boardStoryToolCanonical(alias)
		assert.Equal(t, canonical, got)
		assert.Contains(t, notice, canonical)
		assert.Contains(t, notice, "v0.46.0")
	}
	for _, name := range []string{"board_create_story", "board_read", "board_list", "session_read", "write"} {
		got, notice := boardStoryToolCanonical(name)
		assert.Equal(t, name, got, "non-alias names pass through unchanged")
		assert.Empty(t, notice)
	}
}

// TestBoardStoryAlias_Normalization: storyId/storyIds translate onto
// the wire's task spelling (flat + the tolerated nested task wrapper),
// the legacy spelling flags the window, and non-story tools are
// untouched.
func TestBoardStoryAlias_Normalization(t *testing.T) {
	out, legacy := normalizeBoardStoryArgs("board_move_stage_bulk", map[string]any{
		"storyIds": []any{"tsk-1"}, "toStage": "done"})
	assert.False(t, legacy)
	assert.NotContains(t, out, "storyIds")
	assert.Equal(t, []any{"tsk-1"}, out["taskIds"])
	assert.Equal(t, "done", out["toStage"])

	out, legacy = normalizeBoardStoryArgs("board_move_stage", map[string]any{
		"task": map[string]any{"storyId": "tsk-2", "claimToken": "tok"}})
	assert.False(t, legacy)
	nested, ok := out["task"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, nested, "storyId")
	assert.Equal(t, "tsk-2", nested["taskId"])
	assert.Equal(t, "tok", nested["claimToken"])

	_, legacy = normalizeBoardStoryArgs("board_delete_story", map[string]any{"taskId": "tsk-3"})
	assert.True(t, legacy, "the legacy spelling flags the notice window")

	out, legacy = normalizeBoardStoryArgs("session_read", map[string]any{"sessionId": "s"})
	assert.False(t, legacy)
	assert.Equal(t, map[string]any{"sessionId": "s"}, out, "non-board tools are untouched")
}

// TestBoardStoryAlias_RegistryAdvertisesCanonicalOnly: the catalog
// (tools/list) carries ONLY the canonical names — the alias is a
// call-time shim, so the in-workspace agent learns the new vocabulary
// while pinned callers still land (the parity matrix stays clean).
func TestBoardStoryAlias_RegistryAdvertisesCanonicalOnly(t *testing.T) {
	tools := mcpToolsList(t)
	for name := range boardStoryToolAliases {
		_, aliased := tools[name]
		assert.False(t, aliased, "alias %s must NOT be advertised in tools/list (call-time only)", name)
		canonical, _ := boardStoryToolCanonical(name)
		_, registered := tools[canonical]
		assert.True(t, registered, "canonical %s must be registered", canonical)
	}
}

// TestBoardStoryAlias_LeaseToolsCanonicalKey (review round 1, finding
// 1's regression pin): the two lease tools' schemas advertise storyId
// with the rest of the rename — the canonical key MUST reach the wire
// (translated onto the platform's taskId term), never be silently
// dropped at the REST binding, and the legacy spelling MUST carry the
// window notice. board_heartbeat stands in for report_throttle (the
// same schema shape, the same dispatch path).
func TestBoardStoryAlias_LeaseToolsCanonicalKey(t *testing.T) {
	t.Run("canonical storyId reaches the wire", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"extended":true}`)
		setupRenameWorkspaceEnv(t, api)

		texts, isError := aliasWindowCall(t, "board_heartbeat", map[string]any{
			"storyId": "11111111-1111-1111-1111-111111111111", "claimToken": "tok-1",
		})
		require.False(t, isError)
		assert.Contains(t, texts[0], `"extended":true`, "the heartbeat lands")
		assert.NotContains(t, rec.body, "storyId",
			"the canonical key translates onto the wire's task term — never silently dropped at the REST binding")
		assert.Contains(t, rec.body, `"taskId":"11111111-1111-1111-1111-111111111111"`,
			"the UUID reference rides the wire under the spelling the platform binds")
	})

	t.Run("legacy taskId carries the notice", func(t *testing.T) {
		api, _ := newAutomationAPI(t, 200, `{"extended":true}`)
		setupRenameWorkspaceEnv(t, api)

		texts, isError := aliasWindowCall(t, "board_heartbeat", map[string]any{
			"taskId": "11111111-1111-1111-1111-111111111111", "claimToken": "tok-1",
		})
		require.False(t, isError)
		require.Len(t, texts, 2, "the legacy key spelling carries the deprecation notice")
		assert.Contains(t, texts[1], "DEPRECATED")
		assert.Contains(t, texts[1], "storyId")
	})

	t.Run("report_throttle shares the scope", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)

		_, isError := aliasWindowCall(t, "board_report_throttle", map[string]any{
			"storyId": "tsk-aaaa1111", "claimToken": "tok-1"})
		require.False(t, isError)
		assert.Contains(t, rec.body, `"taskId":"tsk-aaaa1111"`,
			"the throttle tool's canonical key translates like every other story-addressing tool")
	})
}

// TestBoardStoryAlias_NoticeRidesErrors (review round 1's missing
// unhappy tier): the deprecation notice rides FAILING calls too — an
// alias-named call that errors (here: the platform seam answers 404)
// still carries the notice as the second content block. The contract
// in the CHANGELOG ("errors included") is pinned, not assumed.
func TestBoardStoryAlias_NoticeRidesErrors(t *testing.T) {
	api, _ := newAutomationAPI(t, 404, `{"error":"not found","code":"not_found"}`)
	setupRenameWorkspaceEnv(t, api)

	texts, isError := aliasWindowCall(t, "board_pause_task", map[string]any{"taskId": "tsk-missing-1"})
	assert.True(t, isError, "the platform 404 renders an error result")
	require.Len(t, texts, 2, "the alias notice rides the error envelope")
	assert.Contains(t, texts[1], "DEPRECATED")
	assert.Contains(t, texts[1], "board_pause_story")
}

// TestBoardStoryAlias_BulkDeletePluralization (review round 2,
// finding A's regression pins): "stories" does not contain the
// substring "story" — the round-1 scope predicate (a substring match)
// missed board_delete_stories, whose REQUIRED canonical storyIds key
// then rode the wire untranslated into a REST DTO that binds only
// taskIds (400 taskIds-is-required). The scope is now CATALOG-DERIVED
// (a tool is in scope exactly when its schema advertises
// storyId/storyIds), which this family pins from every side: the
// canonical key through the real handler, the legacy key's notice,
// and the alias path.
func TestBoardStoryAlias_BulkDeletePluralization(t *testing.T) {
	t.Run("canonical storyIds reaches the wire as taskIds", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `[{"id":"tsk-aaaa1111","status":"done"}]`)
		setupRenameWorkspaceEnv(t, api)

		texts, isError := aliasWindowCall(t, "board_delete_stories", map[string]any{
			"storyIds": []any{"tsk-aaaa1111"}, "reconnect": false})
		require.False(t, isError)
		assert.Contains(t, texts[0], "tsk-aaaa1111")
		assert.NotContains(t, rec.body, "storyIds", "the canonical key translates, never rides untranslated")
		assert.Contains(t, rec.body, `"taskIds":["tsk-aaaa1111"]`,
			"the bulk reference rides the wire under the spelling the platform DTO binds")
	})

	t.Run("legacy taskIds carries the notice", func(t *testing.T) {
		api, _ := newAutomationAPI(t, 200, `[{"id":"tsk-aaaa1111","status":"done"}]`)
		setupRenameWorkspaceEnv(t, api)

		texts, isError := aliasWindowCall(t, "board_delete_stories", map[string]any{
			"taskIds": []any{"tsk-aaaa1111"}})
		require.False(t, isError)
		require.Len(t, texts, 2, "the legacy key spelling carries the deprecation notice")
		assert.Contains(t, texts[1], "DEPRECATED")
		assert.Contains(t, texts[1], "storyIds")
	})

	t.Run("alias name with canonical key lands too", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `[{"id":"tsk-aaaa1111","status":"done"}]`)
		setupRenameWorkspaceEnv(t, api)

		texts, isError := aliasWindowCall(t, "board_delete_tasks", map[string]any{
			"storyIds": []any{"tsk-aaaa1111"}})
		require.False(t, isError)
		require.Len(t, texts, 2, "the alias tool name carries its notice")
		assert.Contains(t, texts[1], "board_delete_stories")
		assert.Contains(t, rec.body, `"taskIds":["tsk-aaaa1111"]`,
			"canonicalization precedes the scope check — the alias path translates the same way")
	})
}

// TestBoardStoryAlias_LegacyKeyNoticeRidesErrors (review round 2's
// remaining unhappy tier, agentd side): the CANONICAL tool name with
// the LEGACY input key on a FAILING call still carries the key
// notice as the second content block.
func TestBoardStoryAlias_LegacyKeyNoticeRidesErrors(t *testing.T) {
	api, _ := newAutomationAPI(t, 409, `{"error":"task_paused","code":"task_paused"}`)
	setupRenameWorkspaceEnv(t, api)

	texts, isError := aliasWindowCall(t, "board_move_stage", map[string]any{
		"taskId": "tsk-paused-1", "toStage": "done"})
	assert.True(t, isError, "the platform guard renders an error result")
	require.Len(t, texts, 2, "the legacy-key notice rides the error envelope")
	assert.Contains(t, texts[1], "DEPRECATED")
	assert.Contains(t, texts[1], "storyId")
}

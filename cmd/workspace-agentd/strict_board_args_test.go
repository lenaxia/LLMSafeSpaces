// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// strict_board_args_test.go — #2030c pins:
//
//   - default OFF is byte-identical: no strict rejections, no
//     additionalProperties in the served board schemas;
//   - strict ON: unknown fields reject loudly with the recovery
//     teaching, identity-stamp emissions get the precise teaching
//     error, the platform's own stamp on stamp-set tools stays
//     silently admitted, and NO valid call is rejected (the catalog
//     sweep + the rename/window vocabularies);
//   - the schema transform preserves the raw-JSON key order (#1849
//     long-string-last discipline);
//   - the stamp-set mirror cannot drift from the origin plugin.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strictCallTool drives tools/call through the REAL handler and
// returns (result JSON, isError).
func strictCallTool(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	require.NoError(t, err)
	body, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 7, Method: "tools/call", Params: params})
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(body))
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	if resp.Error != nil {
		return resp.Error.Message, true
	}
	var sb strings.Builder
	for _, c := range resp.Result.Content {
		sb.WriteString(c.Text)
	}
	return sb.String(), resp.Result.IsError
}

// TestStrictBoardArgs_DefaultOffIsNoOp: with the flag unset (the
// rollout default) the strict layer rejects nothing and the served
// board schemas carry no additionalProperties — behavior byte-identical
// to the pre-#2030c surface.
func TestStrictBoardArgs_DefaultOffIsNoOp(t *testing.T) {
	// Pin the test's own premise: this is the FLAG-OFF posture, even
	// when the ambient environment carries the flag (the CI strict
	// sweep runs the whole suite with it set — this test stays honest
	// by clearing it).
	t.Setenv(boardStrictArgsFlag, "")
	t.Run("validator passes junk that today's surface accepts", func(t *testing.T) {
		require.False(t, boardStrictArgsEnabled())
		assert.NoError(t, strictBoardArgsCheck("board_list", "board_list", map[string]any{
			"projectId": "prj-x", "lsp_injected_session": "ses-1", "mauve": true,
		}))
		assert.NoError(t, strictBoardArgsCheck("board_read", "board_read", map[string]any{
			"lsp_injected_session": "ses-1",
		}))
	})
	t.Run("catalog carries no additionalProperties on board tools", func(t *testing.T) {
		for _, tool := range mcpToolCatalog() {
			if !strings.HasPrefix(tool.Name, "board_") {
				continue
			}
			assert.NotContains(t, strictSchemaString(tool.InputSchema), "additionalProperties",
				"flag-off catalog must stay byte-identical for %s", tool.Name)
		}
	})
	t.Run("strict transform is the identity when disabled", func(t *testing.T) {
		cat := mcpToolCatalog()
		assert.Equal(t, cat, strictBoardToolSchemas(cat))
	})
}

// TestStrictBoardArgs_UnknownFieldRejectedLoudly: an extra field on a
// board tool rejects with the field named, the admitted vocabulary
// listed, and the recovery path taught (re-emit without the field).
func TestStrictBoardArgs_UnknownFieldRejectedLoudly(t *testing.T) {
	t.Setenv(boardStrictArgsFlag, "1")
	out, isErr := strictCallTool(t, "board_epic_activate", map[string]any{
		"epicId": "epc-test", "color": "mauve",
	})
	require.True(t, isErr, "strict mode must reject the unknown field, got: %s", out)
	assert.Contains(t, out, `unknown field "color"`)
	assert.Contains(t, out, "re-emit the call without the extra field")
	assert.Contains(t, out, "epicId", "the message teaches the admitted vocabulary")
	assert.Contains(t, out, "board_epic_activate failed")
}

// TestStrictBoardArgs_IdentityTeachingError: emitted identity keys get
// the PRECISE teaching error — not the generic unknown-field one — on
// every surface where the key is not the platform's own stamp or the
// tool's declared param: worker tools (never stamped), tools without a
// sessionId param, and the legacy alias names (the live instance's
// 272-event emission class — the plugin never stamps an alias).
func TestStrictBoardArgs_IdentityTeachingError(t *testing.T) {
	t.Setenv(boardStrictArgsFlag, "1")
	t.Run("worker tool stamp emission (board_read is not in the stamp set)", func(t *testing.T) {
		out, isErr := strictCallTool(t, "board_read", map[string]any{
			"shortId": "tsk-x", "lsp_injected_session": "ses_e862c3aaaffeLG0vd96mumHD8u",
		})
		require.True(t, isErr, "got: %s", out)
		assert.Contains(t, out, "platform-injected identity stamp")
		assert.Contains(t, out, "never emit")
		assert.Contains(t, out, "re-emit the call without this field")
		assert.NotContains(t, out, "unknown field", "the identity class must not get the generic error")
	})
	t.Run("sessionId where the tool declares no such param", func(t *testing.T) {
		out, isErr := strictCallTool(t, "board_list", map[string]any{
			"projectId": "prj-x", "sessionId": "ses_e862c3aaaffeLG0vd96mumHD8u",
		})
		require.True(t, isErr, "got: %s", out)
		assert.Contains(t, out, "platform-injected identity stamp")
	})
	t.Run("legacy alias name carries the emission class (never plugin-stamped)", func(t *testing.T) {
		out, isErr := strictCallTool(t, "board_update_task", map[string]any{
			"storyId": "tsk-x", "lsp_injected_session": "ses_e862c3aaaffeLG0vd96mumHD8u",
		})
		require.True(t, isErr, "got: %s", out)
		assert.Contains(t, out, "platform-injected identity stamp")
		// The alias deprecation notice still rides the error envelope.
		assert.Contains(t, out, "DEPRECATED")
	})
}

// TestStrictBoardArgs_PlatformStampAndLegitParamsAdmitted: the calls
// that are valid TODAY must land identically under strict mode — the
// plugin's own stamp on stamp-set tools, sessionId where the schema
// declares it, and the rename's taskId spelling.
func TestStrictBoardArgs_PlatformStampAndLegitParamsAdmitted(t *testing.T) {
	t.Setenv(boardStrictArgsFlag, "1")
	api, rec := newAutomationAPI(t, 200, `{"ok":true}`)
	setupRenameWorkspaceEnv(t, api)

	t.Run("board_list with the platform stamp succeeds through the seam", func(t *testing.T) {
		out, isErr := strictCallTool(t, "board_list", map[string]any{
			"projectId": "prj-x", "lsp_injected_session": "ses-1",
		})
		require.False(t, isErr, "the platform's own stamp must stay admitted, got: %s", out)
		assert.Contains(t, rec.path, "/board/")
	})
	t.Run("board_heartbeat's declared sessionId param stays a normal arg", func(t *testing.T) {
		_, isErr := strictCallTool(t, "board_heartbeat", map[string]any{
			"claimToken": "tok", "sessionId": "ses-1", "shortId": "tsk-x",
		})
		require.False(t, isErr, "a declared sessionId param is not an identity emission")
	})
	t.Run("rename legacy taskId spelling stays admitted", func(t *testing.T) {
		assert.NoError(t, strictBoardArgsCheck("board_delete_story", "board_delete_story",
			map[string]any{"taskId": "tsk-x", "reconnect": true}))
		assert.NoError(t, strictBoardArgsCheck("board_delete_story", "board_delete_story",
			map[string]any{"storyId": "tsk-x"}))
	})
	t.Run("non-board tools stay tolerant under strict mode", func(t *testing.T) {
		// The mode is scoped to board tools: a trigger_create carrying
		// an extra field passes the strict layer untouched (the wire's
		// own validation owns it) — pinned through the real handler.
		out, isErr := strictCallTool(t, "trigger_list", map[string]any{"mauve": true})
		require.False(t, isErr, "strict mode must not touch non-board tools, got: %s", out)
		assert.NoError(t, strictBoardArgsCheck("trigger_list", "trigger_list",
			map[string]any{"mauve": true}))
	})
}

// TestStrictBoardArgs_NoValidCallRejected: the catalog sweep — for
// EVERY board tool in the catalog, a call built from the tool's own
// advertised properties (plus the platform stamp where the plugin adds
// one) passes the strict check. This is the pin that strict mode
// rejects no valid call across the full tool-contract surface.
func TestStrictBoardArgs_NoValidCallRejected(t *testing.T) {
	t.Setenv(boardStrictArgsFlag, "1")
	for _, tool := range mcpToolCatalog() {
		if !strings.HasPrefix(tool.Name, "board_") {
			continue
		}
		args := map[string]any{}
		for prop, spec := range boardSchemaProperties(tool.InputSchema) {
			args[prop] = strictPlaceholderValue(spec)
		}
		if boardStampSetTools[tool.Name] {
			args["lsp_injected_session"] = "ses-1"
		}
		assert.NoErrorf(t, strictBoardArgsCheck(tool.Name, tool.Name, args),
			"%s: schema-derived args must never be rejected", tool.Name)
	}
	// The tolerated envelope shapes: nested wrappers and requestId on
	// non-file tools ride today's surface and must stay valid.
	assert.NoError(t, strictBoardArgsCheck("board_epic_activate", "board_epic_activate",
		map[string]any{"epic": map[string]any{"epicId": "epc-x"}}))
	assert.NoError(t, strictBoardArgsCheck("board_epic_activate", "board_epic_activate",
		map[string]any{"epicId": "epc-x", "requestId": "req-1"}))
}

// strictSchemaString renders one InputSchema as a searchable string —
// json.RawMessage must stringify (its %v form is a byte-slice dump).
func strictSchemaString(schema any) string {
	if raw, ok := schema.(json.RawMessage); ok {
		return string(raw)
	}
	return fmt.Sprintf("%v", schema)
}

// strictPlaceholderValue builds a present (type-plausible) value for a
// schema property — the sweep only needs the KEY present; type
// validation stays the dispatch/platform's job (today's contract).
func strictPlaceholderValue(spec any) any {
	if m, ok := spec.(map[string]any); ok {
		switch m["type"] {
		case "string":
			return "x"
		case "integer", "number":
			return 1
		case "boolean":
			return true
		case "array":
			return []any{}
		case "object":
			return map[string]any{}
		}
	}
	return "x"
}

// TestStrictBoardArgs_CatalogStrictTransform: with the flag on, every
// board tool's served schema carries additionalProperties:false, no
// non-board tool's schema changes, and raw-JSON schemas keep their
// deliberate key order byte-for-byte after the spliced prefix (#1849
// long-string-last discipline).
func TestStrictBoardArgs_CatalogStrictTransform(t *testing.T) {
	t.Run("flag on stamps every board tool, touches nothing else", func(t *testing.T) {
		t.Setenv(boardStrictArgsFlag, "1")
		tools := mcpToolCatalog()
		require.NotEmpty(t, tools)
		boardSeen, otherSeen := 0, 0
		for _, tool := range tools {
			props := strictSchemaString(tool.InputSchema)
			if strings.HasPrefix(tool.Name, "board_") {
				boardSeen++
				assert.Contains(t, props, "additionalProperties", tool.Name)
			} else {
				otherSeen++
				assert.NotContains(t, props, "additionalProperties", tool.Name)
			}
		}
		assert.Positive(t, boardSeen)
		assert.Positive(t, otherSeen)
	})
	t.Run("raw splice preserves the original bytes after the prefix", func(t *testing.T) {
		base := boardManagerTools()
		t.Setenv(boardStrictArgsFlag, "1")
		strict := mcpToolCatalog()
		byName := map[string]mcpTool{}
		for _, tool := range strict {
			byName[tool.Name] = tool
		}
		rawChecked := 0
		for _, tool := range base {
			raw, ok := tool.InputSchema.(json.RawMessage)
			if !ok {
				continue
			}
			got, ok := byName[tool.Name].InputSchema.(json.RawMessage)
			require.True(t, ok, "%s: raw schema must stay raw", tool.Name)
			prefix := `{"additionalProperties":false,`
			require.True(t, strings.HasPrefix(string(got), prefix), tool.Name)
			assert.Equal(t, string(raw)[1:], string(got)[len(prefix):],
				"%s: bytes after the spliced prefix must be untouched (#1849 order)", tool.Name)
			rawChecked++
		}
		assert.Positive(t, rawChecked, "the catalog has ordered raw schemas to cover")
	})
	t.Run("splice refuses a schema that already carries the key", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)
		assert.Equal(t, raw, spliceAdditionalPropertiesFalse(raw))
	})
}

// TestStrictBoardArgs_PluginSetParity: boardStampSetTools must equal
// the origin plugin's INJECTED_SESSION_TOOLS (bare-name form) exactly
// — the silently-admitted stamp set cannot drift from the plugin that
// writes the stamp.
func TestStrictBoardArgs_PluginSetParity(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?s)INJECTED_SESSION_TOOLS\s*=\s*new Set\(\[(.*?)\]`).FindSubmatch(src)
	require.NotNil(t, block, "the plugin's stamp set must stay parseable")
	names := regexp.MustCompile(`"(llmsafespaces_[a-z0-9_]+)"`).FindAllStringSubmatch(string(block[1]), -1)
	require.NotEmpty(t, names)
	plugin := map[string]bool{}
	for _, m := range names {
		plugin[strings.TrimPrefix(m[1], "llmsafespaces_")] = true
	}
	assert.Equal(t, plugin, boardStampSetTools,
		"the Go mirror and the plugin stamp set must match in both directions")
}

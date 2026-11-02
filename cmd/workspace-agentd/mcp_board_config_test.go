// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// mcp_board_config_test.go — board_get_config / board_set_config over
// the real mcpHandler surface: the catalog carries both tools (the
// read annotated open, the set carrying the file-param contract), the
// read routes body-less with the pod's stamps, the set rides the
// configFile discipline (design 0070: payload file only, inline
// config keys refused, stray file keys refused, the consumed file
// deleted after success and kept after failure), and
// board_validate_file's set_config op pins the config schema's
// findings.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPBoardConfig_Catalog(t *testing.T) {
	tools := mcpToolsList(t)

	get, ok := tools["board_get_config"]
	require.True(t, ok, "board_get_config registered")
	// Read-open voice: the readOnly annotation, no required args.
	ann := get["annotations"].(map[string]any)
	assert.Equal(t, true, ann["readOnlyHint"], "a read, annotated as one")
	schema := get["inputSchema"].(map[string]any)
	if req, ok := schema["required"]; ok {
		assert.Empty(t, req, "no required arguments on the read")
	}

	set, ok := tools["board_set_config"]
	require.True(t, ok, "board_set_config registered")
	sschema := set["inputSchema"].(map[string]any)
	require.JSONEq(t, `["tier","configFile"]`, mustJSON(t, sschema["required"]),
		"the set requires the tier selector and the payload file")
	props := sschema["properties"].(map[string]any)
	_, hasFile := props["configFile"]
	assert.True(t, hasFile, "the payload file param is in the schema")
	for _, inline := range []string{"reviewColumn", "autoAssignReviewer", "defaultWorkerSlots", "stagePersonas"} {
		_, present := props[inline]
		assert.False(t, present, "config key %q must NOT ride inline in the schema (file-only payload)", inline)
	}
}

func TestMCPBoardConfig_GetWire(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"scope":"user","sources":{},"personas":{}}`)
	setupRenameWorkspaceEnv(t, api)

	t.Run("user tier: body-less GET with the pod stamps, verbatim reply", func(t *testing.T) {
		rec.method, rec.path, rec.query = "", "", ""
		out, err := callMCPTool(context.Background(), mcpTestPassword, "board_get_config", nil)
		require.NoError(t, err)
		assert.Contains(t, out, `"scope":"user"`, "platform body passes through verbatim")
		assert.Equal(t, http.MethodGet, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/config", rec.path)
		assert.Equal(t, "workspaceID=ws-1", rec.query)
		assert.Empty(t, rec.body, "a read carries no body")
	})

	t.Run("project tier: the projectId rides the query", func(t *testing.T) {
		rec.method, rec.path, rec.query = "", "", ""
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_get_config", map[string]any{
			"projectId": "prj-aaaa1111",
		})
		require.NoError(t, err)
		assert.Contains(t, rec.query, "workspaceID=ws-1")
		assert.Contains(t, rec.query, "projectId=prj-aaaa1111")
	})

	t.Run("the caller's session rides as audit attribution", func(t *testing.T) {
		rec.method, rec.path, rec.query = "", "", ""
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_get_config", map[string]any{
			"lsp_injected_session": "ses_collab1",
		})
		require.NoError(t, err)
		assert.Contains(t, rec.query, "sessionId=ses_collab1")
	})
}

func TestMCPBoardConfig_SetWire(t *testing.T) {
	t.Run("tier=user: the file's config keys ride the wire verbatim; the consumed file is deleted", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"scope":"user"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "cfg.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"reviewColumn":"manual","stagePersonas":{"in_review":"reviewer"}}`), 0o600))

		out, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": path, "lsp_injected_session": "ses_collab1",
		})
		require.NoError(t, err)
		assert.Contains(t, out, `"scope":"user"`)
		assert.Equal(t, http.MethodPut, rec.method)
		assert.Equal(t, "/internal/v1/automation/board/config", rec.path)

		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.Equal(t, `"user"`, string(wire["tier"]))
		assert.Equal(t, `"manual"`, string(wire["reviewColumn"]), "the file's fields pass through verbatim")
		require.NotNil(t, wire["stagePersonas"])
		assert.Contains(t, string(wire["stagePersonas"]), "reviewer")
		assert.Equal(t, `"ws-1"`, string(wire["workspaceID"]), "the resolver stamp rides")
		assert.Equal(t, `"ses_collab1"`, string(wire["sessionId"]), "the caller's session rides the audit attribution")
		_, hasFile := wire["configFile"]
		assert.False(t, hasFile, "the file param never reaches the wire")

		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "the consumed payload file is deleted after success (design 0070 §2.4)")
	})

	t.Run("tier=project: projectId rides the wire body", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"scope":"project"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "cfg.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"autoAssignReviewer":false}`), 0o600))

		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "project", "projectId": "prj-aaaa1111", "configFile": path,
		})
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.Equal(t, `"prj-aaaa1111"`, string(wire["projectId"]))
		assert.Equal(t, "false", string(wire["autoAssignReviewer"]))
	})

	t.Run("a failed set KEEPS the file (the write never landed)", func(t *testing.T) {
		api, _ := newAutomationAPI(t, 500, `{"error":"boom"}`)
		setupRenameWorkspaceEnv(t, api)

		path := filepath.Join(t.TempDir(), "cfg.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"reviewColumn":"manual"}`), 0o600))

		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": path,
		})
		require.Error(t, err)
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "cleanup runs only on success")
	})
}

func TestMCPBoardConfig_SetRefusals(t *testing.T) {
	api, _ := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)

	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cfg.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	t.Run("config keys never ride inline", func(t *testing.T) {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": writeCfg(t, `{}`), "reviewColumn": "manual",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no longer rides inline")
		assert.Contains(t, err.Error(), "configFile")
	})

	t.Run("configFile is required", func(t *testing.T) {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "configFile is required")
	})

	t.Run("stray file params refuse loudly", func(t *testing.T) {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": writeCfg(t, `{}`), "bodyFile": "/tmp/x.json",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bodyFile is not admitted here")
	})

	t.Run("routing keys refuse INSIDE the file (placement: inline-only)", func(t *testing.T) {
		path := writeCfg(t, `{"tier":"user","reviewColumn":"manual"}`)
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": path,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inline-only")
		assert.Contains(t, err.Error(), "set_config")
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "a locally-refused file SURVIVES — the caller fixes and re-submits it")
	})

	t.Run("unknown config keys refuse via the local schema gate", func(t *testing.T) {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_set_config", map[string]any{
			"tier": "user", "configFile": writeCfg(t, `{"reviewColumn":"manual","bogusKey":1}`),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "local validation")
		assert.Contains(t, err.Error(), "bogusKey")
	})
}

func TestMCPBoardConfig_ValidateFile(t *testing.T) {
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cfg.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	run := func(t *testing.T, content string) (string, string) {
		t.Helper()
		path := writeCfg(t, content)
		out, err := callMCPTool(context.Background(), mcpTestPassword, "board_validate_file", map[string]any{
			"op": "set_config", "file": path,
		})
		require.NoError(t, err)
		return out, path
	}

	t.Run("happy paths: full map, empty reset, absent keys", func(t *testing.T) {
		for _, content := range []string{
			`{"reviewColumn":"manual","autoAssignReviewer":false,"defaultWorkerSlots":3,"stagePersonas":{"in_review":"reviewer"}}`,
			`{}`,
		} {
			out, path := run(t, content)
			assert.Contains(t, out, `"valid":true`, content)
			assert.NotContains(t, out, `"severity":"error"`, content)
			_, statErr := os.Stat(path)
			assert.NoError(t, statErr, "validate NEVER deletes the file")
		}
	})

	t.Run("the suppression advisory: an explicit map without in_review warns (never blocks)", func(t *testing.T) {
		out, _ := run(t, `{"stagePersonas":{"in_progress":"build"}}`)
		assert.Contains(t, out, `"valid":true`, "a warning never blocks")
		assert.Contains(t, out, "reviewer-suppression")
		assert.Contains(t, out, "in_review")
	})

	t.Run("defaultWorkerSlots warns (user-level key)", func(t *testing.T) {
		out, _ := run(t, `{"defaultWorkerSlots":3}`)
		assert.Contains(t, out, `"valid":true`)
		assert.Contains(t, out, "tier-scope")
		assert.Contains(t, out, "USER-level key")
	})

	t.Run("error findings: unknown key, bad enum, non-eligible stage, nameless persona, mistyped value", func(t *testing.T) {
		for _, tc := range []struct {
			content string
			want    string
		}{
			{`{"bogus":true}`, "bogus"},
			{`{"reviewColumn":"sideways"}`, "reviewColumn"},
			{`{"stagePersonas":{"backlog":{"name":"x"}}}`, "backlog"},
			{`{"stagePersonas":{"in_review":{}}}`, "in_review"},
			{`{"autoAssignReviewer":"yes"}`, "autoAssignReviewer"},
			{`{"defaultWorkerSlots":0}`, "defaultWorkerSlots"},
		} {
			out, _ := run(t, tc.content)
			assert.Contains(t, out, `"valid":false`, tc.content)
			assert.Contains(t, out, `"severity":"error"`, tc.content)
			assert.True(t, strings.Contains(out, tc.want), "%s: findings should name %q in %s", tc.content, tc.want, out)
		}
	})

	t.Run("unknown op names the vocabulary", func(t *testing.T) {
		_, err := callMCPTool(context.Background(), mcpTestPassword, "board_validate_file", map[string]any{
			"op": "set_board", "file": writeCfg(t, `{}`),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "set_config")
	})
}

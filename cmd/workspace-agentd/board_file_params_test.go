// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_file_params_test.go — the file-crud cutover's resolver and
// dispatch contract (design 0070): payload files are the ONLY payload
// path on the five payload-bearing tools (inline payload args refused;
// descriptionFile/outcomeFile deleted), read under the #1837
// discipline, strict-decoded, validated by the shared validator,
// merged for the verbatim wire body, and deleted after success.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boardWritePayloadFile writes one JSON payload file and returns its path.
func boardWritePayloadFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// --- resolver discipline ------------------------------------------------------

func TestBoardFileParams_ByteIdenticalRoundTrip(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 120; i++ {
		b.WriteString("## Section ")
		b.WriteString(strings.Repeat("alpha ", i%5+1))
		b.WriteString("\nDo the distinct thing numbered ")
		b.WriteString(strings.Repeat("beta", i%3+1))
		b.WriteString(".\n")
	}
	charter := b.String()
	require.Greater(t, len(charter), 4096, "the fixture must be >4KB")

	payload := `{"title":"T-file","description":` + jsonMustMarshal(t, charter) +
		`,"acceptanceCriteria":[{"text":"the file's bytes land byte-identical","met":false}]}`
	path := boardWritePayloadFile(t, t.TempDir(), "task.json", payload)

	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-file0001"}`)
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "bodyFile": path,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "tsk-file0001")

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.JSONEq(t, jsonMustMarshal(t, charter), string(wire["description"]),
		"the file's content reaches the wire verbatim as description")
	assert.JSONEq(t, `"T-file"`, string(wire["title"]), "the payload's fields merge into the wire body")
	assert.NotContains(t, wire, "bodyFile", "the file param is consumed, never forwarded")
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", rec.path)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "the consumed payload file is deleted after the successful create")
}

func TestBoardFileParams_ReadDiscipline(t *testing.T) {
	newArgs := func(path any) map[string]any {
		return map[string]any{"epicId": "epc-bbbb2222", "bodyFile": path, "lsp_injected_session": "ses-1"}
	}
	call := func(args map[string]any) error {
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", args)
		assert.Empty(t, rec.method, "no wire call may happen for a refused file")
		return err
	}

	t.Run("missing file carries the cleanup hint", func(t *testing.T) {
		err := call(newArgs(filepath.Join(t.TempDir(), "absent.json")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "file not found")
		assert.Contains(t, err.Error(), "re-write it; payloads are file-only")
		assert.Contains(t, err.Error(), "board_list", "the retry hint points at confirming the landing")
	})

	t.Run("FIFO refused", func(t *testing.T) {
		dir := t.TempDir()
		fifo := filepath.Join(dir, "fifo")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))
		err := call(newArgs(fifo))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})

	t.Run("over the 256 KiB payload cap", func(t *testing.T) {
		big := `{"description":"` + strings.Repeat("a", boardPayloadFileMaxBytes+2) + `"}`
		path := boardWritePayloadFile(t, t.TempDir(), "big.json", big)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cap")
	})

	t.Run("non-UTF-8 refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin.json")
		require.NoError(t, os.WriteFile(path, []byte("{\"d\":\"ok\x80\xff\xfe\"}"), 0o600))
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "UTF-8")
	})

	t.Run("duplicate keys refused", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "dup.json",
			`{"title":"a","title":"b","description":"d","acceptanceCriteria":[{"text":"c"}]}`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `duplicate key "title"`)
	})

	t.Run("duplicate keys refused at nested depth", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "dupn.json",
			`{"title":"a","description":"d","acceptanceCriteria":[{"text":"c","text":"c"}]}`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate key")
	})

	t.Run("trailing bytes refused (single document)", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "trail.json", `{"title":"a"} {"title":"b"}`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "trailing bytes")
	})

	t.Run("top level must be an object", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "arr.json", `[{"title":"a"}]`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "object")
	})

	t.Run("nested file param refused (one indirection level)", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "nested.json",
			`{"title":"a","description":"d","acceptanceCriteria":[{"text":"c"}],"bodyFile":"/other.json"}`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "file-param")
		assert.Contains(t, err.Error(), "one indirection level")
	})

	t.Run("routing key inside the file refused (stays inline)", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "routing.json",
			`{"epicId":"epc-bbbb2222","title":"a","description":"d","acceptanceCriteria":[{"text":"c"}]}`)
		err := call(newArgs(path))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inline-only")
	})
}

// --- the cutover's core guarantees ---------------------------------------------

// TestBoardFileParams_InlinePayloadRefused: on every payload-bearing
// tool, an inline payload key is refused with the file-key pointer —
// and the payload file is required when it is absent. The unsafe shape
// is unexpressible, not merely discouraged.
func TestBoardFileParams_InlinePayloadRefused(t *testing.T) {
	cases := []struct {
		tool        string
		routing     map[string]any
		inlineKey   string
		inlineValue any
		fileKey     string
	}{
		{"board_create_task", map[string]any{"epicId": "epc-bbbb2222"}, "title", "T", "bodyFile"},
		{"board_create_epic", map[string]any{"projectId": "prj-aaaa1111"}, "title", "E", "bodyFile"},
		{"board_update_task", map[string]any{"taskId": "tsk-cccc3333"}, "description", "d", "patchFile"},
		{"board_update_epic", map[string]any{"epicId": "epc-bbbb2222"}, "outcome", "o", "patchFile"},
		{"board_import_plan", map[string]any{"projectId": "prj-aaaa1111"}, "tasks", []any{}, "planFile"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" inline "+tc.inlineKey, func(t *testing.T) {
			api, rec := newAutomationAPI(t, 200, `{}`)
			setupRenameWorkspaceEnv(t, api)
			args := map[string]any{"lsp_injected_session": "ses-1", tc.inlineKey: tc.inlineValue}
			for k, v := range tc.routing {
				args[k] = v
			}
			_, err := mcpAutomation(context.Background(), mcpTestPassword, tc.tool, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no longer rides inline")
			assert.Contains(t, err.Error(), tc.fileKey, "the refusal names the file key")
			assert.Contains(t, err.Error(), "board_validate_file", "the refusal teaches the workflow")
			assert.Empty(t, rec.method, "no wire call for an inline payload")
		})
		t.Run(tc.tool+" file required", func(t *testing.T) {
			api, rec := newAutomationAPI(t, 200, `{}`)
			setupRenameWorkspaceEnv(t, api)
			args := map[string]any{"lsp_injected_session": "ses-1"}
			for k, v := range tc.routing {
				args[k] = v
			}
			_, err := mcpAutomation(context.Background(), mcpTestPassword, tc.tool, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.fileKey+" is required")
			assert.Contains(t, err.Error(), "board_validate_file")
			assert.Empty(t, rec.method)
		})
	}
}

// TestBoardFileParams_RoutingAndControlPlaneStayInline: the inline
// vocabulary on the five tools is routing + control-plane + identity —
// exactly those keys pass alongside the payload file.
func TestBoardFileParams_RoutingAndControlPlaneStayInline(t *testing.T) {
	t.Run("control-plane setCriteriaMet without patchFile", func(t *testing.T) {
		api, rec := newAutomationAPI(t, 200, `{"id":"t-1"}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
			"taskId": "tsk-aaaa1111", "setCriteriaMet": map[string]any{"index": 0, "met": true},
			"lsp_injected_session": "ses-1"})
		require.NoError(t, err, "the bare criteria check-off is control-plane — no patch file needed")
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `{"index":0,"met":true}`, string(wire["setCriteriaMet"]))
	})

	t.Run("dryRun rides inline with planFile", func(t *testing.T) {
		payload := `{"epic":{"title":"E"},"tasks":[{"key":"k1","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)
		api, rec := newAutomationAPI(t, 200, `{"dry_run":true}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "dryRun": true, "planFile": path,
			"lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `true`, string(wire["dryRun"]), "the control-plane flag rides inline, verbatim")
		assert.NotContains(t, wire, "planFile")
		_, statErr := os.Stat(path)
		assert.False(t, os.IsNotExist(statErr), "a dry run keeps the plan file for the real submission")
	})
}

// TestBoardFileParams_StrayFileKeyRefusedEverywhere: a file-suffixed
// key the tool does not admit — including the DELETED descriptionFile/
// outcomeFile spellings — is refused loudly on every manager tool,
// never silently dropped onto the wire.
func TestBoardFileParams_StrayFileKeyRefusedEverywhere(t *testing.T) {
	strays := []struct {
		tool string
		key  string
	}{
		{"board_create_project", "name_file"},
		{"board_create_project", "bodyFile"},
		{"board_create_task", "descriptionFile"},
		{"board_create_task", "description_file"},
		{"board_create_epic", "outcomeFile"},
		{"board_update_epic", "outcome_file"},
		{"board_import_plan", "bodyFile"},
		{"board_pause_task", "patchFile"},
		{"board_list", "plan_file"},
	}
	for _, s := range strays {
		t.Run(s.tool+" "+s.key, func(t *testing.T) {
			api, rec := newAutomationAPI(t, 200, `{}`)
			setupRenameWorkspaceEnv(t, api)
			args := map[string]any{s.key: "/tmp/x.json", "lsp_injected_session": "ses-1"}
			// routing/scalars so refusals come from the stray guard, not missing ids
			switch s.tool {
			case "board_create_project", "board_import_plan", "board_list":
				args["projectId"] = "prj-aaaa1111"
			case "board_create_task":
				args["epicId"] = "epc-bbbb2222"
			case "board_create_epic", "board_update_epic":
				args["epicId"] = "epc-bbbb2222"
			case "board_update_task", "board_pause_task":
				args["taskId"] = "tsk-cccc3333"
			}
			if s.tool == "board_create_project" {
				args["name"] = "P"
			}
			_, err := mcpAutomation(context.Background(), mcpTestPassword, s.tool, args)
			require.Error(t, err, "%s on %s is a caller mistake, refused loudly", s.key, s.tool)
			assert.Contains(t, err.Error(), "not admitted")
			assert.Empty(t, rec.method)
		})
	}
}

// TestBoardFileParams_PathAnchoring: relative file paths resolve
// against the WORKSPACE ROOT (the PVC mount), never agentd's process
// cwd — the live failure class (design 0070 §2.2).
func TestBoardFileParams_PathAnchoring(t *testing.T) {
	root := t.TempDir()
	orig := boardFileWorkspaceRoot
	boardFileWorkspaceRoot = root
	t.Cleanup(func() { boardFileWorkspaceRoot = orig })

	require.NoError(t, os.MkdirAll(filepath.Join(root, "reports", "plans"), 0o755))
	payload := `{"title":"Anchored","description":"resolved against the workspace root","acceptanceCriteria":[{"text":"c"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(root, "reports", "plans", "p1.json"), []byte(payload), 0o600))

	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-anchor001"}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "bodyFile": "reports/plans/p1.json", // RELATIVE
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err, "the relative path resolves against the workspace root, not the cwd")
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.JSONEq(t, `"Anchored"`, string(wire["title"]))
}

// --- cleanup hook (§2.4) --------------------------------------------------------

func TestBoardFileParams_Cleanup(t *testing.T) {
	taskPayload := `{"title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}`
	patchPayload := `{"title":"T2"}`
	epicPayload := `{"title":"E"}`

	t.Run("success deletes (all three keys)", func(t *testing.T) {
		for _, tc := range []struct{ tool, fileKey, payload string }{
			{"board_create_task", "bodyFile", taskPayload},
			{"board_create_epic", "bodyFile", epicPayload},
			{"board_update_epic", "patchFile", patchPayload},
			{"board_update_task", "patchFile", patchPayload},
			{"board_import_plan", "planFile", `{"epic":{"title":"E"},"tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`},
		} {
			path := boardWritePayloadFile(t, t.TempDir(), "p.json", tc.payload)
			api, _ := newAutomationAPI(t, 200, `{"short_id":"x"}`)
			setupRenameWorkspaceEnv(t, api)
			args := map[string]any{tc.fileKey: path, "lsp_injected_session": "ses-1"}
			switch tc.tool {
			case "board_create_task":
				args["epicId"] = "epc-bbbb2222"
			case "board_create_epic", "board_import_plan":
				args["projectId"] = "prj-aaaa1111"
			case "board_update_epic":
				args["epicId"] = "epc-bbbb2222"
			case "board_update_task":
				args["taskId"] = "tsk-cccc3333"
			}
			_, err := mcpAutomation(context.Background(), mcpTestPassword, tc.tool, args)
			require.NoError(t, err)
			_, statErr := os.Stat(path)
			assert.True(t, os.IsNotExist(statErr), "%s: consumed file deleted after success", tc.tool)
		}
	})

	t.Run("wire failure keeps the file", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "p.json", taskPayload)
		api, _ := newAutomationAPI(t, http.StatusConflict, `{"error":"guard"}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
			"epicId": "epc-bbbb2222", "bodyFile": path, "lsp_injected_session": "ses-1"})
		require.Error(t, err)
		_, statErr := os.Stat(path)
		assert.False(t, os.IsNotExist(statErr), "a refused call leaves the file for the retry")
	})

	t.Run("validation refusal keeps the file", func(t *testing.T) {
		path := boardWritePayloadFile(t, t.TempDir(), "p.json", `{"title":"T"}`) // seedless
		api, _ := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
			"epicId": "epc-bbbb2222", "bodyFile": path, "lsp_injected_session": "ses-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "local validation")
		_, statErr := os.Stat(path)
		assert.False(t, os.IsNotExist(statErr), "a locally refused call leaves the file")
	})

	t.Run("import_plan dryRun keeps the file; the real run deletes it", func(t *testing.T) {
		payload := `{"epic":{"title":"E"},"tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)

		api, _ := newAutomationAPI(t, 200, `{"dry_run":true}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "dryRun": true, "planFile": path, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		_, statErr := os.Stat(path)
		assert.False(t, os.IsNotExist(statErr), "dry run keeps the file")

		boardWritePayloadFile(t, filepath.Dir(path), "plan.json", payload) // refresh (dry run kept it; rewrite for the real run anyway)
		api2, _ := newAutomationAPI(t, 200, `{"dry_run":false}`)
		setupRenameWorkspaceEnv(t, api2)
		_, err = mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		_, statErr = os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "the real run deletes the file")
	})

	t.Run("delete failure warns without failing the call", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "p.json")
		require.NoError(t, os.WriteFile(path, []byte(taskPayload), 0o600))
		// During the wire call (after resolution, before cleanup), swap
		// the file for a NON-EMPTY directory: os.Remove refuses it.
		api, _ := newAutomationAPI(t, 200, `{"short_id":"tsk-cleanup01"}`)
		api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.Body.Close()
			_ = os.Remove(path)
			_ = os.MkdirAll(filepath.Join(path, "child"), 0o755)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"short_id":"tsk-cleanup01"}`))
		})
		setupRenameWorkspaceEnv(t, api)
		out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
			"epicId": "epc-bbbb2222", "bodyFile": path, "lsp_injected_session": "ses-1"})
		require.NoError(t, err, "the call still succeeds — a delete failure only warns")
		assert.Contains(t, out, "cleanup_warning")
		assert.Contains(t, out, "could not be deleted")
	})
}

// TestBoardFileParams_UnifiedPlanDoc: the planFile document covers
// both §1a arms; the wire body carries the file's fields verbatim
// (epic object included — the platform binds it).
func TestBoardFileParams_UnifiedPlanDoc(t *testing.T) {
	t.Run("new-epic arm", func(t *testing.T) {
		payload := `{"epic":{"title":"E2E epic","outcome":"ship it","tags":["alpha"]},` +
			`"tasks":[{"key":"a","title":"A","description":"da","acceptanceCriteria":[{"text":"ca"}],` +
			`"dependsOn":["b"]},{"key":"b","title":"B","description":"db","acceptanceCriteria":[{"text":"cb"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)
		api, rec := newAutomationAPI(t, 200, `{"epic_id":"e-1"}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `{"title":"E2E epic","outcome":"ship it","tags":["alpha"]}`, string(wire["epic"]),
			"the epic object rides the wire verbatim (the platform binds it)")
		assert.NotContains(t, wire, "planFile")
	})

	t.Run("existing-epic arm", func(t *testing.T) {
		payload := `{"epicId":"epc-bbbb2222","tasks":[{"key":"only","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)
		api, rec := newAutomationAPI(t, 200, `{"epic_id":"e-1"}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path, "lsp_injected_session": "ses-1"})
		require.NoError(t, err)
		var wire map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
		assert.JSONEq(t, `"epc-bbbb2222"`, string(wire["epicId"]))
	})

	t.Run("both arms refused locally", func(t *testing.T) {
		payload := `{"epic":{"title":"E"},"epicId":"epc-bbbb2222","tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)
		api, rec := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path, "lsp_injected_session": "ses-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exactly-one-of")
		assert.Empty(t, rec.method, "no wire call for a locally refused plan")
	})

	t.Run("neither arm refused locally", func(t *testing.T) {
		payload := `{"tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`
		path := boardWritePayloadFile(t, t.TempDir(), "plan.json", payload)
		api, _ := newAutomationAPI(t, 200, `{}`)
		setupRenameWorkspaceEnv(t, api)
		_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_import_plan", map[string]any{
			"projectId": "prj-aaaa1111", "planFile": path, "lsp_injected_session": "ses-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exactly-one-of")
	})
}

func jsonMustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

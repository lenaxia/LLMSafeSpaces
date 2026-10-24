// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_file_params_test.go — #1837 P1a: file-backed long fields on the
// agent manager surface. The emission-corruption class (#1832 dogfood)
// lives in >2KB INLINE string arguments; a workspace file path is a
// short argument whose CONTENT the server stores verbatim. Red-first:
// this file landed BEFORE resolveBoardFileArgs.

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

// TestBoardFileParams_ByteIdenticalRoundTrip is the acceptance-bar
// fixture: a >4KB charter via file-reference lands byte-identical on
// the wire (the platform stores what the wire carries).
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

	path := filepath.Join(t.TempDir(), "charter.md")
	require.NoError(t, os.WriteFile(path, []byte(charter), 0o600))

	args := map[string]any{"descriptionFile": path}
	require.NoError(t, resolveBoardFileArgs(args, "description"))
	require.Equal(t, charter, args["description"], "the resolved description must be byte-identical to the file")
	assert.NotContains(t, args, "descriptionFile", "the file param is consumed, never forwarded")
}

func TestBoardFileParams_OutcomeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.md")
	require.NoError(t, os.WriteFile(path, []byte("steer"), 0o600))
	args := map[string]any{"outcomeFile": path}
	require.NoError(t, resolveBoardFileArgs(args, "outcome"))
	require.Equal(t, "steer", args["outcome"])
}

func TestBoardFileParams_AmbiguousInlineAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.md")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))
	args := map[string]any{"descriptionFile": path, "description": "inline"}
	err := resolveBoardFileArgs(args, "description")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one or the other")
}

func TestBoardFileParams_FileForWrongField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	args := map[string]any{"descriptionFile": path}
	err := resolveBoardFileArgs(args, "outcome")
	require.Error(t, err, "a descriptionFile on an outcome-field tool is a caller mistake, refused loudly")
}

func TestBoardFileParams_CapAndShapeGuards(t *testing.T) {
	t.Run("over cap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big.md")
		require.NoError(t, os.WriteFile(path, make([]byte, boardFileFieldMaxBytes+1), 0o600))
		args := map[string]any{"descriptionFile": path}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cap")
	})

	t.Run("FIFO refused", func(t *testing.T) {
		dir := t.TempDir()
		fifo := filepath.Join(dir, "fifo")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))
		args := map[string]any{"descriptionFile": fifo}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})

	t.Run("missing file", func(t *testing.T) {
		args := map[string]any{"descriptionFile": filepath.Join(t.TempDir(), "absent.md")}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
	})

	t.Run("non-UTF-8 refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin.md")
		require.NoError(t, os.WriteFile(path, []byte("ok\x80\xff\xfe"), 0o600))
		args := map[string]any{"descriptionFile": path}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "UTF-8")
	})
}

// TestBoardFileParams_WireContract: the dispatch layer resolves the
// file param BEFORE the wire call — the platform receives an inline
// description and never sees the _file spelling.
func TestBoardFileParams_WireContract(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-file0001"}`)
	setupRenameWorkspaceEnv(t, api)

	path := filepath.Join(t.TempDir(), "charter.md")
	content := strings.Repeat("distinct charter line with payload ", 40)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "title": "T-file", "descriptionFile": path,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "tsk-file0001")

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.JSONEq(t, jsonMustString(t, content), string(wire["description"]),
		"the file's content reaches the wire verbatim as description")
	assert.NotContains(t, wire, "descriptionFile")
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", rec.path)
}

func jsonMustString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

func TestBoardFileParams_NullWrongFieldRefused(t *testing.T) {
	// #1837 review round 1: a null *_file key on a tool that does not
	// admit it is still a mention — refused loudly, never silently
	// dropped onto the wire.
	args := map[string]any{"descriptionFile": nil}
	err := resolveBoardFileArgs(args, "outcome")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not admitted")
}

func TestBoardFileParams_NoFileToolRefusesAnyFileKey(t *testing.T) {
	// #1837 review round 2: a *_file key on a manager tool that admits
	// NO file param (project create) is refused loudly pre-wire.
	api, _ := newAutomationAPI(t, 200, `{}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_project", map[string]any{
		"name": "P", "name_file": "/tmp/x.md",
		"lsp_injected_session": "ses-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not admitted")
}

func TestBoardFileParams_NullAdmittedNeverReachesWire(t *testing.T) {
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-n0000001"}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "title": "T", "descriptionFile": nil,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.NotContains(t, wire, "description_file", "a null mention is consumed — no file spelling reaches the wire")
}

// #1849 review r1: the legacy snake_case spelling must ALSO be
// consumed — neither file spelling ever reaches the wire (null and
// resolved-value paths), and the both-spellings ambiguity is refused.
func TestBoardFileParams_LegacySpellingConsumed_PathAndNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "charter.md")
	require.NoError(t, os.WriteFile(path, []byte("legacy charter"), 0o644))

	// resolved-value path: legacy spelling loads the file, both keys gone
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-n0000002"}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "title": "T-legacy", "description_file": path,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.NotContains(t, wire, "description_file", "legacy spelling consumed — never forwarded")
	assert.NotContains(t, wire, "descriptionFile", "canonical spelling absent — never forwarded")
	assert.Contains(t, string(wire["description"]), "legacy charter", "content rides as the inline field")

	// null path: a null legacy mention is consumed too
	api2, rec2 := newAutomationAPI(t, 200, `{"short_id":"tsk-n0000003"}`)
	setupRenameWorkspaceEnv(t, api2)
	_, err = mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "title": "T", "description_file": nil,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(rec2.body), &wire))
	assert.NotContains(t, wire, "description_file", "null legacy mention consumed")
}

func TestBoardFileParams_BothSpellingsRefused(t *testing.T) {
	args := map[string]any{"descriptionFile": "/a.md", "description_file": "/b.md"}
	err := resolveBoardFileArgs(args, "description")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two spellings")
}

// #1849 review r1 pin: on a no-file tool, EVERY file-suffixed key is
// foreign — including the bare "_file" key the r1 draft accidentally
// admitted via the legacy-alias exemption.
func TestBoardFileParams_BareUnderscoreFileKeyRefused(t *testing.T) {
	err := resolveBoardFileArgs(map[string]any{"_file": "/tmp/x.md"}, "")
	require.Error(t, err, "a bare _file key on a no-file tool is a caller mistake, refused loudly")
}

func TestBoardFileParams_EchoDiffPassesThroughVerbatim(t *testing.T) {
	// #1837 review round 2: content_echo_diff rides the platform body
	// to the calling agent untouched (agentd re-models nothing).
	api, rec := newAutomationAPI(t, 200, `{"short_id":"tsk-e0000001","title":"stored","content_echo_diff":["title"]}`)
	setupRenameWorkspaceEnv(t, api)
	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_task", map[string]any{
		"taskId": "tsk-e0000001", "title": "submitted",
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, `"content_echo_diff":["title"]`)
	_ = rec
}

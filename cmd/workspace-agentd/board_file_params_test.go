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

	args := map[string]any{"description_file": path}
	require.NoError(t, resolveBoardFileArgs(args, "description"))
	require.Equal(t, charter, args["description"], "the resolved description must be byte-identical to the file")
	assert.NotContains(t, args, "description_file", "the file param is consumed, never forwarded")
}

func TestBoardFileParams_OutcomeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.md")
	require.NoError(t, os.WriteFile(path, []byte("steer"), 0o600))
	args := map[string]any{"outcome_file": path}
	require.NoError(t, resolveBoardFileArgs(args, "outcome"))
	require.Equal(t, "steer", args["outcome"])
}

func TestBoardFileParams_AmbiguousInlineAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.md")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))
	args := map[string]any{"description_file": path, "description": "inline"}
	err := resolveBoardFileArgs(args, "description")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one or the other")
}

func TestBoardFileParams_FileForWrongField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.md")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	args := map[string]any{"description_file": path}
	err := resolveBoardFileArgs(args, "outcome")
	require.Error(t, err, "a description_file on an outcome-field tool is a caller mistake, refused loudly")
}

func TestBoardFileParams_CapAndShapeGuards(t *testing.T) {
	t.Run("over cap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big.md")
		require.NoError(t, os.WriteFile(path, make([]byte, boardFileFieldMaxBytes+1), 0o600))
		args := map[string]any{"description_file": path}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cap")
	})

	t.Run("FIFO refused", func(t *testing.T) {
		dir := t.TempDir()
		fifo := filepath.Join(dir, "fifo")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))
		args := map[string]any{"description_file": fifo}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "regular file")
	})

	t.Run("missing file", func(t *testing.T) {
		args := map[string]any{"description_file": filepath.Join(t.TempDir(), "absent.md")}
		err := resolveBoardFileArgs(args, "description")
		require.Error(t, err)
	})

	t.Run("non-UTF-8 refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin.md")
		require.NoError(t, os.WriteFile(path, []byte("ok\x80\xff\xfe"), 0o600))
		args := map[string]any{"description_file": path}
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
		"epicId": "epc-bbbb2222", "title": "T-file", "description_file": path,
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	assert.Contains(t, out, "tsk-file0001")

	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(rec.body), &wire))
	assert.JSONEq(t, jsonMustString(t, content), string(wire["description"]),
		"the file's content reaches the wire verbatim as description")
	assert.NotContains(t, wire, "description_file")
	assert.Equal(t, http.MethodPost, rec.method)
	assert.Equal(t, "/internal/v1/automation/board/epics/epc-bbbb2222/tasks", rec.path)
}

func jsonMustString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

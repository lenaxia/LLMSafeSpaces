// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// board_manager_session_test.go — the attribution-only identity
// contract's degradation arm (PR review r2): a well-typed but
// UNRESOLVABLE declared session ref must not refuse the manager call —
// the board write proceeds with empty audit attribution and the loss
// is noted LOUDLY (a warn log, the loud-degradation contract the
// mistyped-camel arm's short-circuit never reaches).
func TestMCPBoard_ManagerSessionUnresolvableDeclaredRef(t *testing.T) {
	// Capture the warn the loss path emits (TestMain's zap.NewNop
	// default would swallow it silently).
	obsCore, logs := observer.New(zapcore.WarnLevel)
	prev := log
	log = zap.New(obsCore)
	t.Cleanup(func() { log = prev })

	// Loopback seam: the alias lookup succeeds with an EMPTY session
	// list — a spaces-bearing ref matching nothing is the resolver's
	// errSessionRef verdict (not the transport-failure passthrough).
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(loopback.Close)
	origAddr := agentAddrAtomic.Load()
	agentAddrAtomic.Store(loopback.URL)
	t.Cleanup(func() { agentAddrAtomic.Store(origAddr) })

	// The API stub: every board path answers 200.
	var boardPath, boardMethod string
	var boardBody []byte
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		boardPath, boardMethod = r.URL.Path, r.Method
		if r.Body != nil {
			boardBody, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"t-1"}`))
	}))
	t.Cleanup(api.Close)
	setupRenameWorkspaceEnv(t, api)

	path := filepath.Join(t.TempDir(), "patch.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"title":"T"}`), 0o600))
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_update_story", map[string]any{
		"taskId": "tsk-aaaa1111", "patchFile": path,
		"sessionId": "No Such Alias Anywhere"})
	require.NoError(t, err, "an unresolvable declared ref degrades to empty attribution — the call proceeds")

	assert.Equal(t, http.MethodPatch, boardMethod)
	assert.Equal(t, "/internal/v1/automation/board/tasks/tsk-aaaa1111", boardPath)
	var wire map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(boardBody, &wire))
	assert.Nil(t, wire["sessionId"], "unresolvable ref leaves NO identity stamp on the wire")
	assert.JSONEq(t, `"T"`, string(wire["title"]))

	warns := logs.FilterLevelExact(zapcore.WarnLevel).TakeAll()
	require.Len(t, warns, 1, "exactly one loud loss note")
	assert.Contains(t, warns[0].Message, "empty audit attribution")
	assert.Equal(t, "No Such Alias Anywhere", warns[0].ContextMap()["ref"])
	assert.Equal(t, "board_update_story", warns[0].ContextMap()["tool"])
}

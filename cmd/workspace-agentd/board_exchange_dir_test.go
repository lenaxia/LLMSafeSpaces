// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_exchange_dir_test.go — design 0070 §2.4a: the payload
// exchange dir makes the auto-cleanup contract REAL in sidecar mode.
//
// The live failure (owner report 2026-10-30): every board file-payload
// call in a sidecar-mode workspace returned cleanup_warning
// "… read-only file system" — the cleanup hook's os.Remove runs in the
// SIDECAR agentd process, whose /workspace mount is ReadOnly by design
// (0055 M2), and /platform is uid-2000-exclusive (the runtime's write
// tool cannot create files there either). No path was both
// runtime-writable and sidecar-deletable.
//
// The contract now: payload files live under /exchange (the PVC
// subPath mounted RW in BOTH containers); relative board file params
// anchor there; the cleanup hook unlinks through the same mount the
// write tool wrote — no warning. Absolute /workspace paths stay
// READABLE for one release (existing scripts), and a delete failure
// there warns with the pointer AT the exchange dir (the documented
// compat treatment — chosen over outright refusal in the PR).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boardExchangeRootOverride points the exchange + workspace roots at
// temp dirs (the var-override discipline TestBoardFileParams_
// PathAnchoring established) and restores them.
func boardExchangeRootOverride(t *testing.T) (exchange, workspace string) {
	t.Helper()
	exchange = t.TempDir()
	workspace = t.TempDir()
	origEx, origWs := boardFileExchangeRoot, boardFileWorkspaceRoot
	boardFileExchangeRoot = exchange
	boardFileWorkspaceRoot = workspace
	t.Cleanup(func() {
		boardFileExchangeRoot = origEx
		boardFileWorkspaceRoot = origWs
	})
	return exchange, workspace
}

// TestBoardFileParams_ExchangeDelete_NoWarning: the acceptance
// criterion 1 shape — a successful payload-bearing CRUD call through
// the exchange-anchored path deletes its payload file and returns NO
// cleanup_warning. In the pod this is exactly the sidecar's delete
// through the dual-rw /exchange mount (the temp dir stands in for it,
// the same stand-in discipline the anchoring tests use).
func TestBoardFileParams_ExchangeDelete_NoWarning(t *testing.T) {
	exchange, _ := boardExchangeRootOverride(t)

	payload := `{"title":"T-exchange","description":"payload lives in the exchange dir","acceptanceCriteria":[{"text":"c"}]}`
	path := boardWritePayloadFile(t, exchange, "task.json", payload)

	api, _ := newAutomationAPI(t, 200, `{"short_id":"tsk-exch0001"}`)
	setupRenameWorkspaceEnv(t, api)
	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "bodyFile": "task.json", // RELATIVE → /exchange/task.json
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr),
		"the consumed payload file is DELETED through the exchange-anchored path (the 0070 contract, finally real in sidecar mode)")
	assert.NotContains(t, out, "cleanup_warning",
		"a successful exchange-anchored call carries NO cleanup_warning — the EROFS status quo is dead")
}

// TestBoardFileParams_ExchangeDelete_AbsolutePath: the absolute-path
// spelling of the same contract — an agent may write /exchange/foo
// and submit the absolute path.
func TestBoardFileParams_ExchangeDelete_AbsolutePath(t *testing.T) {
	exchange, _ := boardExchangeRootOverride(t)

	payload := `{"title":"T-abs","description":"d","acceptanceCriteria":[{"text":"c"}]}`
	path := boardWritePayloadFile(t, exchange, "abs.json", payload)

	api, _ := newAutomationAPI(t, 200, `{"short_id":"tsk-exch0002"}`)
	setupRenameWorkspaceEnv(t, api)
	_, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "bodyFile": path, // ABSOLUTE under the exchange root
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "absolute exchange paths delete on success too")
}

// TestBoardFileParams_LegacyWorkspacePath_ReadAndPointedWarning: the
// compat window (criterion 5, decision: read+warn). An ABSOLUTE
// /workspace payload path is still READ (the sidecar's RO mount is
// enough — existing scripts keep working for one release), and when
// its delete fails (in the pod: EROFS on the RO mount; here: the
// dir-swap trick forces the same os.Remove refusal) the warning POINTS
// AT THE EXCHANGE DIR instead of telling the agent to rm the file
// itself.
func TestBoardFileParams_LegacyWorkspacePath_ReadAndPointedWarning(t *testing.T) {
	exchange, workspace := boardExchangeRootOverride(t)

	payload := `{"title":"T-legacy","description":"written under the legacy workspace root","acceptanceCriteria":[{"text":"c"}]}`
	path := boardWritePayloadFile(t, workspace, "legacy.json", payload)

	// During the wire call (after resolution, before cleanup), swap
	// the consumed file for a NON-EMPTY directory: os.Remove refuses —
	// the same refusal class as the sidecar's EROFS (the read already
	// succeeded; the delete cannot).
	api, _ := newAutomationAPI(t, 200, `{"short_id":"tsk-legacy01"}`)
	api.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		_ = os.Remove(path)
		_ = os.MkdirAll(filepath.Join(path, "child"), 0o755)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"short_id":"tsk-legacy01"}`))
	})
	setupRenameWorkspaceEnv(t, api)

	out, err := mcpAutomation(context.Background(), mcpTestPassword, "board_create_task", map[string]any{
		"epicId": "epc-bbbb2222", "bodyFile": path, // ABSOLUTE /workspace path — the compat arm
		"lsp_injected_session": "ses-1",
	})
	require.NoError(t, err, "the legacy read still works — compat is read+warn, not refusal")
	assert.Contains(t, out, "short_id") // the platform body is intact

	// The pointed warning: the call succeeded, the file could not be
	// deleted, and the agent is told WHERE payload files belong — not
	// merely to rm the file. The text names the CURRENT exchange root
	// (the anchor var; the production value is /exchange).
	assert.Contains(t, out, "cleanup_warning")
	assert.Contains(t, out, exchange,
		"the warning names the exchange dir — the fix for the failure, not a chore list")
	assert.Contains(t, out, "legacy", "the warning says the location is legacy")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// transcript_repair_test.go — #1342 owner-triage front 3: the
// transcript-repair path for ALREADY-orphaned running tool parts. The
// 2026-09-11 incident's eternal spinners render from the harness's
// durable store on every session load (GetHistory → DB "running" →
// ToolStatusRunning); when the live session-status registry says the
// session has no running turn, a running tool part in the served page
// is closed as error/"harness restart" at read time — no store write,
// STRICT failure semantics (an unavailable registry never aborts a
// live tool).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// historyWithRunningTool is one assistant message whose tool part is
// stuck "running" in the durable store — the incident's orphan shape.
const historyWithRunningTool = `[
	{
		"info":{"role":"user","id":"msg_0"},
		"parts":[{"type":"text","text":"run the build"}]
	},
	{
		"info":{"role":"assistant","id":"msg_1"},
		"parts":[{"type":"tool","tool":"bash","state":{"status":"running","key":"tpu_1"}}]
	}
]`

func firstToolPart(t *testing.T, msgs []session.Message) *session.ToolPart {
	t.Helper()
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Tool != nil {
				return p.Tool
			}
		}
	}
	t.Fatal("no tool part in translated history")
	return nil
}

func TestGetHistory_OrphanedRunningTool_RepairedWhenIdle(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
	srv.register("GET", "/session/status", `{"ses_a":{"type":"idle"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	tool := firstToolPart(t, msgs)
	assert.Equal(t, session.ToolStatusError, tool.State.Status,
		"an orphaned running part on an idle session must render terminal")
	assert.Equal(t, session.ToolAbortReasonHarnessRestart, tool.State.Error)
}

func TestGetHistory_RunningTool_LiveBusySessionUntouched(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
	srv.register("GET", "/session/status", `{"ses_a":{"type":"busy"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	tool := firstToolPart(t, msgs)
	assert.Equal(t, session.ToolStatusRunning, tool.State.Status,
		"a live turn's running part must NEVER be falsely aborted")
	assert.Empty(t, tool.State.Error)
}

func TestGetHistory_RunningTool_RetryAndCompactingAreLive(t *testing.T) {
	for _, typ := range []string{"retry", "compacting"} {
		srv := newFakeOpencode(t)
		srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
		srv.register("GET", "/session/status", `{"ses_a":{"type":"`+typ+`"}}`, 0)

		a := newTestAdapter(t, srv.Server)
		msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
		require.NoError(t, err)
		assert.Equal(t, session.ToolStatusRunning, firstToolPart(t, msgs).State.Status,
			"status %q is live activity — no repair", typ)
	}
}

func TestGetHistory_RunningTool_SessionAbsentFromStatusRegistry_Repaired(t *testing.T) {
	// The incident's shape after a harness restart: the turn died with
	// the process, so the live registry has no entry for the session.
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
	srv.register("GET", "/session/status", `{"ses_other":{"type":"busy"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	tool := firstToolPart(t, msgs)
	assert.Equal(t, session.ToolStatusError, tool.State.Status)
	assert.Equal(t, session.ToolAbortReasonHarnessRestart, tool.State.Error)
}

func TestGetHistory_RunningTool_StatusFetchFails_NoRepair(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
	srv.register("GET", "/session/status", ``, 500)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	tool := firstToolPart(t, msgs)
	assert.Equal(t, session.ToolStatusRunning, tool.State.Status,
		"STRICT failure semantics: an unavailable status registry must never read as idle and abort a possibly-live tool")
}

func TestGetHistory_RunningTool_ErrorSessionStatus_Repaired(t *testing.T) {
	// A session in error state has no running turn — its running part is
	// fiction left by a dead/failed turn.
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", historyWithRunningTool, 0)
	srv.register("GET", "/session/status", `{"ses_a":{"type":"error"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	assert.Equal(t, session.ToolStatusError, firstToolPart(t, msgs).State.Status)
}

func TestGetHistory_NoRunningPart_NoStatusFetch(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", `[
		{"info":{"role":"user","id":"msg_0"},"parts":[{"type":"text","text":"hi"}]}
	]`, 0)
	// /session/status is NOT registered — a fetch would 404 and, more
	// importantly, the request recorder proves it never happened.
	a := newTestAdapter(t, srv.Server)
	_, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	for _, r := range srv.requests {
		assert.NotContains(t, r, "/session/status",
			"pages without running parts must not pay the status-registry call")
	}
}

func TestGetHistory_CompletedTool_NeverTouched(t *testing.T) {
	srv := newFakeOpencode(t)
	srv.register("GET", "/session/ses_a/message", `[
		{"info":{"role":"assistant","id":"msg_1"},
		 "parts":[{"type":"tool","tool":"bash","state":{"status":"completed","key":"tpu_1"}}]}
	]`, 0)
	srv.register("GET", "/session/status", `{"ses_a":{"type":"idle"}}`, 0)

	a := newTestAdapter(t, srv.Server)
	msgs, err := a.GetHistory(context.Background(), "u-1", "ws-1", "ses_a")
	require.NoError(t, err)

	tool := firstToolPart(t, msgs)
	assert.Equal(t, session.ToolStatusCompleted, tool.State.Status,
		"terminal parts keep their own state — the repair touches running only")
	assert.Empty(t, tool.State.Error)
}

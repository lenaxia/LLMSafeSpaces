// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_directive_comms_test.go — #2046 directive delivery discipline,
// the app-side pins:
//
//   - the directive texts carry the lsp:agent-message-v1 sentinel with
//     a TYPED payload (mode board-directive, the kind, the task, the
//     audit id) — one sentinel composer shared with leaseNudgeText;
//   - the BORN-RED adapter-boundary pin: a directive driven through
//     the PRODUCTION messenger lands at POST /session/:id/message with
//     ≥1 NON-EMPTY text part carrying the sentinel (the 2026-11-15
//     incident saw user-role agent:"build" messages with ZERO parts —
//     whatever loses the parts downstream, this seam must never send
//     them);
//   - a busy target (a held turn) queues the delivery at the turn
//     boundary: the caller returns inside the fast-fail window, the
//     POST completes when the turn does (admit-and-schedule, R4);
//   - a dead target's fast 4xx is the synchronous delivery outcome the
//     audit event records.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// directiveSentinelPayload decodes the sentinel line's JSON payload.
func directiveSentinelPayload(t *testing.T, text string) map[string]any {
	t.Helper()
	lines := strings.SplitN(text, "\n", 2)
	require.Len(t, lines, 2, "sentinel line + prose")
	line := lines[0]
	require.True(t, strings.HasPrefix(line, "<!-- lsp:agent-message-v1 {"), "v1 sentinel prefix")
	require.True(t, strings.HasSuffix(line, " -->"), "v1 sentinel suffix")
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSuffix(
		strings.TrimPrefix(line, "<!-- lsp:agent-message-v1 "), " -->")), &payload))
	return payload
}

func TestDirectiveTexts_SentinelGrammarAndProse(t *testing.T) {
	d := board.Directive{Kind: board.DirectiveHalt, TaskShortID: "tsk-directed-worker", AuditID: "dir-audit-1"}

	halt := haltDirectiveText(d)
	pl := directiveSentinelPayload(t, halt)
	assert.Equal(t, "board-directive", pl["mode"])
	assert.Equal(t, "halt", pl["directive"])
	assert.Equal(t, "tsk-directed-worker", pl["taskId"])
	assert.Equal(t, "dir-audit-1", pl["auditId"])
	prose := strings.SplitN(halt, "\n", 2)[1]
	assert.Contains(t, prose, "BOARD PAUSE DIRECTIVE", "the prose carries the pause contract")
	assert.Contains(t, prose, "tsk-directed-worker")
	assert.Contains(t, prose, "409 task_paused")

	rd := board.Directive{Kind: board.DirectiveResume, TaskShortID: "tsk-directed-worker", AuditID: "dir-audit-2"}
	pl = directiveSentinelPayload(t, resumeDirectiveText(rd))
	assert.Equal(t, "resume", pl["directive"])
	assert.Equal(t, "dir-audit-2", pl["auditId"])
	assert.Contains(t, resumeDirectiveText(rd), "BOARD RESUME DIRECTIVE")

	dp := board.Directive{Kind: board.DirectiveDeposed, TaskShortID: "tsk-directed-worker", AuditID: "dir-audit-3"}
	pl = directiveSentinelPayload(t, deposedDirectiveText(dp))
	assert.Equal(t, "deposed", pl["directive"])
	assert.Contains(t, deposedDirectiveText(dp), "BOARD DEPOSED DIRECTIVE")
}

// TestDirectiveSentinel_MatchesNudgeComposerShape pins the ONE-composer
// contract: the directive sentinel and the lease-nudge sentinel share
// one line grammar (one composer, two callers — single-owned format).
func TestDirectiveSentinel_MatchesNudgeComposerShape(t *testing.T) {
	nudge := leaseNudgeText("tsk-x", board.NudgePayload{NudgeID: "n-1", TaskID: "uuid", TaskShortID: "tsk-x"})
	directive := haltDirectiveText(board.Directive{Kind: board.DirectiveHalt, TaskShortID: "tsk-x", AuditID: "a-1"})

	for _, text := range []string{nudge, directive} {
		line := strings.SplitN(text, "\n", 2)[0]
		require.True(t, strings.HasPrefix(line, "<!-- lsp:agent-message-v1 {"), "shared composer prefix")
		require.True(t, strings.HasSuffix(line, " -->"), "shared composer suffix")
		require.NotContains(t, line, "\n", "the sentinel is a single line")
	}
}

// TestBoardWorkerComms_Messenger_DirectiveSentinelAtAdapterBoundary is
// the born-red #2046 pin: the directive delivered through the
// PRODUCTION messenger (newBoardMessenger → boardResolvingSender.Send →
// adapter → V1 POST) carries ≥1 NON-EMPTY text part holding the
// sentinel — the incident's zero-parts injections can never originate
// at this seam again.
func TestBoardWorkerComms_Messenger_DirectiveSentinelAtAdapterBoundary(t *testing.T) {
	stub := newStubOpencode(t)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	messenger := newBoardMessenger(scope.resolve, newBoardTestSender(adapter))

	require.NoError(t, messenger(context.Background(), "ses_worker",
		board.Directive{Kind: board.DirectiveHalt, TaskShortID: wireShortID, AuditID: "dir-red-1"}))

	require.Eventually(t, func() bool {
		v1, _, _ := stub.counts()
		return v1 >= 1
	}, 5*time.Second, 20*time.Millisecond, "the directive must be delivered over V1")

	_, _, texts := stub.counts()
	require.Len(t, texts, 1)
	text := texts[0]
	require.NotEmpty(t, text, "the delivered text part is NON-EMPTY (the incident's zero-parts shape)")
	pl := directiveSentinelPayload(t, text)
	assert.Equal(t, "board-directive", pl["mode"])
	assert.Equal(t, "halt", pl["directive"])
	assert.Equal(t, wireShortID, pl["taskId"])
	assert.Equal(t, "dir-red-1", pl["auditId"])
	assert.Contains(t, text, "BOARD PAUSE DIRECTIVE", "the prose follows the sentinel")
}

// TestBoardWorkerComms_Messenger_BusyTargetQueuesAtTurnBoundary pins
// R4's admit-and-schedule at the delivery seam: a directive to a BUSY
// target (the stub holds the V1 response open — the running-turn shape)
// is dispatched without waiting the turn out (the caller returns inside
// the fast-fail window) and completes when the turn does — delivery at
// the turn boundary, never a mid-generation cut and never a drop.
func TestBoardWorkerComms_Messenger_BusyTargetQueuesAtTurnBoundary(t *testing.T) {
	stub := newStubOpencode(t)
	stub.mu.Lock()
	stub.v1Delay = 8 * time.Second // the held turn — LONGER than the fast-fail window
	stub.mu.Unlock()
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	messenger := newBoardMessenger(scope.resolve, newBoardTestSender(adapter))

	start := time.Now()
	require.NoError(t, messenger(context.Background(), "ses_worker",
		board.Directive{Kind: board.DirectiveHalt, TaskShortID: wireShortID, AuditID: "dir-busy-1"}))
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 4500*time.Millisecond,
		"a busy target queues server-side — the pause request returns at the fast-fail window, not after the turn (turn held 8s, caller returned in %s)", elapsed)
	assert.Greater(t, elapsed, 2500*time.Millisecond,
		"the window is the 3s fast-fail bound — an instant return would mean the delivery verdict never waited")

	select {
	case <-stub.respondedSignal():
	case <-time.After(15 * time.Second):
		t.Fatal("the queued directive delivery never completed at the turn boundary")
	}
	_, _, texts := stub.counts()
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "lsp:agent-message-v1", "the boundary-delivered text carries the sentinel")
}

// TestBoardWorkerComms_Messenger_DeadSession4xxIsTheAuditOutcome pins
// the outcome the audit event keys on: a dead target's fast 4xx returns
// SYNCHRONOUSLY from the messenger — the "failed" the board-side event
// records — and no directive text is lost silently.
func TestBoardWorkerComms_Messenger_DeadSession4xxIsTheAuditOutcome(t *testing.T) {
	stub := newStubOpencode(t)
	stub.mu.Lock()
	stub.v1Status = http.StatusNotFound // the dead/reaped session shape
	stub.mu.Unlock()
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	messenger := newBoardMessenger(scope.resolve, newBoardTestSender(adapter))

	err := messenger(context.Background(), "ses_dead",
		board.Directive{Kind: board.DirectiveHalt, TaskShortID: wireShortID, AuditID: "dir-dead-1"})
	require.Error(t, err, "a dead session's 4xx is the delivery outcome the audit event records")
	assert.Contains(t, err.Error(), "404")
}

// compile-time pin: the messenger seam stays the board.WorkerMessenger
// shape (the structured directive — kind + task + audit id).
var _ board.WorkerMessenger = newBoardMessenger(func(context.Context, string) (string, string, error) {
	return "", "", nil
}, nil)

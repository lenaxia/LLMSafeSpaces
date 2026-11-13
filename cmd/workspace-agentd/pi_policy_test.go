// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_policy_test.go — the §1.4 policy pins against the scripted RPC
// fixture subprocess: the per-session policy's statusz parity
// end-to-end (acceptance criterion 1, wire-in → snapshot-out), the
// crash → backoff → respawn-with-same-session-id resume proof
// (criterion 2's restart half), the wedge-restart remedy, and the
// single-process policy's switch_session + busy refusal.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestPolicy builds a per-session policy over a fresh fixture tree.
type piPolicyFixture struct {
	policy  *processPerSessionPolicy
	fixture string
}

func newPerSessionFixture(t *testing.T, extraEnv ...string) *piPolicyFixture {
	t.Helper()
	withTestLogger(t)
	fixture := t.TempDir()
	cfg := piFakeBaseConfig(t, fixture, extraEnv...)
	return &piPolicyFixture{
		policy:  newProcessPerSessionPolicy(cfg, zap.NewNop(), newPiStateAggregator()),
		fixture: fixture,
	}
}

// writeScript stages the event lines a prompt emits for sessionID.
func (f *piPolicyFixture) writeScript(sessionID string, lines ...string) {
	piFakeWriteFile(filepath.Join(f.fixture, "script"), sessionID+".script",
		fmt.Sprintf("# scripted fixture for %s\n%s", sessionID, joinLines(lines)))
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// attachWait attaches and requires a live generation.
func attachWait(t *testing.T, p piProcessPolicy, sessionID string) piSessionHandle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := p.Attach(ctx, sessionID)
	require.NoError(t, err)
	return h
}

// prompt issues a prompt against the session's current child.
func prompt(t *testing.T, h piSessionHandle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.Call(ctx, map[string]any{"type": "prompt", "text": "fixture turn"})
	require.NoError(t, err)
}

// The happy-path + retry shapes validated against the pinned binary.
const (
	evtAgentStart = `{"type":"agent_start"}`
	evtUserEnd    = `{"type":"message_end","message":{"role":"user","text":"fixture turn"}}`
	evtAssistant  = `{"type":"message_end","message":{"role":"assistant","usage":{"input":900,"output":100,"cacheRead":1000,"cacheWrite":50}}}`
	evtAgentEnd   = `{"type":"agent_end","willRetry":false}`
	evtSettled    = `{"type":"agent_settled","aborted":false}`
	evtRetryEnd   = `{"type":"agent_end","willRetry":true}`
	evtRetryStart = `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":500,"errorMessage":"provider 429"}`
)

func TestPiPolicyStatuszParityAgainstScriptedFixture(t *testing.T) {
	f := newPerSessionFixture(t)

	// s-busy: prompt → retry storm → NO settle (the wedge-shaped turn).
	// Agent stays busy through two agent_end records — the settle
	// contract's e2e pin.
	f.writeScript("s-busy",
		evtAgentStart, evtUserEnd, evtRetryStart, evtRetryEnd, evtRetryStart, evtRetryEnd)
	// s-done: a complete turn with usage and settle.
	f.writeScript("s-done", evtAgentStart, evtUserEnd, evtAssistant, evtAgentEnd, evtSettled)

	attachWait(t, f.policy, "s-busy")
	attachWait(t, f.policy, "s-done")

	// Pre-seed the persisted rows (ReconcileDir's input) so a third,
	// never-attached session renders idle.
	piFakeWriteFile(filepath.Join(f.fixture, "sessions"),
		"20261113T100000_s-persisted.jsonl", `{"kind":"session_info"}`)

	hBusy := attachWait(t, f.policy, "s-busy")
	hDone := attachWait(t, f.policy, "s-done")
	prompt(t, hBusy)
	prompt(t, hDone)

	// Let the events land (reader goroutines are asynchronous).
	require.Eventually(t, func() bool {
		snap := f.policy.Snapshot()
		return len(snap.Sessions) == 2 &&
			snap.Sessions[0].Status == "busy" && snap.Sessions[1].Status == "idle"
	}, 10*time.Second, 50*time.Millisecond, "both turns must fold: s-busy busy, s-done idle")

	// Health-pass corroboration: state/stats polls fold the facts the
	// events do not carry (model, context, names) — the per-session
	// state files drive each child's answers separately.
	piFakeWriteFile(filepath.Join(f.fixture, "state"), "s-busy.state.json",
		`{"sessionId":"s-busy","sessionName":"Busy fixture","isStreaming":true,
		  "model":{"id":"big-ctx","provider":"relay","contextWindow":256000}}`)
	piFakeWriteFile(filepath.Join(f.fixture, "state"), "s-done.state.json",
		`{"sessionId":"s-done","sessionName":"Done fixture","isStreaming":false,
		  "model":{"id":"small-ctx","provider":"relay","contextWindow":32000}}`)
	piFakeWriteFile(filepath.Join(f.fixture, "stats"), "s-busy.stats.json",
		`{"tokens":{"input":900,"output":100,"cacheRead":1000,"cacheWrite":50},
		  "contextUsage":{"tokens":2050,"contextWindow":256000}}`)
	pollCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f.policy.HealthPoll(pollCtx)

	f.policy.ReconcileDir()
	snap := f.policy.Snapshot()

	require.Len(t, snap.Sessions, 3, "live + remembered + dir-scan rows")
	require.Equal(t, 1, snap.SessionsActive)

	// Re-marshal each row so the per-session asserts read uniformly.
	info := map[string]json.RawMessage{}
	for _, s := range snap.Sessions {
		b, err := json.Marshal(s)
		require.NoError(t, err)
		info[s.ID] = b
	}

	var busyInfo, doneInfo struct {
		ID          string          `json:"id"`
		Status      string          `json:"status"`
		Title       string          `json:"title"`
		Model       string          `json:"model"`
		ContextUsed int64           `json:"contextUsed"`
		Tokens      json.RawMessage `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal(info["s-busy"], &busyInfo))
	require.NoError(t, json.Unmarshal(info["s-done"], &doneInfo))

	// s-busy: busy, aged, streaming-corroborated, full parity fields.
	require.Equal(t, "busy", busyInfo.Status)
	require.Equal(t, "Busy fixture", busyInfo.Title)
	require.Equal(t, "relay/big-ctx", busyInfo.Model)
	require.Equal(t, int64(2050), busyInfo.ContextUsed)
	require.Contains(t, snap.BusyAges, "s-busy")
	require.GreaterOrEqual(t, snap.OldestBusySeconds, 0)
	require.GreaterOrEqual(t, snap.LastProgressSeconds, 0)
	var busyTokens struct {
		Input  int64 `json:"input"`
		Output int64 `json:"output"`
	}
	require.NoError(t, json.Unmarshal(busyInfo.Tokens, &busyTokens))
	require.Equal(t, int64(900), busyTokens.Input)
	require.Equal(t, int64(100), busyTokens.Output)

	// s-done: settled idle, its own model/title, no busy age.
	require.Equal(t, "idle", doneInfo.Status)
	require.Equal(t, "Done fixture", doneInfo.Title)
	require.Equal(t, "relay/small-ctx", doneInfo.Model)
	require.NotContains(t, snap.BusyAges, "s-done")

	// s-persisted: dir-scan row renders idle with no live truth.
	var persistedInfo struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(info["s-persisted"], &persistedInfo))
	require.Equal(t, "idle", persistedInfo.Status)

	require.NotNil(t, snap.Context)
	require.Equal(t, int64(256000), snap.Context.TotalTokens, "max session context window wins the aggregate")

	require.NoError(t, f.policy.Shutdown(context.Background()))
}

func TestPiPolicyCrashRespawnResumesSameSession(t *testing.T) {
	f := newPerSessionFixture(t, "PI_FAKE_SUICIDE_ON_PROMPT=1")
	h := attachWait(t, f.policy, "s-crash")
	require.Equal(t, uint64(1), h.Generation())
	bootPath := filepath.Join(f.fixture, "boot", "s-crash.boot")
	require.Eventually(t, func() bool {
		return len(piFakeLines(bootPath)) == 1
	}, 10*time.Second, 50*time.Millisecond, "generation 1 must boot with the pinned session id")

	// Prompt → agent_start → exit(9) mid-turn: busy-then-dead, no
	// settle. The response arrives before the death, so the prompt
	// itself succeeds.
	prompt(t, h)

	// The crash respawn (managedProcess backoff curve: first crash
	// sleeps ~2s) brings a fresh generation pinned to the SAME
	// session id — the persisted-session resume contract.
	require.Eventually(t, func() bool {
		return h.Generation() >= 2
	}, 15*time.Second, 100*time.Millisecond, "crash must respawn (fresh generation)")

	// #1632 no-inherit: the dead generation's busy episode is void.
	snap := f.policy.Snapshot()
	var status string
	for _, s := range snap.Sessions {
		if s.ID == "s-crash" {
			status = s.Status
		}
	}
	require.Equal(t, "idle", status, "respawn must not inherit the busy episode")
	require.Equal(t, 1, h.Restarts())

	// Both generations spawned with the same --session-id (the boot
	// log carries argv), and the persisted transcript line from the
	// crashed turn survived for the resumed child.
	require.Eventually(t, func() bool {
		boots := piFakeLines(bootPath)
		return len(boots) == 2 &&
			strings.Contains(boots[1], "--session-id s-crash") &&
			strings.Contains(boots[1], "--mode rpc")
	}, 10*time.Second, 50*time.Millisecond, "both generations pin the session id")
	files, err := os.ReadDir(filepath.Join(f.fixture, "persist"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "the crashed turn's transcript line persisted")

	require.NoError(t, f.policy.Shutdown(context.Background()))
}

func TestPiPolicyWedgedChildRestartedByHealthPoll(t *testing.T) {
	orig := piStdinWriteTimeout
	piStdinWriteTimeout = 250 * time.Millisecond
	t.Cleanup(func() { piStdinWriteTimeout = orig })

	// Stop reading after the initial get_state (the first command the
	// child ever sees is whichever arrives first — make it robust by
	// stopping after 2).
	f := newPerSessionFixture(t, "PI_FAKE_STOP_READ_AFTER=2")
	h := attachWait(t, f.policy, "s-wedge")

	// Fill the pipe until wedged: per-call short deadlines so an
	// unanswered-but-written call frees the loop to write the next one
	// (only a BLOCKED write — the wedged classification — ends early).
	junk := strings.Repeat("j", 8*1024)
	wedged := false
	for i := 0; i < 32 && !wedged; i++ {
		callCtx, callCancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		_, err := h.Call(callCtx, map[string]any{"type": "prompt", "padding": junk})
		callCancel()
		if err != nil && strings.Contains(err.Error(), "wedged") {
			wedged = true
		}
	}
	require.True(t, wedged, "the pipe must fill and classify the child wedged")

	// The health pass sees Wedged() → requestRestart (no backoff
	// penalty — the remedy, not a crash).
	f.policy.HealthPoll(context.Background())
	require.Eventually(t, func() bool {
		return h.Generation() >= 2
	}, 15*time.Second, 100*time.Millisecond, "health poll must restart the wedged child")
	require.Equal(t, 0, h.Restarts(), "a wedge restart is not a crash restart")

	require.NoError(t, f.policy.Shutdown(context.Background()))
}

func TestPiPolicyReleaseStopsChildKeepsRow(t *testing.T) {
	f := newPerSessionFixture(t)
	h := attachWait(t, f.policy, "s-rel")
	prompt(t, h) // persists a transcript line + dir row via ReconcileDir
	f.policy.ReconcileDir()
	require.Len(t, f.policy.Snapshot().Sessions, 1)

	f.policy.Release("s-rel")
	require.Eventually(t, func() bool {
		f.policy.ReconcileDir()
		snap := f.policy.Snapshot()
		return len(snap.Sessions) == 1 && snap.Sessions[0].Status == "idle"
	}, 10*time.Second, 50*time.Millisecond, "released session renders idle until its file disappears")
	require.NoError(t, f.policy.Shutdown(context.Background()))
}

func TestPiSingleProcessPolicySwitchAndBusyRefusal(t *testing.T) {
	withTestLogger(t)
	fixture := t.TempDir()
	cfg := piFakeBaseConfig(t, fixture)
	p := newSingleProcessPolicy(cfg, zap.NewNop(), newPiStateAggregator())

	// Two persisted session files (switch_session takes a path).
	piFakeWriteFile(filepath.Join(fixture, "sessions"), "20261113T090000_s-one.jsonl", `{"kind":"session_info"}`)
	piFakeWriteFile(filepath.Join(fixture, "sessions"), "20261113T100000_s-two.jsonl", `{"kind":"session_info"}`)

	h1 := attachWait(t, p, "s-one")
	require.Equal(t, uint64(1), h1.Generation())
	// The singleton spawns with --no-session (in-memory current
	// session — the policy's own spawn shape; the fake child resolves
	// its fixture knobs under that id).
	boot := piFakeLines(filepath.Join(fixture, "boot", "_nosession_.boot"))
	require.Len(t, boot, 1)
	require.Contains(t, boot[0], "--no-session")

	// Busy (started, never settled): switching is refused — the
	// recorded limitation of policy (b), stated honestly as a typed
	// error. The scripted events attribute to s-one (the policy's
	// current session), and the fake reads its script by its OWN id.
	promptWithScript(t, fixture, "_nosession_", evtAgentStart, evtUserEnd)
	prompt(t, h1)
	require.Eventually(t, func() bool {
		return p.Snapshot().SessionsActive == 1
	}, 10*time.Second, 50*time.Millisecond)

	_, err := p.Attach(context.Background(), "s-two")
	require.ErrorIs(t, err, errPiBusySwitch)

	// Settle, then the switch lands.
	promptWithScript(t, fixture, "_nosession_", evtSettled)
	prompt(t, h1)
	require.Eventually(t, func() bool {
		return p.Snapshot().SessionsActive == 0
	}, 10*time.Second, 50*time.Millisecond)

	_, err = p.Attach(context.Background(), "s-two")
	require.NoError(t, err)

	// The switch carried s-two's newest persisted path.
	cmds := piFakeLines(filepath.Join(fixture, "cmds", "_nosession_.cmds"))
	var switchLine string
	for _, c := range cmds {
		if len(c) > len("switch_session") && c[:len("switch_session")] == "switch_session" {
			switchLine = c
		}
	}
	require.NotEmpty(t, switchLine, "switch_session must have been issued")
	require.Contains(t, switchLine, "20261113T100000_s-two.jsonl", "newest persisted path for the target session")

	require.NoError(t, p.Shutdown(context.Background()))
}

// promptWithScript stages the script (by the FAKE-side session id)
// then issues the prompt through the handle.
func promptWithScript(t *testing.T, fixture, fakeSID string, lines ...string) {
	t.Helper()
	piFakeWriteFile(filepath.Join(fixture, "script"), fakeSID+".script", joinLines(lines))
}

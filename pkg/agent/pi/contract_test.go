// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// contract_test.go — the contract-test suite: every agent.Adapter
// method exercised against the recorded fixture transport
// (fixture_test.go), the pkg/abi/abitest stance applied to the pi seam
// (a reference implementation the suite runs against; recorded
// envelopes + the real persisted session file in testdata). Pins the
// story's acceptance surface:
//
//   - GetHistoryPage rides the native get_entries {since} durable
//     cursor on the steady-state path (asserted by command inspection).
//   - VerifyDelivery answers by JSONL file scan — with the RPC surface
//     DOWN (every session call fails) it still answers definitively.
//   - Capabilities exposes no ask/permission affordances; the pending
//     surface is authoritative-empty and replies are typed
//     not-supported.
//   - The abort-then-clear_queue dance preserves the queued-input
//     contract (drained queue texts are re-queued as follow_ups).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

const (
	fUserID      = "user-fixture"
	fWorkspaceID = "ws-fixture"
)

func newFixtureAdapter(t *testing.T) (*Adapter, *fixtureTransport) {
	t.Helper()
	ft := newFixtureTransport(t)
	return NewAdapter(ft, nil), ft
}

// --- Sessions -----------------------------------------------------------

func TestContract_CreateSession_MintsIDSetsName(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()
	s, err := a.CreateSession(ctx, fUserID, fWorkspaceID, "My title")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID == "" || len(s.ID) != 32 { // UUID hex, pi charset
		t.Fatalf("expected minted 32-char id, got %q", s.ID)
	}
	if err := validateSessionID(s.ID); err != nil {
		t.Fatalf("minted id fails pi charset: %v", err)
	}
	if s.WorkspaceID != fWorkspaceID {
		t.Fatalf("workspace: %q", s.WorkspaceID)
	}
	if got := ft.lastCommand(s.ID, "set_session_name"); got == nil || got["name"] != "My title" {
		t.Fatalf("set_session_name not issued with the title: %v", got)
	}
	// The persisted file exists (the transport created it) — GetSession
	// works against the fresh child.
	got, err := a.GetSession(ctx, fUserID, fWorkspaceID, s.ID)
	if err != nil {
		t.Fatalf("GetSession after create: %v", err)
	}
	if got.ID != s.ID {
		t.Fatalf("GetSession id %q != created %q", got.ID, s.ID)
	}
	if got.Status != session.StatusIdle {
		t.Fatalf("fresh session status: %q", got.Status)
	}
}

func TestContract_GetSession_RecordedState(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	ctx := context.Background()
	s, err := a.GetSession(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.ID != fixtureSessionID {
		t.Fatalf("id must be surfaced verbatim: %q", s.ID)
	}
	// The recorded get_state fixture (fixture-model, not streaming,
	// not compacting, sessionName unset).
	if s.Status != session.StatusIdle {
		t.Fatalf("status: %q", s.Status)
	}
	if s.Model == nil || s.Model.ID != "fixture-model" || s.Model.Provider != "fixture" {
		t.Fatalf("model: %+v", s.Model)
	}
	// Best-effort stats ride along: recorded contextUsage {111, 200000}.
	if s.ContextUsage == nil || s.ContextUsage.Used != 111 || s.ContextUsage.Window != 200000 {
		t.Fatalf("context usage: %+v", s.ContextUsage)
	}
}

func TestContract_GetSession_GoneIsTyped(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	_, err := a.GetSession(context.Background(), fUserID, fWorkspaceID, "nosuchsession0000")
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("expected typed gone, got %v", err)
	}
}

func TestContract_ListSessions_ScansDirWhileDown(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.seedFile("bbbb0001-cccc-dddd-eeee-ffff00001111", "testdata/rich_session.jsonl")
	ft.setDown() // no RPC at all
	out, err := a.ListSessions(context.Background(), fUserID, fWorkspaceID)
	if err != nil {
		t.Fatalf("ListSessions while down: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 sessions from dir scan, got %d", len(out))
	}
	byID := map[string]session.Session{}
	for _, s := range out {
		byID[s.ID] = s
	}
	rich, ok := byID["bbbb0001-cccc-dddd-eeee-ffff00001111"]
	if !ok {
		t.Fatalf("rich session missing: %+v", out)
	}
	if rich.Title != "Rich fixture session" {
		t.Fatalf("rich title (session_info name): %q", rich.Title)
	}
	if rich.WorkspaceID != fWorkspaceID {
		t.Fatalf("workspace stamp: %q", rich.WorkspaceID)
	}
	if rich.Status != session.StatusIdle {
		t.Fatalf("status: %q", rich.Status)
	}
}

func TestContract_CountMessages_FileScanWhileDown(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.seedFile("bbbb0001-cccc-dddd-eeee-ffff00001111", "testdata/rich_session.jsonl")
	ft.setDown()
	n, err := a.CountMessages(context.Background(), fUserID, fWorkspaceID, "bbbb0001-cccc-dddd-eeee-ffff00001111")
	if err != nil {
		t.Fatalf("CountMessages while down: %v", err)
	}
	// rich_session active branch, platform-visible messages:
	// model_switch(m1) user(e3) assistant(e4, fused tools) assistant(e7)
	// shell(e8) compaction(c9) model_switch(m10) custom-msg(e12) user(e13)
	// assistant(e14) = 10 (toolResults fused; custom state + session_info
	// entries skipped).
	if n != 10 {
		t.Fatalf("CountMessages: got %d want 10", n)
	}
	// Must equal the transcript length (the ground-truth contract).
	hist, err := a.fileTranscript(context.Background(), fUserID, fWorkspaceID, "bbbb0001-cccc-dddd-eeee-ffff00001111")
	if err != nil {
		t.Fatalf("fileTranscript: %v", err)
	}
	if n != len(hist) {
		t.Fatalf("count %d != transcript %d", n, len(hist))
	}
}

func TestContract_RenameDelete(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()
	if err := a.RenameSession(ctx, fUserID, fWorkspaceID, fixtureSessionID, "renamed"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if got := ft.lastCommand(fixtureSessionID, "set_session_name"); got == nil || got["name"] != "renamed" {
		t.Fatalf("rename command: %v", got)
	}
	if err := a.DeleteSession(ctx, fUserID, fWorkspaceID, fixtureSessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := a.GetSession(ctx, fUserID, fWorkspaceID, fixtureSessionID); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("post-delete GetSession: %v", err)
	}
	// Deleting again surfaces the typed reap verdict (#1479).
	if err := a.DeleteSession(ctx, fUserID, fWorkspaceID, fixtureSessionID); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("re-delete verdict: %v", err)
	}
}

// --- Messaging ----------------------------------------------------------

func TestContract_Send_SyncWaitsForSettle(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "remember the number 42", session.SendOpts{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.Type != session.MessageAssistant {
		t.Fatalf("Send result type: %q", m.Type)
	}
	if m.Text != "" && m.Text != "persisted reply" {
		t.Fatalf("unexpected text: %q", m.Text)
	}
	// The prompt command carried followUp streamingBehavior (the
	// safe-during-streaming default).
	cmd := ft.lastCommand(fixtureSessionID, "prompt")
	if cmd["message"] != "remember the number 42" || cmd["streamingBehavior"] != "followUp" {
		t.Fatalf("prompt command: %v", cmd)
	}
}

func TestContract_Send_AdmissionMapsToQueueSteer(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "steer me", session.SendOpts{Admission: session.AdmissionSteer}); err != nil {
		t.Fatalf("steer Send: %v", err)
	}
	if got := ft.lastCommand(fixtureSessionID, "steer"); got == nil || got["message"] != "steer me" {
		t.Fatalf("steer command: %v", got)
	}
	if _, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "queue me", session.SendOpts{Admission: session.AdmissionQueue}); err != nil {
		t.Fatalf("queue Send: %v", err)
	}
	if got := ft.lastCommand(fixtureSessionID, "follow_up"); got == nil || got["message"] != "queue me" {
		t.Fatalf("follow_up command: %v", got)
	}
}

func TestContract_SendAsync_ReturnsDurableEntryID(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := a.SendAsync(ctx, fUserID, fWorkspaceID, fixtureSessionID, "remember the number 42", session.SendOpts{})
	if err != nil {
		t.Fatalf("SendAsync: %v", err)
	}
	if id == "" {
		t.Fatal("SendAsync returned no correlation id for a started run")
	}
	// The id must be a REAL persisted entry id (durable cursor truth).
	hist, err := a.GetHistory(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	found := false
	for _, m := range hist {
		if m.ID == id && m.Type == session.MessageUser {
			found = true
		}
	}
	if !found {
		t.Fatalf("correlation id %q is not a persisted user entry id", id)
	}
}

func TestContract_Abort_PreservesQueuedInput(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()
	// Arm clear_queue to return a drained queue (the pi contract's
	// returned texts).
	ft.mu.Lock()
	ft.drainResponse = []string{"queued work A", "queued work B"}
	ft.mu.Unlock()
	if err := a.Abort(ctx, fUserID, fWorkspaceID, fixtureSessionID); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	cmds := ft.commandsOf(fixtureSessionID)
	// The dance: clear_queue BEFORE abort, then one follow_up re-queue
	// per drained text (survives to the NEXT wake).
	var clearIdx, abortIdx int
	clearIdx, abortIdx = -1, -1
	for i, c := range cmds {
		if c == "clear_queue" && clearIdx < 0 {
			clearIdx = i
		}
		if c == "abort" && abortIdx < 0 {
			abortIdx = i
		}
	}
	if clearIdx < 0 || abortIdx < 0 || clearIdx > abortIdx {
		t.Fatalf("expected clear_queue before abort, got %v", cmds)
	}
	requeues := 0
	for _, c := range cmds {
		if c == "follow_up" {
			requeues++
		}
	}
	if requeues != 2 {
		t.Fatalf("expected 2 re-queued follow_ups, got %d (%v)", requeues, cmds)
	}
}

func TestContract_SetSessionModel(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()
	if err := a.SetSessionModel(ctx, fUserID, fWorkspaceID, fixtureSessionID, &session.ModelRef{Provider: "fixture", ID: "fixture-model"}); err != nil {
		t.Fatalf("SetSessionModel: %v", err)
	}
	cmd := ft.lastCommand(fixtureSessionID, "set_model")
	if cmd["provider"] != "fixture" || cmd["modelId"] != "fixture-model" {
		t.Fatalf("set_model: %v", cmd)
	}
	// Slashed bare form splits on the first segment.
	if err := a.SetSessionModel(ctx, fUserID, fWorkspaceID, fixtureSessionID, &session.ModelRef{ID: "fixture/fixture-model"}); err != nil {
		t.Fatalf("slashed ref: %v", err)
	}
	cmd = ft.lastCommand(fixtureSessionID, "set_model")
	if cmd["provider"] != "fixture" {
		t.Fatalf("slashed set_model: %v", cmd)
	}
	// Unexpressible flat id errors; nil is a no-op.
	if err := a.SetSessionModel(ctx, fUserID, fWorkspaceID, fixtureSessionID, &session.ModelRef{ID: "flat"}); err == nil {
		t.Fatal("flat id should error (pi needs provider+model)")
	}
	if err := a.SetSessionModel(ctx, fUserID, fWorkspaceID, fixtureSessionID, nil); err != nil {
		t.Fatalf("nil ref should no-op: %v", err)
	}
	// Send applies the model BEFORE delivery (the per-prompt override
	// cannot ride pi's prompt command).
	_, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "hi", session.SendOpts{Model: &session.ModelRef{Provider: "p", ID: "m"}})
	if err != nil {
		t.Fatalf("Send with model: %v", err)
	}
	cmds := ft.commandsOf(fixtureSessionID)
	var setIdx, promptIdx int
	setIdx, promptIdx = -1, -1
	for i, c := range cmds {
		if c == "set_model" && setIdx < 0 {
			setIdx = i
		}
		if c == "prompt" && promptIdx < 0 {
			promptIdx = i
		}
	}
	if setIdx < 0 || promptIdx < 0 || setIdx > promptIdx {
		t.Fatalf("model must be set before prompt: %v", cmds)
	}
}

// --- VerifyDelivery -----------------------------------------------------

func TestContract_VerifyDelivery_FileScanWhileDown(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()
	// Deliver a message (persists the entry), THEN take pi down.
	if _, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "verify me please", session.SendOpts{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ft.setDown()

	delivered, definitive, err := a.VerifyDelivery(ctx, fUserID, fWorkspaceID, fixtureSessionID, "verify me please", time.Now().Add(-time.Minute))
	if err != nil || !delivered || !definitive {
		t.Fatalf("delivered verdict: (%v,%v,%v)", delivered, definitive, err)
	}

	// Absence is definitive: a text never sent cannot be re-sent.
	delivered, definitive, err = a.VerifyDelivery(ctx, fUserID, fWorkspaceID, fixtureSessionID, "never sent text", time.Now().Add(-time.Minute))
	if err != nil || delivered || !definitive {
		t.Fatalf("absent verdict: (%v,%v,%v)", delivered, definitive, err)
	}

	// A floor AFTER the delivery excludes it (clock-skew window aside,
	// the scan honors `since`).
	delivered, definitive, err = a.VerifyDelivery(ctx, fUserID, fWorkspaceID, fixtureSessionID, "remember the number 42", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("future-floor scan: %v", err)
	}
	if delivered && definitive {
		t.Fatal("a floor after the persisted timestamp must not match")
	}

	// Gone session: definitive absence (cannot duplicate onto nothing).
	delivered, definitive, err = a.VerifyDelivery(ctx, fUserID, fWorkspaceID, "nosuchsession0000", "x", time.Now())
	if err != nil || delivered || !definitive {
		t.Fatalf("gone verdict: (%v,%v,%v)", delivered, definitive, err)
	}
}

func TestContract_VerifyDelivery_OriginalFixtureUserMessage(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.setDown()
	// The RECORDED session's persisted user message (from the real
	// capture): proves the scan reads the real artifact.
	delivered, definitive, err := a.VerifyDelivery(context.Background(), fUserID, fWorkspaceID, fixtureSessionID, "remember the number 42", time.Time{})
	if err != nil || !delivered || !definitive {
		t.Fatalf("recorded delivery: (%v,%v,%v)", delivered, definitive, err)
	}
}

// --- History ------------------------------------------------------------

func TestContract_GetHistory_RecordedSession(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	hist, err := a.GetHistory(context.Background(), fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(hist) == 0 {
		t.Fatal("empty history from recorded fixture")
	}
	var userMsg, asstMsg *session.Message
	for i := range hist {
		if hist[i].Type == session.MessageUser && userMsg == nil {
			userMsg = &hist[i]
		}
		if hist[i].Type == session.MessageAssistant && asstMsg == nil {
			asstMsg = &hist[i]
		}
	}
	if userMsg == nil || userMsg.Text != "remember the number 42" {
		t.Fatalf("user message: %+v", userMsg)
	}
	if userMsg.ID != "4acb1be5" {
		t.Fatalf("user entry id surfaced verbatim: %q", userMsg.ID)
	}
	if asstMsg == nil || asstMsg.ID != "6cd91035" {
		t.Fatalf("assistant entry id: %+v", asstMsg)
	}
	if asstMsg.Model == nil || asstMsg.Model.ID != "fixture-model" {
		t.Fatalf("assistant model: %+v", asstMsg.Model)
	}
	if asstMsg.Cost == nil || asstMsg.Cost.InputTokens != 111 || asstMsg.Cost.OutputTokens != 7 {
		t.Fatalf("assistant cost from recorded usage: %+v", asstMsg.Cost)
	}
	if len(asstMsg.Parts) != 1 || asstMsg.Parts[0].Type != session.PartText || asstMsg.Parts[0].Text != "persisted reply " {
		t.Fatalf("assistant parts: %+v", asstMsg.Parts)
	}
}

func TestContract_GetHistoryPage_RidesTheDurableCursor(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx := context.Background()

	// Cold read: full get_entries (no since).
	page1, err := a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10)
	if err != nil {
		t.Fatalf("cold page: %v", err)
	}
	if cold := ft.lastCommand(fixtureSessionID, "get_entries"); cold == nil {
		t.Fatal("cold read must issue get_entries")
	} else if _, has := cold["since"]; has {
		t.Fatalf("cold read must not carry since: %v", cold)
	}

	// State change: a new delivery appends entries.
	if _, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "second turn", session.SendOpts{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Warm read: the SAME get_entries count grows by one — and the
	// newest command carries since (the durable cursor path).
	before := len(ft.commandsOf(fixtureSessionID))
	page2, err := a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10)
	if err != nil {
		t.Fatalf("warm page: %v", err)
	}
	after := ft.commandsOf(fixtureSessionID)
	if len(after) != before+1 {
		t.Fatalf("warm read must be exactly one get_entries: %v", after)
	}
	warm := ft.lastCommand(fixtureSessionID, "get_entries")
	if warm == nil || warm["since"] == nil || warm["since"] == "" {
		t.Fatalf("warm read must ride the since cursor: %v", warm)
	}
	if len(page2) != len(page1)+2 { // user + assistant entries appended
		t.Fatalf("page grew %d → %d", len(page1), len(page2))
	}
	// Newest-limit slicing, oldest-first within the page.
	if len(page2) > 0 && page2[len(page2)-1].Type != session.MessageAssistant {
		t.Fatalf("page tail should be the newest assistant message: %+v", page2[len(page2)-1])
	}

	// limit slicing on the warm path.
	one, err := a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 1)
	if err != nil {
		t.Fatalf("limit-1 page: %v", err)
	}
	if len(one) != 1 || one[0].ID != page2[len(page2)-1].ID {
		t.Fatalf("limit-1 mismatch: %+v", one)
	}
}

// --- Stream -------------------------------------------------------------

func TestContract_Stream_TranslatesTheRun(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	evCh, err := a.Stream(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []session.Event
	collect := func() {
		for {
			select {
			case ev, ok := <-evCh:
				if !ok {
					return
				}
				got = append(got, ev)
			default:
				return
			}
		}
	}
	collect()

	// Drive a run (the fixture replays the whole event script).
	if _, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "stream me", session.SendOpts{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Drain until the settled status lands.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				goto done
			}
			got = append(got, ev)
			if ev.Type == session.EventSessionStatus && ev.Status == session.StatusIdle {
				goto done
			}
		case <-deadline:
			t.Fatal("stream never settled")
		case <-time.After(10 * time.Millisecond):
			collect()
		}
	}
done:
	byType := map[session.EventType]int{}
	for _, ev := range got {
		byType[ev.Type]++
		if ev.SessionID != fixtureSessionID {
			t.Fatalf("event missing session stamp: %+v", ev)
		}
	}
	// The lifecycle spine of the recorded script.
	for _, want := range []session.EventType{session.EventSessionStatus, session.EventMessageStart, session.EventMessageEnd,
		session.EventPartStart, session.EventPartDelta, session.EventPartEnd} {
		if byType[want] == 0 {
			t.Fatalf("missing %s in stream (have %v)", want, byType)
		}
	}
	// Tool lifecycle: part.start (toolcall_start) + running/completed
	// snapshots from tool_execution_*.
	var toolEnd *session.Part
	for i := range got {
		if got[i].Type == session.EventPartEnd && got[i].Part != nil && got[i].Part.Type == session.PartTool &&
			got[i].Part.Tool != nil && got[i].Part.Tool.CallID == "call_bash1" &&
			got[i].Part.Tool.State.Status == session.ToolStatusCompleted {
			toolEnd = got[i].Part
		}
	}
	if toolEnd == nil {
		t.Fatal("no completed tool part for call_bash1 (tool_execution_end fusion)")
	}
	if toolEnd.Tool.Name != "bash" {
		t.Fatalf("tool name: %q", toolEnd.Tool.Name)
	}
	// Output is the tool_execution_end result payload verbatim (the
	// recorded shape: {"content":[{"type":"text","text":...}],"details":{}}).
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(toolEnd.Tool.Output, &res); err != nil || len(res.Content) == 0 || res.Content[0].Text != "fixture-file" {
		t.Fatalf("tool output not the result content: %v (%s)", err, toolEnd.Tool.Output)
	}
	// Text deltas accumulated across two part.delta events.
	deltas := 0
	for _, ev := range got {
		if ev.Type == session.EventPartDelta && ev.PartID != "" {
			deltas++
		}
	}
	if deltas < 2 {
		t.Fatalf("expected streamed deltas, got %d", deltas)
	}
	// Compaction surfaced as a transcript message pair.
	var compaction bool
	for _, ev := range got {
		if ev.Type == session.EventMessageEnd && ev.Message != nil && ev.Message.Type == session.MessageCompaction {
			compaction = true
		}
	}
	if !compaction {
		t.Fatal("compaction_end did not surface a compaction message")
	}
	// session_info_changed → session.updated with the title.
	var renamed bool
	for _, ev := range got {
		if ev.Type == session.EventSessionUpdated && ev.Session != nil && ev.Session.Title == "fixture session name" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatal("session_info_changed did not surface session.updated title")
	}
	// extension_error surfaced as an error event.
	var extErr bool
	for _, ev := range got {
		if ev.Type == session.EventError && strings.Contains(ev.Error.Message, "/tmp/ext.ts") {
			extErr = true
		}
	}
	if !extErr {
		t.Fatal("extension_error not surfaced")
	}
}

func TestContract_Stream_UnknownRecordsDroppedSilently(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	evCh, err := a.Stream(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	ft.broadcast(fixtureSessionID, []byte(`{"type":"brand_new_event","payload":{"x":1}}`))
	ft.broadcast(fixtureSessionID, []byte(`{not json`))
	select {
	case ev := <-evCh:
		t.Fatalf("unknown/malformed records must be dropped, got %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// --- Input surface (the §1.5 gap) ---------------------------------------

func TestContract_NoAskPermissionAffordances(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	ctx := context.Background()

	caps := a.Capabilities()
	// Every capability the platform can render; none of them is an
	// ask/permission affordance (CapSteer/CapQueue/CapReasoning only).
	want := map[session.Capability]bool{session.CapSteer: true, session.CapQueue: true, session.CapReasoning: true}
	if len(caps) != len(want) {
		t.Fatalf("capabilities: %v", caps)
	}
	for _, c := range caps {
		if !want[c] {
			t.Fatalf("unexpected capability %q (ask/permission affordances are ruled out, §1.5)", c)
		}
	}

	pending, err := a.ListPending(ctx, fUserID, fWorkspaceID, fixtureSessionID)
	if err != nil {
		t.Fatalf("ListPending must be an authoritative empty (nil error), got %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending: %v", pending)
	}
	if err := a.RejectInput(ctx, fUserID, fWorkspaceID, "req-1"); !errors.Is(err, ErrInputNotSupported) {
		t.Fatalf("RejectInput: %v", err)
	}
	if err := a.AnswerQuestion(ctx, fUserID, fWorkspaceID, "req-1", [][]string{{"a"}}); !errors.Is(err, ErrInputNotSupported) {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if err := a.ReplyPermission(ctx, fUserID, fWorkspaceID, "req-1", "once", ""); !errors.Is(err, ErrInputNotSupported) {
		t.Fatalf("ReplyPermission: %v", err)
	}
}

// --- Models -------------------------------------------------------------

func TestContract_ListAvailableModels_RecordedCatalog(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	models, err := a.ListAvailableModels(context.Background(), fUserID, fWorkspaceID)
	if err != nil {
		t.Fatalf("ListAvailableModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("catalog: %+v", models)
	}
	m := models[0]
	if m.ID != "fixture-model" || m.Provider != "fixture" || m.ContextWindow != 200000 || m.MaxOutput != 4096 || m.DisplayName != "fixture-model" {
		t.Fatalf("model info: %+v", m)
	}
}

// --- Raw-record decode surface -----------------------------------------

func TestContract_RawEventDecodeSurface(t *testing.T) {
	a, _ := newFixtureAdapter(t)

	// ContextUsageFromEvent: message_update with usage + injected id.
	rec := injectSessionID([]byte(`{"type":"message_update","usage":{"input":900,"output":100,"cacheRead":1000,"cacheWrite":50},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"}}`), "sess-1")
	sid, usage, ok := a.ContextUsageFromEvent("message_update", string(rec))
	if !ok || sid != "sess-1" || usage == nil || usage.Used != 1950 {
		t.Fatalf("ContextUsageFromEvent: (%q,%v,%v)", sid, usage, ok)
	}
	// Hot-path rejection without parse.
	if _, _, ok := a.ContextUsageFromEvent("agent_start", "{}"); ok {
		t.Fatal("non-usage event must not be ok")
	}
	// Contract upholds: ok=true requires non-empty session id.
	if _, _, ok := a.ContextUsageFromEvent("message_update", `{"type":"message_update","usage":{"input":5}}`); ok {
		t.Fatal("usage without injected sessionId must not be ok")
	}

	// RetryFromEvent.
	sid, retry, ok := a.RetryFromEvent("auto_retry_start", string(injectSessionID([]byte(`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"529 overloaded"}`), "sess-2")))
	if !ok || sid != "sess-2" || retry == nil || retry.Attempt != 1 || retry.Action != "retry" || retry.NextAt == 0 || !strings.Contains(retry.Message, "1/3") {
		t.Fatalf("RetryFromEvent start: (%q,%+v,%v)", sid, retry, ok)
	}
	_, retry, ok = a.RetryFromEvent("auto_retry_end", string(injectSessionID([]byte(`{"type":"auto_retry_end","success":false,"finalError":"boom"}`), "sess-2")))
	if !ok || retry.Action != "failed" || retry.Message != "boom" {
		t.Fatalf("RetryFromEvent end: %+v", retry)
	}

	// ClientEventsFromEvent: text delta + unknown type dropped.
	evs := a.ClientEventsFromEvent("message_update", `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`)
	if len(evs) != 1 || evs[0].Type != session.EventPartDelta || evs[0].Delta != "hi" {
		t.Fatalf("ClientEventsFromEvent: %+v", evs)
	}
	if evs := a.ClientEventsFromEvent("totally_new", `{"type":"totally_new"}`); evs != nil {
		t.Fatalf("unknown type must yield nil, got %+v", evs)
	}
}

// --- Credentials --------------------------------------------------------

func TestContract_Credentials(t *testing.T) {
	a := NewAdapter(newFixtureTransport(t), nil)
	raw, err := a.FormatProviderConfig([]agent.LLMProviderData{{
		Kind: "openai-compatible", Slug: "acme", APIKey: "sk-x", BaseURL: "https://acme.example/v1",
		Models: []agent.LLMModelConfig{{ID: "acme-1", ContextLimit: 128000, OutputLimit: 8192}},
	}})
	if err != nil {
		t.Fatalf("FormatProviderConfig: %v", err)
	}
	res, err := a.ValidateCredentials(raw)
	if err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
	if res.State != agent.CredentialStatePresent || res.Agent != agent.AgentTypePi {
		t.Fatalf("result: %+v", res)
	}
	missing, _ := a.ValidateCredentials(nil)
	if missing.State != agent.CredentialStateMissing {
		t.Fatalf("missing: %+v", missing)
	}
	invalid, _ := a.ValidateCredentials([]byte("{nope"))
	if invalid.State != agent.CredentialStateInvalid {
		t.Fatalf("invalid: %+v", invalid)
	}
}

// --- Registry -----------------------------------------------------------

func TestContract_Register(t *testing.T) {
	Register()
	rt, err := agent.Get(agent.AgentTypePi)
	if err != nil {
		t.Fatalf("Get(pi): %v", err)
	}
	if rt.Type() != agent.AgentTypePi {
		t.Fatalf("type: %q", rt.Type())
	}
}

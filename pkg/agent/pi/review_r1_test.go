// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// review_r1_test.go — the r1 CHANGES_REQUESTED findings, each with the
// reproducing test the reviewer required:
//
//  1. rewind-then-append warm GetHistoryPage (the ghost-leak repro,
//     history.go's branch-continuation guard).
//  2. mid-stream message_end without message_start mints an id (the
//     self-pass fix, mutation-caught this time).
//  3. validateSessionID rejection paths (ids become file names and
//     spawn args — the path-safety guard).
//  4. Abort unhappy paths: clear_queue failure tolerated; abort
//     failure after drain restores the queue.
//  5. Send unhappy paths: missing disposition on steer; mid-run stream
//     error; disposition=handled and disposition=queued verdicts.
//  6. rune-safe title truncation (multi-byte first user message).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// scriptedClient answers get_entries from canned data (ringDelta unit
// tests — no fixture transport needed).
type scriptedClient struct {
	responses map[string]piEntriesData // since → data
}

func (s *scriptedClient) Call(ctx context.Context, command map[string]any) (json.RawMessage, error) {
	if command["type"] != "get_entries" {
		return nil, fmt.Errorf("scriptedClient: unexpected command %v", command["type"])
	}
	since, _ := command["since"].(string)
	d, ok := s.responses[since]
	if !ok {
		return nil, &piCallError{command: "get_entries", message: "since not found"}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`{"type":"response","command":"get_entries","success":true,"data":` + string(raw) + `}`), nil
}

func (s *scriptedClient) Events(ctx context.Context) (<-chan EventRecord, error) {
	ch := make(chan EventRecord)
	return ch, nil
}

// 1. The r1 ghost-leak repro: pi's rewind is a leaf MOVE over an
// append-only file. After rewind-to-user + append, the delta's tail IS
// the new leaf, so the pre-fix linear-trust shortcut extended the
// stale window and the abandoned assistant leaked into the warm page.
func TestReviewR1_RewindThenAppend_RebuildsInsteadOfLeaking(t *testing.T) {
	mk := func(id, parent, role, text string) piEntry {
		e := piEntry{ID: id, Type: "message", Message: &piAgentMessage{Role: role, Content: []byte(`"` + text + `"`)}}
		if parent != "" {
			e.ParentID = &parent
		}
		return e
	}
	// Window state after a cold read of [model_switch?, user, assistant]:
	ring := &historyRing{
		cursor: "a1", // the assistant — append tail == leaf
		leafID: "a1",
		msgs: []session.Message{
			session.UserMessage("u1", "go left", time.Time{}),
			session.AssistantMessage("a1", nil, time.Time{}),
		},
	}
	// The rewind: pi moves the leaf back to u1, then the user appends
	// u2 from there. get_entries since=a1 returns ONLY u2 (append
	// order), leafId=u2 — the abandoned a1 is before the cursor.
	client := &scriptedClient{responses: map[string]piEntriesData{
		"": { // the rebuild fetch: the whole tree, active branch u1→u2
			Entries: []piEntry{mk("u1", "", "user", "go left"), mk("a1", "u1", "assistant", "ghost"), mk("u2", "u1", "user", "go right")},
			LeafID:  strPtr("u2"),
		},
		"a1": { // the delta after the cursor
			Entries: []piEntry{mk("u2", "u1", "user", "go right")},
			LeafID:  strPtr("u2"),
		},
	}}
	a := NewAdapter(nil, nil)
	msgs, fresh, err := a.ringDelta(context.Background(), client, ring)
	if err == nil || fresh != nil {
		t.Fatalf("rewind-then-append must be classified stale, got (%v,%v)", msgs, err)
	}
	if !errors.Is(err, errCursorStale) {
		t.Fatalf("expected errCursorStale, got %v", err)
	}

	// And historyPage converts the stale verdict into a full rebuild:
	// the warm page must show the ACTIVE branch, ghost-free.
	ft := newFixtureTransport(t)
	adapter := NewAdapter(ft, nil)
	ctx := context.Background()
	if _, err := adapter.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Simulate the rewind+append directly on the persisted file: append
	// a user entry whose parent is the FIRST user entry (u=4acb1be5),
	// not the current tail (appendEntryRaw keeps the caller's parent).
	ft.appendEntryRaw(fixtureSessionID, map[string]any{
		"type": "message", "id": "rewound1", "parentId": "4acb1be5",
		"timestamp": "2026-11-13T13:00:00.000Z",
		"message":   map[string]any{"role": "user", "content": "after rewind", "timestamp": 1794554401000},
	})
	page, err := adapter.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10)
	if err != nil {
		t.Fatalf("warm page after rewind: %v", err)
	}
	var ghost bool
	for _, m := range page {
		if m.ID == "6cd91035" {
			ghost = true // the abandoned assistant
		}
	}
	if ghost {
		t.Fatalf("abandoned-branch assistant leaked into the warm page: %+v", page)
	}
	found := false
	for _, m := range page {
		if m.ID == "rewound1" && m.Text == "after rewind" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-rewind append missing from the warm page: %+v", page)
	}
}

// 2. The self-pass fix's missing regression pin: message_end on a
// fresh translator (subscription started mid-message) must mint an id.
func TestReviewR1_MidStreamMessageEnd_MintsID(t *testing.T) {
	tr := newStreamTranslator()
	evs := clientEvents(piEvent{Type: evMessageEnd, Message: []byte(`{"role":"user","content":"hi","timestamp":1}`)}, "sess", time.Now(), tr)
	if len(evs) != 1 || evs[0].Type != session.EventMessageEnd {
		t.Fatalf("events: %+v", evs)
	}
	if evs[0].MessageID == "" {
		t.Fatal("mid-stream message_end left an empty MessageID")
	}
	if evs[0].Message == nil || evs[0].Message.ID == "" {
		t.Fatalf("mid-stream message_end left an empty Message.ID: %+v", evs[0].Message)
	}
	// Same guarantee on the stateless raw path.
	a := NewAdapter(nil, nil)
	evs = a.ClientEventsFromEvent("message_end", `{"type":"message_end","message":{"role":"user","content":"hi","timestamp":1}}`)
	if len(evs) != 1 || evs[0].MessageID == "" {
		t.Fatalf("stateless path ids: %+v", evs)
	}
}

// TestReviewR2_StatelessIDsOnEveryEvent pins the adapter-level
// beginMessage mint (adapter.go's ClientEventsFromEvent): EVERY
// stateless event class that carries ids — message_update's part
// events (the majority of emitted traffic) and the tool_execution_*
// part snapshots, not just message_end's self-mint — must carry a
// non-empty MessageID/PartID. Red without the mint (r2's
// mutation-verified gap).
func TestReviewR2_StatelessIDsOnEveryEvent(t *testing.T) {
	a := NewAdapter(nil, nil)

	// message_update text_delta → part.delta with both ids.
	evs := a.ClientEventsFromEvent("message_update", `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`)
	if len(evs) != 1 {
		t.Fatalf("delta events: %+v", evs)
	}
	if evs[0].MessageID == "" || evs[0].PartID == "" {
		t.Fatalf("stateless part.delta carries empty ids: %+v", evs[0])
	}

	// message_update toolcall_start → part.start with both ids.
	evs = a.ClientEventsFromEvent("message_update", `{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","contentIndex":1,"id":"call_x","toolName":"bash"}}`)
	if len(evs) != 1 || evs[0].MessageID == "" || evs[0].PartID == "" {
		t.Fatalf("stateless toolcall_start ids: %+v", evs)
	}

	// tool_execution_end → part.end snapshot with both ids (the
	// mint-on-the-fly path for calls with no streamed block).
	evs = a.ClientEventsFromEvent("tool_execution_end", `{"type":"tool_execution_end","toolCallId":"call_y","toolName":"bash","result":{"content":[]},"isError":false}`)
	if len(evs) != 1 || evs[0].MessageID == "" || evs[0].PartID == "" {
		t.Fatalf("stateless tool_execution_end ids: %+v", evs)
	}
	if evs[0].Part == nil || evs[0].Part.Tool == nil || evs[0].Part.Tool.CallID != "call_y" {
		t.Fatalf("stateless tool snapshot: %+v", evs[0].Part)
	}
}

// 3. validateSessionID: the guard in front of every file name and
// spawn arg.
func TestReviewR1_ValidateSessionID(t *testing.T) {
	for _, bad := range []string{"", "../escape", "with/slash", "with\\backslash", "with space", "séance", "with.dot", "..", ".", strings.Repeat("a", 129)} {
		if err := validateSessionID(bad); err == nil {
			t.Errorf("validateSessionID(%q) must reject", bad)
		}
	}
	for _, good := range []string{"a", "aaaabbbb-cccc-dddd-eeee-ffff00001111", "with_underscore", "with-dash", strings.Repeat("a", 128)} {
		if err := validateSessionID(good); err != nil {
			t.Errorf("validateSessionID(%q) must accept: %v", good, err)
		}
	}
}

// 4a. clear_queue failure is tolerated: abort still stops the work.
func TestReviewR1_Abort_ClearQueueFailureTolerated(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.armFail("clear_queue", errors.New("queue surface unavailable"))
	if err := a.Abort(context.Background(), fUserID, fWorkspaceID, fixtureSessionID); err != nil {
		t.Fatalf("Abort must succeed when clear_queue fails: %v", err)
	}
	cmds := ft.commandsOf(fixtureSessionID)
	var aborted, requeued bool
	for _, c := range cmds {
		if c == "abort" {
			aborted = true
		}
		if c == "follow_up" {
			requeued = true
		}
	}
	if !aborted {
		t.Fatalf("abort not issued after clear_queue failure: %v", cmds)
	}
	if requeued {
		t.Fatalf("nothing was drained; nothing may be re-queued: %v", cmds)
	}
}

// 4b. abort failure AFTER a drain restores the queue (input must not
// vanish on a failed command) and surfaces the error.
func TestReviewR1_Abort_AbortFailureAfterDrainRestoresQueue(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.mu.Lock()
	ft.drainResponse = []string{"precious input"}
	ft.mu.Unlock()
	ft.armFail("abort", errors.New("child wedged"))
	err := a.Abort(context.Background(), fUserID, fWorkspaceID, fixtureSessionID)
	if err == nil {
		t.Fatal("Abort must surface the abort failure")
	}
	cmds := ft.commandsOf(fixtureSessionID)
	var requeued bool
	for _, c := range cmds {
		if c == "follow_up" {
			requeued = true
		}
	}
	if !requeued {
		t.Fatalf("drained input was not restored after abort failure: %v", cmds)
	}
	// The restored text rides the follow_up verbatim.
	if cmd := ft.lastCommand(fixtureSessionID, "follow_up"); cmd == nil || cmd["message"] != "precious input" {
		t.Fatalf("restore payload: %v", cmd)
	}
}

// 5a. A steer response without a disposition is an error (the wire
// contract says every successful steer carries one).
func TestReviewR1_Send_SteerWithoutDispositionErrors(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	// Override the envelope with a bare success (no data at all).
	ft.armBareSuccess("steer")
	_, err := a.Send(context.Background(), fUserID, fWorkspaceID, fixtureSessionID, "steer me", session.SendOpts{Admission: session.AdmissionSteer})
	if err == nil || !strings.Contains(err.Error(), "disposition") {
		t.Fatalf("expected no-disposition error, got %v", err)
	}
}

// 5b. A stream error mid-wait fails the sync Send (never a silent
// partial answer).
func TestReviewR1_Send_StreamErrorFailsSend(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.armStreamError(errors.New("child stdout died"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.Send(ctx, fUserID, fWorkspaceID, fixtureSessionID, "hello", session.SendOpts{})
	if err == nil || !strings.Contains(err.Error(), "stdout died") {
		t.Fatalf("expected stream error to fail Send, got %v", err)
	}
}

// 5c. disposition=handled: no run, no wait — the honest system message.
func TestReviewR1_Send_HandledDisposition(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.armDisposition("handled")
	m, err := a.Send(context.Background(), fUserID, fWorkspaceID, fixtureSessionID, "/command", session.SendOpts{})
	if err != nil {
		t.Fatalf("handled Send: %v", err)
	}
	if m == nil || m.Type != session.MessageSystem {
		t.Fatalf("handled message: %+v", m)
	}
}

// 5d. disposition=queued: SendAsync answers "" (queued messages have
// no persisted entry id yet — the documented contract).
func TestReviewR1_SendAsync_QueuedDispositionEmptyCorrelation(t *testing.T) {
	a, ft := newFixtureAdapter(t)
	ft.armDisposition("queued")
	id, err := a.SendAsync(context.Background(), fUserID, fWorkspaceID, fixtureSessionID, "later please", session.SendOpts{Admission: session.AdmissionQueue})
	if err != nil {
		t.Fatalf("queued SendAsync: %v", err)
	}
	if id != "" {
		t.Fatalf("queued correlation must be empty (nothing persisted yet), got %q", id)
	}
}

// 6. Rune-safe title truncation.
func TestReviewR1_FirstUserTextRuneTruncation(t *testing.T) {
	sum := &sessionFileSummary{
		LeafID: "u1",
		Entries: []piEntry{
			{ID: "u1", Type: "message", Message: &piAgentMessage{Role: "user", Content: []byte(`"日本語のテキストです"`)}},
		},
	}
	got := sum.title()
	if !isValidUTF8(got) {
		t.Fatalf("title is malformed UTF-8: %q", got)
	}
	if got != "日本語のテキストです" {
		t.Fatalf("short title truncated wrongly: %q", got)
	}
	long := strings.Repeat("日", 100)
	sum.Entries[0].Message = &piAgentMessage{Role: "user", Content: []byte(`"` + long + `"`)}
	got = sum.title()
	if len([]rune(got)) != 80 {
		t.Fatalf("long title not cut at 80 runes: %d", len([]rune(got)))
	}
	if !isValidUTF8(got) {
		t.Fatalf("truncated title is malformed UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// history_test.go — the replay-ring unit pins beyond the contract
// suite: stale-cursor rebuild, capacity trim, and the linear delta
// translation.

import (
	"context"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func TestHistoryRing_StaleCursorFallsBackToFullFetch(t *testing.T) {
	a, _ := newFixtureAdapter(t)
	ctx := context.Background()

	// Prime the ring.
	if _, err := a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Corrupt the ring's cursor to an id the session does not know
	// (fork/rewind): the delta call fails success:false → errCursorStale
	// → full refetch must answer.
	a.ringsMu.Lock()
	for _, r := range a.rings {
		r.cursor = "zzzzzzzz"
	}
	a.ringsMu.Unlock()

	page, err := a.GetHistoryPage(ctx, fUserID, fWorkspaceID, fixtureSessionID, 10)
	if err != nil {
		t.Fatalf("stale-cursor page: %v", err)
	}
	if len(page) != 3 { // model_switch + user + assistant
		t.Fatalf("rebuilt page: %+v", page)
	}
	// The ring was rebuilt with a valid cursor.
	a.ringsMu.Lock()
	r := a.rings[ringKey{fWorkspaceID, fixtureSessionID}]
	a.ringsMu.Unlock()
	if r == nil || r.cursor == "zzzzzzzz" {
		t.Fatalf("ring not rebuilt: %+v", r)
	}
}

func TestHistoryRing_CapacityTrim(t *testing.T) {
	msgs := make([]session.Message, ringCapacity+50)
	for i := range msgs {
		msgs[i] = session.UserMessage(idN(i), "m", timeZero())
	}
	r := trimRing(&historyRing{msgs: msgs})
	if len(r.msgs) != ringCapacity {
		t.Fatalf("trim: %d", len(r.msgs))
	}
	if r.msgs[0].ID != idN(50) {
		t.Fatalf("trim kept the wrong window: %q", r.msgs[0].ID)
	}
}

func TestNewestLimit(t *testing.T) {
	msgs := []session.Message{
		session.UserMessage("a", "1", timeZero()),
		session.UserMessage("b", "2", timeZero()),
		session.UserMessage("c", "3", timeZero()),
	}
	got := newestLimit(msgs, 2)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("newestLimit: %+v", got)
	}
	if got := newestLimit(msgs, 5); len(got) != 3 {
		t.Fatalf("oversize limit: %+v", got)
	}
	if got := newestLimit(msgs, 0); len(got) != 3 {
		t.Fatalf("zero limit: %+v", got)
	}
}

func TestTranslateLinear_DeltaOrder(t *testing.T) {
	entries := []piEntry{
		{ID: "u1", Type: "message", Message: &piAgentMessage{Role: "user", Content: []byte(`"q"`), Timestamp: 1}},
		{ID: "a1", Type: "message", Message: &piAgentMessage{Role: "assistant", Content: []byte(`[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]`), Timestamp: 2}},
		{ID: "r1", Type: "message", Message: &piAgentMessage{Role: "toolResult", ToolCallID: "c1", Content: []byte(`[{"type":"text","text":"out"}]`), Timestamp: 3}},
	}
	msgs := translateLinear(entries)
	if len(msgs) != 2 {
		t.Fatalf("delta msgs: %+v", msgs)
	}
	asst := msgs[1]
	if len(asst.Parts) != 1 || asst.Parts[0].Tool == nil || asst.Parts[0].Tool.State.Status != session.ToolStatusCompleted {
		t.Fatalf("fused delta tool: %+v", asst.Parts)
	}
}

// --- fixture-transcript sanity over the doc-derived tree ---------------

func TestFileTranscript_BranchNotOnActiveLeaf(t *testing.T) {
	// A branch whose entries are NOT on the active chain must not leak
	// into the transcript: build a tree where the leaf moved.
	byID := map[string]*piEntry{
		"u1": {ID: "u1", Message: &piAgentMessage{Role: "user", Content: []byte(`"root"`)}},
		"a1": {ID: "a1", ParentID: strPtr("u1"), Message: &piAgentMessage{Role: "assistant", Content: []byte(`[{"type":"text","text":"left"}]`)}},
		"u2": {ID: "u2", ParentID: strPtr("u1"), Message: &piAgentMessage{Role: "user", Content: []byte(`"right"`)}},
	}
	msgs := translateEntries(collectEntries(byID), "u2")
	if len(msgs) != 2 {
		t.Fatalf("branch transcript: %+v", msgs)
	}
	for _, m := range msgs {
		if m.ID == "a1" {
			t.Fatal("abandoned-branch entry leaked into the active transcript")
		}
	}
}

func collectEntries(byID map[string]*piEntry) []piEntry {
	out := make([]piEntry, 0, len(byID))
	for _, e := range byID {
		out = append(out, *e)
	}
	return out
}

func strPtr(s string) *string { return &s }

func timeZero() time.Time { return time.Unix(0, 0).UTC() }

func idN(i int) string { return "id-" + string(rune('a'+i%26)) + itoa(i/26) }
func itoa(i int) string {
	if i == 0 {
		return ""
	}
	return itoa(i/26) + string(rune('a'+i%26))
}

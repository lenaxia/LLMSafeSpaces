// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"testing"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// feed ingests a sequence of contract events through the injected parser
// fixture (the real translation is tested at the opencode seam).
type eventParser struct {
	events chan *abiv1.Event
}

func (p *eventParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	evt := <-p.events
	return evt, evt != nil, nil
}

func newEventAuthority(t *testing.T, seed map[string]sessionstate.SessionSeed) (*sessionstate.Authority, *eventParser) {
	t.Helper()
	p := &eventParser{events: make(chan *abiv1.Event, 64)}
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      p,
		Store:       seedStore{m: seed},
		Passwords:   []string{"pw"},
	}
	a, err := sessionstate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, p
}

// feed stages a contract event on the parser channel and drives one Ingest.
func feed(t *testing.T, a *sessionstate.Authority, p *eventParser, evt *abiv1.Event) {
	t.Helper()
	p.events <- evt
	a.Ingest([]byte("fixture"))
}

type seedStore struct {
	m map[string]sessionstate.SessionSeed
}

func (s seedStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	out := make(map[string]sessionstate.SessionSeed, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out, nil
}

func statusEvent(sid string, st abiv1.SessionStatus) *abiv1.Event {
	return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: st}
}

func partEvent(t abiv1.EventType, sid, mid, pid, text string) *abiv1.Event {
	evt := &abiv1.Event{Type: t, SessionId: sid, MessageId: mid, PartId: pid}
	switch t {
	case abiv1.EventType_EVENT_TYPE_PART_START, abiv1.EventType_EVENT_TYPE_PART_END:
		evt.Part = &abiv1.Part{Id: pid, Type: abiv1.PartType_PART_TYPE_TEXT, Payload: &abiv1.Part_Text{Text: text}}
	case abiv1.EventType_EVENT_TYPE_PART_DELTA:
		evt.Delta = text
	}
	return evt
}

func inputEvent(id, sid string, kind abiv1.InputKind) *abiv1.Event {
	return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, SessionId: sid,
		Input: &abiv1.InputRequest{Id: id, SessionId: sid, Kind: kind, Question: "Q?"}}
}

// TestProjection_BusyStepBoundaries: busy derives from step-boundary events
// and status; a failed step clears busy (issue #1137).
func TestProjection_BusyStepBoundaries(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	if st := a.State().Sessions["s1"]; st == nil || !st.Busy {
		t.Fatal("busy status event must set busy")
	}
	feed(t, a, p, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_ERROR, SessionId: "s1",
		MessageId: "m1", Error: &abiv1.Error{Code: "step.failed", Message: "x"}})
	if st := a.State().Sessions["s1"]; st == nil || st.Busy {
		t.Fatal("a failed step must clear busy")
	}
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	feed(t, a, p, statusEvent("s2", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	if a.State().Sessions["s2"].Busy != true {
		t.Fatal("per-session busy isolation broken")
	}
}

// TestProjection_OrphanedBusyImpossible: the 2026-08-15 phantom-busy class
// — busy mid-turn, opencode dies (generation change), reseed from store
// truth rebuilds the projection without the orphan (issue #1137).
func TestProjection_OrphanedBusyImpossible(t *testing.T) {
	a, p := newEventAuthority(t, map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "m1", "p1", "partial answer"))
	if st := a.State().Sessions["s1"]; !st.Busy || len(st.InFlightParts) == 0 {
		t.Fatal("pre-conditions: busy with in-flight part")
	}

	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
		t.Fatal(err)
	}
	st := a.State().Sessions["s1"]
	if st == nil {
		t.Fatal("session vanished from projection")
	}
	if st.Busy {
		t.Error("orphaned busy survived the reseed — the phantom-busy class lives")
	}
	if len(st.InFlightParts) != 0 {
		t.Errorf("orphaned in-flight parts survived reseed: %d", len(st.InFlightParts))
	}
}

// TestProjection_QuestionPermissionLifecycle: pending inputs tracked per
// session; resolution clears; reseed restores the pending set from store
// truth (issue #1137).
func TestProjection_QuestionPermissionLifecycle(t *testing.T) {
	a, p := newEventAuthority(t, map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_BUSY, PendingInputs: []*abiv1.InputRequest{
			{Id: "q-reseed", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION},
		}},
	})
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonBoot); err != nil {
		t.Fatal(err)
	}
	if got := a.State().Sessions["s1"].PendingInputs; len(got) != 1 || got[0].GetId() != "q-reseed" {
		t.Fatalf("reseed did not restore pending inputs: %v", got)
	}

	feed(t, a, p, inputEvent("q1", "s1", abiv1.InputKind_INPUT_KIND_QUESTION))
	feed(t, a, p, inputEvent("pm1", "s1", abiv1.InputKind_INPUT_KIND_PERMISSION))
	if got := a.State().Sessions["s1"].PendingInputs; len(got) != 3 {
		t.Fatalf("pending set after two asks = %v, want 3", got)
	}

	feed(t, a, p, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, SessionId: "s1",
		Input: &abiv1.InputRequest{Id: "q1"}})
	if got := a.State().Sessions["s1"].PendingInputs; len(got) != 2 {
		t.Fatalf("resolution did not clear the pending input: %v", got)
	}

	// Store still holds q-reseed → reseed clears the live pair, restores store truth.
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
		t.Fatal(err)
	}
	if got := a.State().Sessions["s1"].PendingInputs; len(got) != 1 || got[0].GetId() != "q-reseed" {
		t.Fatalf("reseed must replace the pending set with store truth: %v", got)
	}
}

// TestProjection_SessionSetLifecycle: sessions enter via events, leave via
// reseed against store truth (the snapshot enumerates the right set).
func TestProjection_SessionSetLifecycle(t *testing.T) {
	a, p := newEventAuthority(t, map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	feed(t, a, p, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s2",
		Session: &abiv1.Session{Id: "s2", Title: "T"}})
	if _, ok := a.State().Sessions["s2"]; !ok {
		t.Fatal("session.created-equivalent event must add s2 to the set")
	}
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonBoot); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.State().Sessions["s2"]; ok {
		t.Error("s2 not in store truth must leave the set on reseed")
	}
	if _, ok := a.State().Sessions["s1"]; !ok {
		t.Error("s1 from store truth must be present")
	}
}

// TestProjection_SnapshotCompleteness: in-flight parts carry partials
// (PART_START then PART_DELTA appends); a snapshot alone renders the turn
// (I12).
func TestProjection_SnapshotCompleteness(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "m1", "p1", ""))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_DELTA, "s1", "m1", "p1", "Hello "))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_DELTA, "s1", "m1", "p1", "world"))

	st := a.State().Sessions["s1"]
	if len(st.InFlightParts) != 1 {
		t.Fatalf("in-flight parts = %d, want 1", len(st.InFlightParts))
	}
	if got := st.InFlightParts[0].GetText(); got != "Hello world" {
		t.Errorf("partial text = %q, want folded deltas %q", got, "Hello world")
	}

	frames, cancel, err := a.Stream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	first := <-frames
	snap := first.GetSnapshot()
	requireSessions := snap.GetSnapshot().GetSessions()
	if len(requireSessions) == 0 {
		t.Fatal("pod snapshot must enumerate sessions")
	}
}

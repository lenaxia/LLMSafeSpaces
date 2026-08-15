// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventTypeCountMatchesExplicitList(t *testing.T) {
	// Design 0049 §4.5 lists these EventType values explicitly. The explicit
	// list is authoritative. This test pins the count at 10; adding or
	// removing an EventType is a contract change requiring a design-doc update.
	all := []EventType{
		EventSessionStatus, EventSessionUpdated,
		EventMessageStart, EventMessageEnd,
		EventPartStart, EventPartDelta, EventPartEnd,
		EventInputRequest, EventInputResolved,
		EventError,
	}
	const want = 10
	if len(all) != want {
		t.Fatalf("EventType explicit list length = %d, want %d", len(all), want)
	}
	seen := make(map[EventType]bool, len(all))
	for _, e := range all {
		if seen[e] {
			t.Fatalf("duplicate EventType %q", e)
		}
		seen[e] = true
	}
}

func TestEventRoundTripAllTypes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	cases := []struct {
		name  string
		event Event
	}{
		{
			name:  "session.status",
			event: Event{Type: EventSessionStatus, Timestamp: now, SessionID: "s1", Status: StatusBusy},
		},
		{
			name:  "session.updated",
			event: Event{Type: EventSessionUpdated, Timestamp: now, SessionID: "s1", Session: &Session{ID: "s1", WorkspaceID: "ws", Status: StatusIdle}},
		},
		{
			name:  "message.start",
			event: Event{Type: EventMessageStart, Timestamp: now, SessionID: "s1", MessageID: "m1", Message: &Message{ID: "m1", Type: MessageAssistant, CreatedAt: &now}},
		},
		{
			name:  "message.end",
			event: Event{Type: EventMessageEnd, Timestamp: now, SessionID: "s1", MessageID: "m1"},
		},
		{
			name:  "part.start",
			event: Event{Type: EventPartStart, Timestamp: now, SessionID: "s1", MessageID: "m1", PartID: "p1", Part: &Part{Type: PartText, ID: "p1", Text: "h"}},
		},
		{
			name:  "part.delta",
			event: Event{Type: EventPartDelta, Timestamp: now, SessionID: "s1", MessageID: "m1", PartID: "p1", Delta: "ello"},
		},
		{
			name:  "part.end",
			event: Event{Type: EventPartEnd, Timestamp: now, SessionID: "s1", MessageID: "m1", PartID: "p1"},
		},
		{
			name: "input.request",
			event: Event{Type: EventInputRequest, Timestamp: now, SessionID: "s1", Input: &InputRequest{
				ID: "in1", Kind: InputQuestion, Question: "which?", Header: "pick",
				Options: []InputOption{{Label: "A"}, {Label: "B"}}, Multiple: false, Custom: true,
				Tool: &ToolRef{MessageID: "m1", CallID: "call_1"},
			}},
		},
		{
			name: "input.permission.request",
			event: Event{Type: EventInputRequest, Timestamp: now, SessionID: "s1", Input: &InputRequest{
				ID: "in2", Kind: InputPermission, Permission: "edit", Patterns: []string{"**/*.go"},
				Always: []string{"edit(**/*.go)"}, Metadata: map[string]json.RawMessage{"cwd": json.RawMessage(`"/ws"`)},
			}},
		},
		{
			name:  "input.resolved",
			event: Event{Type: EventInputResolved, Timestamp: now, SessionID: "s1", Input: &InputRequest{ID: "in1", Kind: InputQuestion}},
		},
		{
			name:  "error",
			event: Event{Type: EventError, Timestamp: now, SessionID: "s1", Error: &Error{Code: "rate_limited", Message: "429"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roundTrip(t, tc.event)
		})
	}
}

func TestEventOmitsEmptyOptionalFields(t *testing.T) {
	e := Event{Type: EventPartDelta, Timestamp: time.Now().UTC(), PartID: "p1", Delta: "x"}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"sessionId", "messageId", "status", "session", "message", "part", "input", "error"} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("optional field %q should omit when unset; got %s", absent, out)
		}
	}
}

func TestSendOptsAndAdmission(t *testing.T) {
	// Design 0049 §4.4: Admission is a delivery mode on SendOpts, not a method.
	roundTrip(t, SendOpts{Model: &ModelRef{ID: "gpt-5"}, Admission: AdmissionSteer})
	roundTrip(t, SendOpts{Admission: AdmissionQueue})
	roundTrip(t, SendOpts{}) // default/immediate send: admission zero-value omits
}

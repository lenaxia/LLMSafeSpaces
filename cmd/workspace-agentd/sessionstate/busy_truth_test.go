// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

// busy_truth_test.go — #1574 (TDD: authored before the derivation).
//
// The owner's rule: busy ⇔ autonomous progress pending — the session
// will do work on its own without user input. Streaming (the only
// thing counted before), tool executions running or queued (in-flight
// parts), and turn machinery in flight (ledger QueueDepth) all count;
// permission/question prompts awaiting an answer are the carve-out
// (blocked on the USER — not busy; pending_inputs is their surface).
//
// The incident's exact shape is pinned first: the harness reported
// IDLE while a bash command waited — every consumer lied. And the
// #1573 divergence: two views with two definitions; here the ONE
// definition lands in the projection, components surfaced so consumers
// render WHY.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// busyOf extracts the derived busy components from the session view.
func busyOf(t *testing.T, a *sessionstate.Authority, sid string) *abiv1.BusyComponents {
	t.Helper()
	st, ok := a.State().Sessions[sid]
	if !ok {
		t.Fatalf("session %s missing from state", sid)
	}
	return st.BusyComponents
}

// TestBusyFromInFlightParts: the incident's exact case — status IDLE
// (the harness's only signal) while a tool part is in flight. Derived
// busy MUST be true and the STATUS must read BUSY; the components say
// why (in_flight_parts=1, streaming=false).
func TestBusyFromInFlightParts(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "m1", "p1", ""))

	st := a.State().Sessions["s1"]
	if st.Status != abiv1.SessionStatus_SESSION_STATUS_BUSY {
		t.Fatalf("IDLE status + in-flight part must render BUSY (the #1574 incident shape), got %v", st.Status)
	}
	b := st.BusyComponents
	if b == nil || !b.GetBusy() {
		t.Fatalf("derived busy must be true with an in-flight part, got %+v", b)
	}
	if b.GetInFlightParts() != 1 {
		t.Fatalf("components: in_flight_parts=1 expected, got %+v", b)
	}
}

// TestBusyClearsWhenPartsAndStreamEnd: IDLE + parts gone → not busy.
func TestBusyClearsWhenPartsAndStreamEnd(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	if b := busyOf(t, a, "s1"); b.GetBusy() {
		t.Fatalf("streaming ended, no parts, no queue — not busy: %+v", b)
	}
}

// TestBusyStreamingCountsAlone: the pre-#1574 signal still counts.
func TestBusyStreamingCountsAlone(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))

	b := busyOf(t, a, "s1")
	if !b.GetBusy() || !b.GetStreaming() {
		t.Fatalf("a status busy-mark (streaming) must derive busy: %+v", b)
	}
}

// TestBusyPermissionWaitIsNotBusy: the owner's carve-out, pinned — a
// pending QUESTION/PERMISSION ask with nothing else in flight is NOT
// busy (blocked on the user), and pending_inputs still carries it (the
// pills' surface — the carve-out must not regress them).
func TestBusyPermissionWaitIsNotBusy(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	feed(t, a, p, inputEvent("q1", "s1", abiv1.InputKind_INPUT_KIND_QUESTION))
	feed(t, a, p, inputEvent("pm1", "s1", abiv1.InputKind_INPUT_KIND_PERMISSION))

	st := a.State().Sessions["s1"]
	if st.BusyComponents.GetBusy() {
		t.Fatalf("permission/question wait is NOT busy (the carve-out): %+v", st.Busy)
	}
	if st.BusyComponents.GetPendingUserInputs() != 2 {
		t.Fatalf("pending_user_inputs=2 expected, got %+v", st.Busy)
	}
	if len(st.PendingInputs) != 2 {
		t.Fatalf("the carve-out must not regress the pending-inputs surface (the pills): %v", st.PendingInputs)
	}
	if st.Status == abiv1.SessionStatus_SESSION_STATUS_BUSY {
		t.Fatal("status must not flip BUSY on a user-input wait")
	}
}

// TestBusyPermissionWaitDoesNotMaskWork: the carve-out is one-directional
// — a pending ask does not subtract from other busy components (a tool
// may run while an unrelated ask waits).
func TestBusyPermissionWaitDoesNotMaskWork(t *testing.T) {
	a, p := newEventAuthority(t, nil)
	feed(t, a, p, statusEvent("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	feed(t, a, p, inputEvent("q1", "s1", abiv1.InputKind_INPUT_KIND_QUESTION))
	feed(t, a, p, partEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "m1", "p1", ""))

	b := busyOf(t, a, "s1")
	if !b.GetBusy() {
		t.Fatalf("a running part keeps busy even with a pending ask: %+v", b)
	}
	if b.GetPendingUserInputs() != 1 || b.GetInFlightParts() != 1 {
		t.Fatalf("components must report both signals: %+v", b)
	}
}

// TestBusyQueueDepthCounts: a REAL queued delivery through the public
// wire op (r1 ask: the named test must pin the queue leg, not a
// vacuous zero-check). An admitted-then-unpromoted row is turn
// machinery in flight — the session derives BUSY with the queue
// component surfaced even though no status event ever marked it (the
// harness's status channel is silent about queued work — that silence
// was the false-IDLE).
func TestBusyQueueDepthCounts(t *testing.T) {
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       &mapStore{m: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}},
		Passwords:   []string{"pw"},
		Admitter:    &recordingAdmitter{},
		FastCursor:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	// Seed the projection's record for s1 from store truth.
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonBoot); err != nil {
		t.Fatal(err)
	}

	_, h := a.Handler()
	c := newAuthedServer(t, h)
	if _, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-busy", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "queued work"}}},
	})); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, ok := a.State().Sessions["s1"]
		if ok && st.BusyComponents.GetBusy() && st.BusyComponents.GetQueueDepth() == 1 {
			if st.Status != abiv1.SessionStatus_SESSION_STATUS_BUSY {
				t.Fatalf("derived busy must render BUSY status, got %v", st.Status)
			}
			if st.BusyComponents.GetStreaming() {
				t.Fatalf("no stream ever started — the queue leg alone derives busy: %+v", st.BusyComponents)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an admitted-then-unpromoted delivery must derive busy via the queue leg within 5s")
}

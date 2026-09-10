// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abitest_test

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// epic-71 / 1b: the fault-matrix event-suppression knob (legs 3/5).
// #1312's rule — the knobs are the failure-mode documentation in
// executable form, composable and inspectable — pinned here over the real
// connect wire.

func eventsClient(t *testing.T) (abiconnect.HarnessABIServiceClient, *abitest.Server) {
	t.Helper()
	srv := abitest.New()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return abiconnect.NewHarnessABIServiceClient(ts.Client(), base.String()), srv
}

// TestSuppressEventTypes_OmitsMatchingFrames: armed suppression removes
// the status event from the Events stream while the snapshot and reseed
// notice still arrive — leg 5's "mid-turn death emits nothing" shape.
func TestSuppressEventTypes_OmitsMatchingFrames(t *testing.T) {
	client, srv := eventsClient(t)
	srv.SuppressEventTypes(
		abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		abiv1.EventType_EVENT_TYPE_MESSAGE_START,
	)

	stream, err := client.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var sawSnapshot, sawStatusEvent, sawReseeded bool
	for stream.Receive() {
		frame := stream.Msg()
		switch f := frame.GetFrame().(type) {
		case *abiv1.StreamFrame_Snapshot:
			sawSnapshot = true
		case *abiv1.StreamFrame_Event:
			if f.Event.GetEvent().GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS {
				sawStatusEvent = true
			}
		case *abiv1.StreamFrame_Reseeded:
			sawReseeded = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawSnapshot || !sawReseeded {
		t.Fatalf("suppression must omit only event frames: snapshot=%v reseeded=%v", sawSnapshot, sawReseeded)
	}
	if sawStatusEvent {
		t.Fatal("suppressed SESSION_STATUS event frame reached the stream")
	}
}

// TestSuppressEventTypes_KnobStateInspectable: the armed set is readable
// for assertions (knob state is part of the contract).
func TestSuppressEventTypes_KnobStateInspectable(t *testing.T) {
	srv := abitest.New()
	if got := srv.SuppressedEventTypes(); len(got) != 0 {
		t.Fatalf("default armed set must be empty, got %v", got)
	}
	srv.SuppressEventTypes(abiv1.EventType_EVENT_TYPE_SESSION_STATUS, abiv1.EventType_EVENT_TYPE_MESSAGE_END)
	got := srv.SuppressedEventTypes()
	if len(got) != 2 {
		t.Fatalf("armed set = %v, want 2 entries", got)
	}
}

// TestSuppressEventTypes_OffByDefault: with no knob armed the reference
// stream is unchanged — knobs default off so existing contract rows stay
// green (the #1312 regression rule).
func TestSuppressEventTypes_OffByDefault(t *testing.T) {
	client, srv := eventsClient(t)
	if len(srv.SuppressedEventTypes()) != 0 {
		t.Fatal("knob must default off")
	}
	stream, err := client.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	sawStatusEvent := false
	for stream.Receive() {
		if f, ok := stream.Msg().GetFrame().(*abiv1.StreamFrame_Event); ok {
			if f.Event.GetEvent().GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS {
				sawStatusEvent = true
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawStatusEvent {
		t.Fatal("unarmed stream must still carry the status event")
	}
}

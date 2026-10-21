// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// #1817/#1821 piece 1 (ruling A — emission translation): the
// contract-stream frames the frontend fold keys on must carry the
// PHRASE (the id space the sessions list and ChatPage join on), with
// the harness id preserved ADDITIVELY as internal_session_id. The
// internal state machine stays keyed by the harness id — only the
// EMISSION translates. These pins are RED on main: frames carry the
// harness id and no internal field.
func TestEmissionFramesCarryPhraseID(t *testing.T) {
	const harnessID = "ses_harness_a"
	const phraseID = "brave-ocean-3"

	cfg := testConfig(t, t.TempDir(), &fixtureParser{}, &mapStore{})
	cfg.IDTranslator = func(id string) string {
		if id == harnessID {
			return phraseID
		}
		return ""
	}
	a := newAuthority(t, cfg)

	frames, cancel, err := a.Stream(t.Context())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	a.Ingest([]byte("busy " + harnessID))

	// Consume a bounded window of frames: the subscribe snapshot + the
	// live event. Post-fix the event carries the PHRASE (matching on
	// the harness id would never terminate — that IS the fix).
	gotEvent, gotSnapshotRow := false, false
	for i := 0; i < 8 && (!gotEvent || !gotSnapshotRow); i++ {
		select {
		case frame := <-frames:
			switch f := frame.Frame.(type) {
			case *abiv1.StreamFrame_Snapshot:
				gotSnapshotRow = true
				if f.Snapshot == nil || f.Snapshot.Snapshot == nil {
					t.Fatalf("snapshot frame without payload")
				}
				// The subscribe snapshot precedes the ingest; with no
				// session yet it carries zero rows — the LIVE snapshot
				// arrives on a second subscribe. Assert on a fresh
				// stream AFTER the session exists.
				frames2, cancel2, err := a.Stream(t.Context())
				if err != nil {
					t.Fatalf("second Stream: %v", err)
				}
				defer cancel2()
				snapFrame := <-frames2
				sf, ok := snapFrame.Frame.(*abiv1.StreamFrame_Snapshot)
				if !ok {
					t.Fatalf("second stream's first frame is not a snapshot")
				}
				for _, s := range sf.Snapshot.Snapshot.Sessions {
					if s.InternalSessionId == harnessID {
						if s.SessionId != phraseID {
							t.Errorf("snapshot row: wire id %q, want the phrase %q (internal %q)", s.SessionId, phraseID, s.InternalSessionId)
						}
					} else if s.SessionId == harnessID {
						t.Errorf("snapshot row keyed by the HARNESS id with no internal field — the #1817 breakage")
					}
				}
			case *abiv1.StreamFrame_Event:
				evt := f.Event.GetEvent()
				if evt == nil {
					continue
				}
				if evt.SessionId == harnessID {
					gotEvent = true
					t.Errorf("live event keyed by the HARNESS id %q — expected the phrase %q + internal_session_id", evt.SessionId, phraseID)
				}
				if evt.InternalSessionId == harnessID {
					gotEvent = true
					if evt.SessionId != phraseID {
						t.Errorf("event: wire id %q, want the phrase %q", evt.SessionId, phraseID)
					}
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no frame arrived within the window (i=%d)", i)
		}
	}
	if !gotEvent {
		t.Fatalf("never observed the session event frame")
	}
}

// The additive internal field + the heal-window passthrough pin: with
// NO translator (un-refreshed emitter), frames keep the harness id —
// never worse; consumers must not require the phrase.
func TestEmissionFrames_HealWindow_PassthroughHarnessID(t *testing.T) {
	a := newAuthority(t, testConfig(t, t.TempDir(), &fixtureParser{}, &mapStore{}))
	frames, cancel, err := a.Stream(t.Context())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer cancel()

	a.Ingest([]byte("busy ses_h1"))

	for frame := range frames {
		if f, ok := frame.Frame.(*abiv1.StreamFrame_Event); ok && f.Event != nil {
			evt := f.Event.GetEvent()
			if evt.SessionId == "ses_h1" {
				if evt.InternalSessionId != "" {
					t.Errorf("heal-window emission must not fabricate internal_session_id")
				}
				return // passthrough pinned
			}
		}
	}
	t.Fatalf("never observed the session event frame")
}

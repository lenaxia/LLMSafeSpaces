// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// events_test.go — the fixture-coverage drift gate (the opencode wire
// package's knownEventTypes posture, §7): every event type the recorded
// fixtures carry must be classified in knownEventTypes, so a fixture
// refresh that introduces a new type fails here and the taxonomy grows
// in the same change. Plus the defensive-decode pins: unknown fields
// (the recorded capture carries reasoning/totalFromProvider drift) and
// unknown types never break a stream.

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestEventFixtureCoverage_KnownEventTypes(t *testing.T) {
	f, err := os.Open("testdata/events_stream.jsonl")
	if err != nil {
		t.Fatalf("open events fixture: %v", err)
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ev, ok := parseEvent([]byte(line))
		if !ok {
			t.Fatalf("fixture line failed to parse as an event: %s", line)
		}
		seen[ev.Type] = true
	}
	if len(seen) == 0 {
		t.Fatal("no fixture events")
	}
	for typ := range seen {
		if !knownEventTypes[typ] {
			t.Errorf("fixture event type %q is not in knownEventTypes — extend the taxonomy in the same change (drift gate)", typ)
		}
	}
}

func TestParseEvent_ToleratesUnknownFieldsAndTypes(t *testing.T) {
	// The RECORDED get_entries capture carries fields beyond the docs
	// (usage.reasoning, cost.totalFromProvider) — decode must ignore.
	rec := `{"type":"message_update","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"reasoning":5,"totalTokens":12,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0,"totalFromProvider":false}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"},"brandNewField":{"nested":true}}`
	ev, ok := parseEvent([]byte(rec))
	if !ok || ev.Type != evMessageUpdate {
		t.Fatalf("recorded-drift record must parse: %v %v", ev, ok)
	}
	if ev.Usage == nil || ev.Usage.promptTokens() != 8 || ev.Usage.Reasoning != 5 {
		t.Fatalf("usage: %+v", ev.Usage)
	}
	if ev.AssistantMessageEvent == nil || ev.AssistantMessageEvent.Delta != "x" {
		t.Fatalf("ame: %+v", ev.AssistantMessageEvent)
	}

	// Unknown event type: tolerated envelope with zeroed payloads.
	ev, ok = parseEvent([]byte(`{"type":"from_the_future","x":1}`))
	if !ok || ev.Type != "from_the_future" {
		t.Fatalf("unknown type must be tolerated: %v %v", ev, ok)
	}

	// Response family and garbage never decode as events.
	if _, ok := parseEvent([]byte(`{"type":"response","success":true}`)); ok {
		t.Fatal("response family must not decode as an event")
	}
	if _, ok := parseEvent([]byte(`not json`)); ok {
		t.Fatal("garbage must not decode")
	}
	if _, ok := parseEvent([]byte(`{}`)); ok {
		t.Fatal("type-less record must not decode")
	}
}

func TestParseEvent_PartialDecodeOnTypeConflict(t *testing.T) {
	// A drifted field whose type conflicts with ours (usage as string):
	// the envelope's type still classifies, payloads zero out — one
	// record's drift never kills the stream.
	ev, ok := parseEvent([]byte(`{"type":"agent_settled","aborted":"yes"}`))
	if !ok || ev.Type != evAgentSettled {
		t.Fatalf("conflicting payload must not lose the type: %v %v", ev, ok)
	}
	if ev.Aborted {
		t.Fatal("conflicting field decodes to zero value")
	}
}

func TestInjectSessionID_Additive(t *testing.T) {
	raw := []byte(`{"type":"agent_start"}`)
	out := injectSessionID(raw, "sess-9")
	if got := recordSessionID(out); got != "sess-9" {
		t.Fatalf("injected id not read back: %s", out)
	}
	// Existing sessionId wins (no overwrite).
	raw = []byte(`{"type":"agent_start","sessionId":"mine"}`)
	if got := recordSessionID(injectSessionID(raw, "other")); got != "mine" {
		t.Fatalf("overwrite: %q", got)
	}
	// Garbage passes through untouched.
	if string(injectSessionID([]byte(`{nope`), "x")) != `{nope` {
		t.Fatal("garbage mutated")
	}
}

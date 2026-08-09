// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMessageTypeValues(t *testing.T) {
	all := []MessageType{
		MessageUser, MessageAssistant, MessageShell,
		MessageAgentSwitch, MessageModelSwitch, MessageCompaction, MessageSystem,
	}
	seen := make(map[MessageType]bool, len(all))
	for _, m := range all {
		if string(m) == "" {
			t.Fatalf("MessageType must be non-empty")
		}
		if seen[m] {
			t.Fatalf("duplicate MessageType %q", m)
		}
		seen[m] = true
	}
}

func TestMessageRoundTripAllTypes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	exit := 0
	cases := []struct {
		name string
		msg  Message
	}{
		{"user", UserMessage("m1", "write a test", now)},
		{"assistant", AssistantMessage("m2", []Part{
			{Type: PartText, Text: "done"},
			{Type: PartTool, Tool: &ToolPart{Name: "edit", State: ToolState{Status: ToolStatusCompleted}}},
		}, now)},
		{"shell", ShellMessage("m3", "go build ./...", &exit, now)},
		{"agent_switch", AgentSwitchMessage("m4", "opencode", "pi", now)},
		{"model_switch", ModelSwitchMessage("m5", &ModelRef{ID: "old"}, &ModelRef{ID: "gpt-5", Provider: "openai"}, now)},
		{"compaction", CompactionMessage("m6", "context compacted", now)},
		{"system", SystemMessage("m7", "session resumed", now)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.msg.Type == "" {
				t.Fatalf("constructor left Type empty")
			}
			roundTrip(t, tc.msg)
		})
	}
}

func TestMessageConstructorSetsDiscriminator(t *testing.T) {
	now := time.Now().UTC()
	disc := map[MessageType]Message{
		MessageUser:        UserMessage("id", "x", now),
		MessageAssistant:   AssistantMessage("id", nil, now),
		MessageShell:       ShellMessage("id", "ls", nil, now),
		MessageAgentSwitch: AgentSwitchMessage("id", "a", "b", now),
		MessageModelSwitch: ModelSwitchMessage("id", nil, nil, now),
		MessageCompaction:  CompactionMessage("id", "x", now),
		MessageSystem:      SystemMessage("id", "x", now),
	}
	for wantType, msg := range disc {
		if msg.Type != wantType {
			t.Fatalf("constructor for %q set Type=%q", wantType, msg.Type)
		}
	}
}

func TestMessageOmitsEmptyOptionalFields(t *testing.T) {
	m := UserMessage("m1", "hi", time.Now().UTC())
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{
		"parts", "model", "cost", "command", "exitCode",
		"fromAgent", "toAgent", "fromModel", "toModel", "error", "sessionId",
	} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("optional field %q should omit when unset; got %s", absent, out)
		}
	}
}

func TestAssistantMessageCarriesCostAndModel(t *testing.T) {
	now := time.Now().UTC()
	msg := AssistantMessage("m1", []Part{{Type: PartText, Text: "ok"}}, now)
	msg.Model = &ModelRef{ID: "gpt-5", Provider: "openai"}
	msg.Cost = &Cost{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CostUSD: 0.0012}
	roundTrip(t, msg)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStatusValues(t *testing.T) {
	for _, s := range []Status{StatusIdle, StatusBusy, StatusError, StatusCompacting, StatusArchived, StatusUnknown} {
		if string(s) == "" {
			t.Fatalf("Status must be non-empty")
		}
	}
}

func TestSessionRoundTrip(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(2 * time.Minute)
	s := Session{
		ID:          "s1",
		WorkspaceID: "ws1",
		ParentID:    "root-session",
		Title:       "fix the bug",
		AgentID:     "primary",
		Model:       &ModelRef{ID: "gpt-5", Provider: "openai"},
		Status:      StatusBusy,
		Cost:        &Cost{TotalTokens: 100, CostUSD: 0.01},
		Time:        &TimeRange{StartedAt: start, CompletedAt: &end},
		Summary:     "refactored x",
		Archived:    false,
	}
	roundTrip(t, s)
}

func TestSessionOmitsEmptyOptionalFields(t *testing.T) {
	s := Session{ID: "s1", WorkspaceID: "ws1", Status: StatusIdle}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{
		"parentId", "title", "agentId", "model", "cost", "time", "summary", "archived",
	} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("optional field %q should omit when unset; got %s", absent, out)
		}
	}
}

func TestCostAndModelInfoRoundTrip(t *testing.T) {
	roundTrip(t, Cost{InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3, CacheReadTokens: 4, CacheWriteTokens: 5, TotalTokens: 15, CostUSD: 0.5})
	roundTrip(t, ModelRef{ID: "claude-opus", Provider: "anthropic"})
	roundTrip(t, ModelInfo{ID: "claude-opus", Provider: "anthropic", DisplayName: "Claude Opus", ContextWindow: 200000, MaxOutput: 8192})
	roundTrip(t, TimeRange{StartedAt: time.Now().UTC()})
}

func TestCapabilityConstantsExist(t *testing.T) {
	// Design 0049 §4.4 + §4.6: steer/queue admission + rewind/fork/stash pass-through.
	for _, c := range []Capability{CapSteer, CapQueue, CapRewind, CapFork, CapStash, CapDiff, CapReasoning} {
		if string(c) == "" {
			t.Fatalf("Capability must be non-empty")
		}
	}
}

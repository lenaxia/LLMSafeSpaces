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
		"parentId", "title", "agentId", "model", "cost", "time", "summary", "archived", "contextUsage",
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
	roundTrip(t, ContextUsage{Used: 14865, Window: 200000})
}

func TestContextUsageIsSessionLiveOccupancy(t *testing.T) {
	// ContextUsage.Used is the semantic "tokens of context window currently
	// occupied" — the numerator for the "context: 45% used" display that
	// ModelInfo.ContextWindow denominates. Adapters compute it once from
	// agent-specific accounting (e.g. input + cache.read + cache.write);
	// raw token ledgers stay in Cost. Design 0049 amendment (epic 65 S3).
	s := Session{ID: "s1", WorkspaceID: "ws1", Status: StatusBusy,
		ContextUsage: &ContextUsage{Used: 14865, Window: 200000}}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Session
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ContextUsage == nil || back.ContextUsage.Used != 14865 || back.ContextUsage.Window != 200000 {
		t.Fatalf("ContextUsage must round-trip; got %+v", back.ContextUsage)
	}
	// Window is optional: adapters without a known window report Used only.
	bare := ContextUsage{Used: 42}
	if _, err := json.Marshal(bare); err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
}

func TestCapabilityConstantsExist(t *testing.T) {
	// Design 0049 §4.4 + §4.6: steer/queue admission + rewind/fork/stash pass-through.
	for _, c := range []Capability{CapSteer, CapQueue, CapRewind, CapFork, CapStash, CapDiff, CapReasoning} {
		if string(c) == "" {
			t.Fatalf("Capability must be non-empty")
		}
	}
}

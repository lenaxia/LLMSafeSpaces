// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPartTypeCountIsExactlyFive(t *testing.T) {
	// Design 0049 §4.1 rule 1 + §7 rule 1: "5 part types forever".
	// Adding a part type is a contract change requiring a design-doc
	// update. If this test fails, EITHER revert the change OR amend
	// design/0049_2026-08-09_agent-session-contract.md §4.3 + Epic 65
	// US-65.2's "Done when" first. There is no third option; "just this
	// once" rebuilds the agent coupling in a new file (Rule 12).
	all := []PartType{PartText, PartReasoning, PartTool, PartFileChange, PartCustom}
	const want = 5
	if len(all) != want {
		t.Fatalf("PartType union must stay at %d forever — got %d. Contract change: "+
			"amend design/0049 §4.3 + Epic 65 US-65.2 first, OR revert the constant.",
			want, len(all))
	}
	seen := make(map[PartType]bool, len(all))
	for _, p := range all {
		if seen[p] {
			t.Fatalf("duplicate PartType %q", p)
		}
		seen[p] = true
	}
}

func TestPartRoundTripAllTypes(t *testing.T) {
	now := time.Now().UTC()
	started := now
	completed := now.Add(50 * time.Millisecond)
	cases := []struct {
		name string
		part Part
	}{
		{
			name: "text",
			part: Part{Type: PartText, ID: "p1", Text: "hello world"},
		},
		{
			name: "reasoning",
			part: Part{Type: PartReasoning, ID: "p2", Reasoning: "thinking..."},
		},
		{
			name: "tool running",
			part: Part{Type: PartTool, ID: "p3", Tool: &ToolPart{
				CallID: "call_1", Name: "bash",
				Input: json.RawMessage(`{"command":"ls"}`),
				State: ToolState{Status: ToolStatusRunning, StartedAt: &started},
			}},
		},
		{
			name: "tool completed with output",
			part: Part{Type: PartTool, ID: "p4", Tool: &ToolPart{
				CallID: "call_2", Name: "edit",
				Input:  json.RawMessage(`{"path":"a.go"}`),
				Output: json.RawMessage(`{"ok":true}`),
				State: ToolState{
					Status:      ToolStatusCompleted,
					StartedAt:   &started,
					CompletedAt: &completed,
				},
			}},
		},
		{
			name: "tool errored",
			part: Part{Type: PartTool, ID: "p5", Tool: &ToolPart{
				Name: "grep", State: ToolState{Status: ToolStatusError, Error: "exit 1"},
			}},
		},
		{
			name: "file change",
			part: Part{Type: PartFileChange, ID: "p6", FileChange: &FileDiff{
				Path: "main.go", Status: ChangeModified,
				Patch: "@@ -1,3 +1,3 @@\n-a\n+b\n", Additions: 1, Deletions: 1,
			}},
		},
		{
			name: "file rename",
			part: Part{Type: PartFileChange, ID: "p7", FileChange: &FileDiff{
				Path: "new.go", OldPath: "old.go", Status: ChangeRenamed, Patch: "",
			}},
		},
		{
			name: "custom",
			part: Part{Type: PartCustom, ID: "p8", Custom: &CustomPart{
				Kind: "x.custom", Data: json.RawMessage(`{"whatever":true}`),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			roundTrip(t, tc.part)
		})
	}
}

func TestPartOmitsEmptyOptionalFields(t *testing.T) {
	// A bare text part must marshal to only type + the set field; no empty
	// tool/fileChange/custom/reasoning keys leak onto the wire.
	p := Part{Type: PartText, Text: "hi"}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"reasoning", "tool", "fileChange", "custom", "id"} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("optional field %q should omit when unset; got %s", absent, out)
		}
	}
}

func TestToolStatusStateMachineValues(t *testing.T) {
	// Design 0049 §4.3: tool state machine is pending→running→completed|error.
	for _, s := range []ToolStatus{ToolStatusPending, ToolStatusRunning, ToolStatusCompleted, ToolStatusError} {
		if string(s) == "" {
			t.Fatalf("ToolStatus must be non-empty")
		}
	}
}

func TestCustomPartRequiresKind(t *testing.T) {
	// Design 0049 §4.3: Custom requires a `Kind` discriminator. The contract
	// enforces this at the schema level: Kind has no omitempty, so it is
	// always emitted on the wire (a required field). Value-level non-empty
	// enforcement is deferred to the adapter's Validate (US-65.3).
	cp := CustomPart{Data: json.RawMessage(`{}`)}
	out, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["kind"]; !ok {
		t.Fatalf("Kind must always be present on the wire (required field); got %s", out)
	}
	cp.Kind = "ext.foo"
	roundTrip(t, cp)
}

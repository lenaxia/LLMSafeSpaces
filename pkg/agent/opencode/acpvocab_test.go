// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Design 0063 §4 mapping-table rows, one test per row, plus the spike's
// pins (replay-only user_message_chunk; todowrite-as-other; plan mapped
// but never emitted; unknown kinds tolerated, never dropped).

func TestAcpUpdateToPartRows(t *testing.T) {
	cases := []struct {
		name string
		in   AcpUpdate
		want session.Part
	}{
		{
			name: "agent_message_chunk -> Text",
			in:   AcpUpdate{Kind: AcpUpdateAgentMessageChunk, MessageID: "m1", Content: AcpContentBlock{Type: "text", Text: "pong"}},
			want: session.Part{Type: session.PartText, Text: "pong"},
		},
		{
			name: "agent_thought_chunk -> Reasoning",
			in:   AcpUpdate{Kind: AcpUpdateAgentThoughtChunk, MessageID: "m1", Content: AcpContentBlock{Type: "text", Text: "thinking"}},
			want: session.Part{Type: session.PartReasoning, Reasoning: "thinking"},
		},
		{
			name: "user_message_chunk (replay-only) -> Text",
			in:   AcpUpdate{Kind: AcpUpdateUserMessageChunk, MessageID: "m0", Content: AcpContentBlock{Type: "text", Text: "the prompt"}},
			want: session.Part{Type: session.PartText, Text: "the prompt"},
		},
		{
			name: "tool_call pending -> Tool Pending",
			in:   AcpUpdate{Kind: AcpUpdateToolCall, ToolCall: &AcpToolCall{ToolCallID: "c1", Title: "bash", Kind: AcpToolKindExecute, Status: AcpToolStatusPending}},
			want: session.Part{Type: session.PartTool, Tool: &session.ToolPart{CallID: "c1", Name: "bash", State: session.ToolState{Status: session.ToolStatusPending}}},
		},
		{
			name: "tool_call_update in_progress -> Tool Running",
			in:   AcpUpdate{Kind: AcpUpdateToolCallUpdate, ToolCall: &AcpToolCall{ToolCallID: "c1", Status: AcpToolStatusInProgress, RawInput: json.RawMessage(`{"command":"echo hi"}`)}},
			want: session.Part{Type: session.PartTool, Tool: &session.ToolPart{CallID: "c1", Input: json.RawMessage(`{"command":"echo hi"}`), State: session.ToolState{Status: session.ToolStatusRunning}}},
		},
		{
			name: "tool_call_update completed -> Tool Completed with output",
			in:   AcpUpdate{Kind: AcpUpdateToolCallUpdate, ToolCall: &AcpToolCall{ToolCallID: "c1", Status: AcpToolStatusCompleted, RawOutput: json.RawMessage(`{"output":"hi\n"}`)}},
			want: session.Part{Type: session.PartTool, Tool: &session.ToolPart{CallID: "c1", Output: json.RawMessage(`{"output":"hi\n"}`), State: session.ToolState{Status: session.ToolStatusCompleted}}},
		},
		{
			name: "tool_call_update failed -> Tool Error with message",
			in:   AcpUpdate{Kind: AcpUpdateToolCallUpdate, ToolCall: &AcpToolCall{ToolCallID: "c1", Status: AcpToolStatusFailed, Error: "boom"}},
			want: session.Part{Type: session.PartTool, Tool: &session.ToolPart{CallID: "c1", State: session.ToolState{Status: session.ToolStatusError, Error: "boom"}}},
		},
		{
			name: "available_commands_update -> Custom(acp.available_commands)",
			in:   AcpUpdate{Kind: AcpUpdateAvailableCommands, Commands: []AcpCommand{{Name: "init", Description: "setup"}}},
			want: session.Part{Type: session.PartCustom, Custom: &session.CustomPart{Kind: "acp.available_commands", Data: json.RawMessage(`[{"name":"init","description":"setup"}]`)}},
		},
		{
			name: "plan (zod-only, never emitted) -> Tool row, not a new part type",
			in:   AcpUpdate{Kind: AcpUpdatePlan, Plan: []AcpPlanEntry{{Content: "spike", Status: "pending"}}},
			want: session.Part{Type: session.PartTool, Tool: &session.ToolPart{Name: "plan", Input: json.RawMessage(`[{"content":"spike","status":"pending"}]`), State: session.ToolState{Status: session.ToolStatusCompleted}}},
		},
		{
			name: "unknown kind -> Custom(acp.unknown), never dropped",
			in:   AcpUpdate{Kind: AcpUpdateKind("session_info_update")},
			want: session.Part{Type: session.PartCustom, Custom: &session.CustomPart{Kind: "acp.unknown", Data: json.RawMessage(`"session_info_update"`)}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.in.ToPart()
			if !ok {
				t.Fatalf("ToPart returned ok=false, want a part for %q", tc.in.Kind)
			}
			if got.Type != tc.want.Type {
				t.Fatalf("type = %q, want %q", got.Type, tc.want.Type)
			}
			switch {
			case tc.want.Text != "":
				if got.Text != tc.want.Text {
					t.Fatalf("text = %q, want %q", got.Text, tc.want.Text)
				}
			case tc.want.Reasoning != "":
				if got.Reasoning != tc.want.Reasoning {
					t.Fatalf("reasoning = %q, want %q", got.Reasoning, tc.want.Reasoning)
				}
			case tc.want.Tool != nil:
				if got.Tool == nil {
					t.Fatal("tool part missing")
				}
				if got.Tool.CallID != tc.want.Tool.CallID || got.Tool.Name != tc.want.Tool.Name {
					t.Fatalf("callID/name = %q/%q, want %q/%q", got.Tool.CallID, got.Tool.Name, tc.want.Tool.CallID, tc.want.Tool.Name)
				}
				if got.Tool.State.Status != tc.want.Tool.State.Status {
					t.Fatalf("status = %q, want %q", got.Tool.State.Status, tc.want.Tool.State.Status)
				}
				if got.Tool.State.Error != tc.want.Tool.State.Error {
					t.Fatalf("error = %q, want %q", got.Tool.State.Error, tc.want.Tool.State.Error)
				}
				if string(got.Tool.Input) != string(tc.want.Tool.Input) {
					t.Fatalf("input = %s, want %s", got.Tool.Input, tc.want.Tool.Input)
				}
				if string(got.Tool.Output) != string(tc.want.Tool.Output) {
					t.Fatalf("output = %s, want %s", got.Tool.Output, tc.want.Tool.Output)
				}
			case tc.want.Custom != nil:
				if got.Custom == nil {
					t.Fatal("custom part missing")
				}
				if got.Custom.Kind != tc.want.Custom.Kind {
					t.Fatalf("kind = %q, want %q", got.Custom.Kind, tc.want.Custom.Kind)
				}
			}
		})
	}
}

func TestAcpUsageIsNotAPart(t *testing.T) {
	u := AcpUpdate{Kind: AcpUpdateUsage, Usage: &AcpUsage{Used: 50848, Size: 1000000}}
	if _, ok := u.ToPart(); ok {
		t.Fatal("usage_update must not map to a Part (session-level gauge, design 0063 §3.2)")
	}
}

func TestAcpDiffContentToFileChange(t *testing.T) {
	u := AcpUpdate{
		Kind: AcpUpdateToolCallUpdate,
		ToolCall: &AcpToolCall{
			ToolCallID: "c1", Status: AcpToolStatusCompleted, Title: "edit",
			Content: []AcpToolContent{{Kind: AcpToolContentDiff, Path: "note.txt", OldText: "line three", NewText: "line FOUR"}},
		},
	}
	got, ok := u.ToPart()
	if !ok || got.Type != session.PartTool || got.Tool == nil {
		t.Fatalf("diff-carrying tool_call_update must map to a Tool part, got %+v ok=%v", got, ok)
	}
	// The diff rides the tool part (the contract's FileChange part is
	// produced by the filediff producer from changed paths — the
	// vocabulary exposes it for consumers that want it directly).
	fd := u.ToolCall.FileChange()
	if fd == nil || fd.Path != "note.txt" {
		t.Fatalf("FileChange() = %+v, want path note.txt", fd)
	}
	if !strings.Contains(fd.Patch, "-line three") || !strings.Contains(fd.Patch, "+line FOUR") {
		t.Fatalf("patch = %q, want unified old->new", fd.Patch)
	}
}

func TestAcpPartRoundTrip(t *testing.T) {
	// Text <-> agent_message_chunk
	p := session.Part{Type: session.PartText, Text: "hello"}
	u, ok := AcpUpdateFromPart(p)
	if !ok || u.Kind != AcpUpdateAgentMessageChunk || u.Content.Text != "hello" {
		t.Fatalf("text round-trip: %+v ok=%v", u, ok)
	}
	back, _ := u.ToPart()
	if back.Text != "hello" || back.Type != session.PartText {
		t.Fatalf("text round-trip back: %+v", back)
	}
	// Reasoning <-> agent_thought_chunk
	p = session.Part{Type: session.PartReasoning, Reasoning: "hm"}
	u, ok = AcpUpdateFromPart(p)
	if !ok || u.Kind != AcpUpdateAgentThoughtChunk || u.Content.Text != "hm" {
		t.Fatalf("reasoning round-trip: %+v ok=%v", u, ok)
	}
	// Tool -> tool_call with kind inferred from Name
	p = session.Part{Type: session.PartTool, Tool: &session.ToolPart{CallID: "c9", Name: "bash", State: session.ToolState{Status: session.ToolStatusRunning}}}
	u, ok = AcpUpdateFromPart(p)
	if !ok || u.Kind != AcpUpdateToolCall || u.ToolCall == nil || u.ToolCall.Kind != AcpToolKindExecute || u.ToolCall.Status != AcpToolStatusInProgress {
		t.Fatalf("tool round-trip: %+v ok=%v", u, ok)
	}
}

// THE PIN (design 0063 §3.2/§5.5): todowrite maps to kind "other" —
// opencode's todo tool is a generic tool_call, never the plan variant.
func TestAcpTodowriteIsKindOther(t *testing.T) {
	if k := AcpToolKindFromName("todowrite"); k != AcpToolKindOther {
		t.Fatalf("todowrite kind = %q, want %q (the todowrite-as-generic-tool_call pin)", k, AcpToolKindOther)
	}
	// And the plan update kind exists but the native direction cannot
	// synthesize it (schema-present, never emitted):
	if !AcpUpdatePlan.IsNeverEmittedByNative() {
		t.Fatal("plan must be marked never-emitted-by-native")
	}
}

// THE PIN (design 0063 §3.2): user_message_chunk is replay-only — the
// native->ACP direction has NO producer for it.
func TestAcpUserMessageChunkHasNoNativeProducer(t *testing.T) {
	for _, kind := range NativeChunkKinds() {
		if kind == AcpUpdateUserMessageChunk {
			t.Fatal("native chunk direction must not produce user_message_chunk (replay-only: fork/load history ingestion)")
		}
	}
	// The replay direction exists and maps to user Text:
	u, ok := AcpUpdateFromPart(session.Part{Type: session.PartText, Text: "echoed"})
	if ok && u.Kind == AcpUpdateUserMessageChunk {
		t.Fatal("AcpUpdateFromPart must not emit user_message_chunk either — it is ingest-only")
	}
}

func TestAcpToolKindTable(t *testing.T) {
	cases := map[string]AcpToolKind{
		"bash":      AcpToolKindExecute,
		"shell":     AcpToolKindExecute,
		"write":     AcpToolKindEdit,
		"edit":      AcpToolKindEdit,
		"read":      AcpToolKindRead,
		"glob":      AcpToolKindRead,
		"grep":      AcpToolKindSearch,
		"webfetch":  AcpToolKindFetch,
		"todowrite": AcpToolKindOther,
		"task":      AcpToolKindOther,
		"mystery":   AcpToolKindOther,
	}
	for name, want := range cases {
		if got := AcpToolKindFromName(name); got != want {
			t.Errorf("AcpToolKindFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAcpStatusNormalization(t *testing.T) {
	// Native opencode statuses normalize to ACP names...
	cases := map[string]AcpToolStatus{
		"pending":   AcpToolStatusPending,
		"running":   AcpToolStatusInProgress,
		"completed": AcpToolStatusCompleted,
		"error":     AcpToolStatusFailed,
	}
	for native, want := range cases {
		if got := AcpToolStatusFromNative(native); got != want {
			t.Errorf("AcpToolStatusFromNative(%q) = %q, want %q", native, got, want)
		}
	}
	// ...and ACP names map to contract statuses 1:1.
	contract := map[AcpToolStatus]session.ToolStatus{
		AcpToolStatusPending:    session.ToolStatusPending,
		AcpToolStatusInProgress: session.ToolStatusRunning,
		AcpToolStatusCompleted:  session.ToolStatusCompleted,
		AcpToolStatusFailed:     session.ToolStatusError,
	}
	for acp, want := range contract {
		if got := acp.ToContractStatus(); got != want {
			t.Errorf("%q.ToContractStatus() = %q, want %q", acp, got, want)
		}
	}
}

// The consumer path (Stage A's required real consumer): translateTool —
// used by BOTH the history translation and the US-65.8 SSE bridge via
// translatePart — must flow native -> AcpToolCall -> ToolPart with
// byte-identical output and the state machine owned by the mapper.
func TestTranslateToolThroughVocabulary(t *testing.T) {
	started := time.Date(2026, 9, 28, 17, 0, 0, 0, time.UTC)
	completed := started.Add(3 * time.Second)
	in := &ocTool{
		CallID: "call_abc",
		Name:   "bash",
		Input:  json.RawMessage(`{"command":"echo hi"}`),
		Output: json.RawMessage(`{"output":"hi\n"}`),
		State:  &ocToolState{Status: "running", StartedAt: &started},
	}
	got := translateTool(in)
	if got == nil {
		t.Fatal("translateTool returned nil")
	}
	if got.CallID != "call_abc" || got.Name != "bash" {
		t.Fatalf("callID/name = %q/%q", got.CallID, got.Name)
	}
	if got.State.Status != session.ToolStatusRunning {
		t.Fatalf("status = %q, want running (native running -> ACP in_progress -> contract running)", got.State.Status)
	}
	if string(got.Input) != `{"command":"echo hi"}` || string(got.Output) != `{"output":"hi\n"}` {
		t.Fatalf("input/output drift: %s / %s", got.Input, got.Output)
	}
	if got.State.StartedAt == nil || !got.State.StartedAt.Equal(started) {
		t.Fatalf("startedAt not preserved: %v", got.State.StartedAt)
	}
	// The vocabulary carries the kind metadata internally (bash ->
	// execute) even though the contract ToolPart has no kind field.
	acp := AcpToolCallFromNative(in)
	if acp.Kind != AcpToolKindExecute || acp.Status != AcpToolStatusInProgress {
		t.Fatalf("vocabulary shape: kind=%q status=%q, want execute/in_progress", acp.Kind, acp.Status)
	}
	// Unknown native status keeps the historical safe default (pending).
	if got := translateToolStatus("garbage"); got != session.ToolStatusPending {
		t.Fatalf("unknown native status = %q, want pending (behavior-preserving default)", got)
	}
	// Times and error ride through the failed path.
	failed := &ocTool{CallID: "c2", Name: "edit", State: &ocToolState{Status: "error", Error: "boom", CompletedAt: &completed}}
	fp := translateTool(failed)
	if fp.State.Status != session.ToolStatusError || fp.State.Error != "boom" || fp.State.CompletedAt == nil {
		t.Fatalf("failed-path drift: %+v", fp.State)
	}
	// nil safety preserved.
	if translateTool(nil) != nil {
		t.Fatal("translateTool(nil) must return nil")
	}
}

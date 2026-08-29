// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// US-65.3 translate.go — pin every translation rule with table-driven
// tests so the adapter implementation cannot drift from the contract.
// These tests cover the pure translation layer; HTTP wiring lands in
// adapter.go + adapter_test.go.

func TestTranslateMessage_UserMessage(t *testing.T) {
	m := ocMessage{
		Info: ocInfo{Role: "user", ID: "msg_1", SessionID: "ses_1"},
		Parts: []ocPart{
			{Type: "text", ID: "p1", Text: "hello"},
		},
	}
	sm, files := translateMessage(m)

	assert.Equal(t, session.MessageUser, sm.Type)
	assert.Equal(t, "msg_1", sm.ID)
	assert.Equal(t, "ses_1", sm.SessionID)
	require.Len(t, sm.Parts, 1)
	assert.Equal(t, session.PartText, sm.Parts[0].Type)
	assert.Equal(t, "hello", sm.Parts[0].Text)
	assert.Empty(t, files, "user message with no patch → no changedFiles")
}

func TestTranslateMessage_AssistantMessage_TextReasoningTool(t *testing.T) {
	startedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(2 * time.Second)
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_2", SessionID: "ses_1"},
		Parts: []ocPart{
			{Type: "text", ID: "p1", Text: "thinking..."},
			{Type: "reasoning", ID: "p2", Reasoning: "step by step"},
			{Type: "tool", ID: "p3", Tool: &ocTool{
				CallID: "call_1",
				Name:   "bash",
				Input:  json.RawMessage(`{"cmd":"ls"}`),
				Output: json.RawMessage(`{"stdout":"file.go"}`),
				State: &ocToolState{
					Status:      "completed",
					StartedAt:   &startedAt,
					CompletedAt: &completedAt,
				},
			}},
		},
	}
	sm, _ := translateMessage(m)

	require.Equal(t, session.MessageAssistant, sm.Type)
	require.Len(t, sm.Parts, 3)

	assert.Equal(t, session.PartText, sm.Parts[0].Type)
	assert.Equal(t, "thinking...", sm.Parts[0].Text)

	assert.Equal(t, session.PartReasoning, sm.Parts[1].Type)
	assert.Equal(t, "step by step", sm.Parts[1].Reasoning)

	require.Equal(t, session.PartTool, sm.Parts[2].Type)
	require.NotNil(t, sm.Parts[2].Tool)
	assert.Equal(t, "bash", sm.Parts[2].Tool.Name)
	assert.Equal(t, "call_1", sm.Parts[2].Tool.CallID)
	assert.Equal(t, session.ToolStatusCompleted, sm.Parts[2].Tool.State.Status)
	assert.Equal(t, &completedAt, sm.Parts[2].Tool.State.CompletedAt)
}

func TestTranslateMessage_StepStartFinish_Dropped(t *testing.T) {
	// step-start and step-finish are turn-boundary markers that carry
	// no renderable content. The contract has no equivalent — they
	// must be dropped, NOT added as new PartType constants.
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_1"},
		Parts: []ocPart{
			{Type: "step-start", ID: "p1"},
			{Type: "text", ID: "p2", Text: "actual content"},
			{Type: "step-finish", ID: "p3"},
		},
	}
	sm, _ := translateMessage(m)
	require.Len(t, sm.Parts, 1, "step-start and step-finish must be dropped")
	assert.Equal(t, "actual content", sm.Parts[0].Text)
}

func TestTranslateMessage_PatchPart_CollectsChangedFiles(t *testing.T) {
	// opencode's `patch` part carries only file paths — no diff hunks.
	// The adapter must collect the paths so the caller (Adapter) can
	// run filediff.Producer to produce FileChange parts.
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_1"},
		Parts: []ocPart{
			{Type: "text", ID: "p1", Text: "made changes"},
			{Type: "patch", ID: "p2", Files: []string{
				"/workspace/foo.go",
				"/workspace/bar.go",
				"/workspace/foo.go", // duplicate — must be de-duped
				"",                  // empty — must be dropped
			}},
		},
	}
	sm, files := translateMessage(m)

	require.Len(t, sm.Parts, 1, "patch part must not produce a Part in translateMessage")
	assert.Equal(t, "made changes", sm.Parts[0].Text)

	require.Len(t, files, 2, "duplicates and empty paths dropped")
	assert.Contains(t, files, "/workspace/foo.go")
	assert.Contains(t, files, "/workspace/bar.go")
}

func TestTranslateMessage_CustomPart_PreservesKind(t *testing.T) {
	// Custom is the pressure-relief valve for extension-defined
	// semantics. Kind is required; if absent, drop the part.
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_1"},
		Parts: []ocPart{
			{Type: "custom", ID: "p1", Custom: &session.CustomPart{
				Kind: "doomSummary",
				Data: json.RawMessage(`{"confidence":0.9}`),
			}},
			{Type: "custom", ID: "p2", Custom: &session.CustomPart{Kind: ""}}, // missing kind → dropped
		},
	}
	sm, _ := translateMessage(m)

	require.Len(t, sm.Parts, 1, "custom with empty Kind must be dropped")
	assert.Equal(t, session.PartCustom, sm.Parts[0].Type)
	require.NotNil(t, sm.Parts[0].Custom)
	assert.Equal(t, "doomSummary", sm.Parts[0].Custom.Kind)
	assert.JSONEq(t, `{"confidence":0.9}`, string(sm.Parts[0].Custom.Data))
}

func TestTranslateMessage_UnknownPartType_PreservedAsCustom(t *testing.T) {
	// Unknown part types must NOT be silently dropped. Preserving as
	// Custom with the opencode type string as Kind surfaces future
	// extensions in the UI without adapter changes. This is design
	// 0049 §4.3's pressure-relief-valve rule.
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_1"},
		Parts: []ocPart{
			{Type: "future_part_v2", ID: "p1", Text: "unknown content"},
		},
	}
	sm, _ := translateMessage(m)

	require.Len(t, sm.Parts, 1)
	assert.Equal(t, session.PartCustom, sm.Parts[0].Type)
	require.NotNil(t, sm.Parts[0].Custom)
	assert.Equal(t, "future_part_v2", sm.Parts[0].Custom.Kind,
		"unknown opencode part type becomes the Custom Kind discriminator")
}

func TestTranslateMessage_ShellMessage(t *testing.T) {
	exitCode := 0
	m := ocMessage{
		Info: ocInfo{Role: "shell", ID: "msg_sh", Command: "git status", ExitCode: &exitCode},
	}
	sm, _ := translateMessage(m)

	assert.Equal(t, session.MessageShell, sm.Type)
	assert.Equal(t, "git status", sm.Command)
	require.NotNil(t, sm.ExitCode)
	assert.Equal(t, 0, *sm.ExitCode)
}

func TestTranslateMessage_AgentSwitch(t *testing.T) {
	m := ocMessage{
		Info: ocInfo{Type: "agent_switch", ID: "msg_sw", FromAgent: "build", ToAgent: "code"},
	}
	sm, _ := translateMessage(m)
	assert.Equal(t, session.MessageAgentSwitch, sm.Type)
	assert.Equal(t, "build", sm.FromAgent)
	assert.Equal(t, "code", sm.ToAgent)
}

func TestTranslateMessage_ModelSwitch(t *testing.T) {
	m := ocMessage{
		Info: ocInfo{Type: "model_switch", ID: "msg_sw"},
	}
	m.Info.FromModel = &ocModelRef{ID: "gpt-4o", Provider: "openai"}
	m.Info.ToModel = &ocModelRef{ID: "claude-3", Provider: "anthropic"}

	sm, _ := translateMessage(m)
	require.Equal(t, session.MessageModelSwitch, sm.Type)
	require.NotNil(t, sm.FromModel)
	assert.Equal(t, "gpt-4o", sm.FromModel.ID)
	assert.Equal(t, "openai", sm.FromModel.Provider)
	require.NotNil(t, sm.ToModel)
	assert.Equal(t, "claude-3", sm.ToModel.ID)
}

func TestTranslateMessage_Compaction(t *testing.T) {
	m := ocMessage{Info: ocInfo{Type: "compaction", ID: "msg_c"}, Parts: []ocPart{
		{Type: "text", Text: "context compressed"},
	}}
	sm, _ := translateMessage(m)
	assert.Equal(t, session.MessageCompaction, sm.Type)
}

func TestTranslateMessage_SystemMessage(t *testing.T) {
	m := ocMessage{Info: ocInfo{Type: "system", ID: "msg_s"}, Parts: []ocPart{
		{Type: "text", Text: "system notice"},
	}}
	sm, _ := translateMessage(m)
	assert.Equal(t, session.MessageSystem, sm.Type)
}

func TestTranslateMessage_UnknownRole_FallsBackToSystem(t *testing.T) {
	// If opencode emits a role+type pair the adapter does not
	// recognize, fall back to MessageSystem rather than dropping the
	// message (timeline coherence per design 0049 §4.5).
	m := ocMessage{Info: ocInfo{Role: "unknown_role", ID: "msg_x"}, Parts: []ocPart{
		{Type: "text", Text: "preserved"},
	}}
	sm, _ := translateMessage(m)
	assert.Equal(t, session.MessageSystem, sm.Type)
	require.Len(t, sm.Parts, 1)
}

func TestTranslateMessage_ErrorPropagated(t *testing.T) {
	m := ocMessage{
		Info:  ocInfo{Role: "assistant", ID: "msg_e"},
		Error: &session.Error{Code: "rate_limited", Message: "Rate limited"},
	}
	sm, _ := translateMessage(m)
	require.NotNil(t, sm.Error)
	assert.Equal(t, "rate_limited", sm.Error.Code)
	assert.Equal(t, "Rate limited", sm.Error.Message)
}

func TestTranslateMessage_CostAndTime(t *testing.T) {
	// Regression test: timestamp must be parsed from info.time.created
	// (epoch millis) — NOT from a top-level time field. Pre-fix the
	// translator read from the wrong JSON hierarchy, producing zero
	// timestamps that broke frontend message ordering.
	raw := []byte(`{
		"info": {
			"role": "assistant",
			"id": "msg_t",
			"time": {"created": 1723291200000}
		},
		"cost": {"input": 100, "output": 50, "cost": 0.001}
	}`)
	var m ocMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	sm, _ := translateMessage(m)

	expected := time.UnixMilli(1723291200000).UTC()
	require.NotNil(t, sm.CreatedAt, "createdAt must be non-nil when info.time.created is present")
	assert.Equal(t, expected, *sm.CreatedAt,
		"createdAt must be parsed from info.time.created epoch millis")
	require.NotNil(t, sm.Cost)
	assert.Equal(t, int64(100), sm.Cost.InputTokens)
	assert.Equal(t, 0.001, sm.Cost.CostUSD)
}

func TestTranslateTool_Nil(t *testing.T) {
	assert.Nil(t, translateTool(nil))
}

// TestParseHistoryWire_ToolNull_ProducesNoToolPart covers the C1 edge
// case from the #731 review: a "tool": null field must NOT produce an
// empty &ocTool{} — it should leave Tool nil so the translator skips
// the tool part entirely.
func TestParseHistoryWire_ToolNull_ProducesNoToolPart(t *testing.T) {
	body := []byte(`[{
		"info": {"role": "assistant", "id": "msg_null"},
		"parts": [
			{"type": "tool", "tool": null}
		]
	}]`)
	msgs, _, _, err := ParseHistoryWire(body, "ws-null")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	for _, p := range msgs[0].Parts {
		assert.NotEqual(t, session.PartTool, p.Type, "\"tool\":null must not produce a tool part")
	}
}

func TestTranslateTool_NoState(t *testing.T) {
	tp := translateTool(&ocTool{Name: "bash"})
	require.NotNil(t, tp)
	assert.Equal(t, "bash", tp.Name)
	// Missing state → zero-value ToolState with pending status.
	assert.Equal(t, session.ToolStatusPending, tp.State.Status,
		"missing state must default to pending, not zero-value \"\"")
}

func TestParseHistoryWire_RealShape(t *testing.T) {
	// Captured-shape fixture matching the proxy_filter_test.go
	// opencodeHistoryBody. The adapter must produce the same shape
	// the proxy currently passes through, minus the patch parts (which
	// become FileChange via filediff in the adapter).
	body := []byte(`[
		{
			"info": {"role":"user","id":"msg_0"},
			"parts": [
				{"type":"text","text":"hi"},
				{"type":"patch","files":["/workspace/x"]}
			]
		},
		{
			"info": {"role":"assistant","id":"msg_1"},
			"parts": [
				{"type":"text","text":"hello"},
				{"type":"patch","files":["/workspace/y"]}
			]
		}
	]`)

	msgs, changedFiles, _, err := ParseHistoryWire(body, "ws-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, changedFiles, 2, "changedFilesPerMsg parallels msgs")

	assert.Equal(t, session.MessageUser, msgs[0].Type)
	assert.Equal(t, "msg_0", msgs[0].ID)
	require.Len(t, msgs[0].Parts, 1, "patch part must NOT survive into the contract shape")
	require.Len(t, changedFiles[0], 1)
	assert.Contains(t, changedFiles[0], "/workspace/x")

	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
	assert.Equal(t, "msg_1", msgs[1].ID)
	require.Len(t, changedFiles[1], 1)
	assert.Contains(t, changedFiles[1], "/workspace/y")
}

func TestParseHistoryWire_MalformedJSON(t *testing.T) {
	_, _, _, err := ParseHistoryWire([]byte(`not json`), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opencode history:")
}

func TestParseSessionListWire_BareArray(t *testing.T) {
	body := []byte(`[
		{"id":"ses_1","title":"first","status":{"type":"idle"}},
		{"id":"ses_2","title":"second","status":{"type":"busy"}}
	]`)
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "ws-1", sessions[0].WorkspaceID)
	assert.Equal(t, session.StatusIdle, sessions[0].Status)
	assert.Equal(t, session.StatusBusy, sessions[1].Status)
}

func TestParseSessionListWire_WrappedData(t *testing.T) {
	// Some opencode versions wrap in {data: [...]}.
	body := []byte(`{"data":[
		{"id":"ses_1","status":{"type":"idle"}}
	]}`)
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "ses_1", sessions[0].ID)
}

func TestParseSessionListWire_MalformedJSON(t *testing.T) {
	_, err := ParseSessionListWire([]byte(`not json`), "ws-1")
	require.Error(t, err)
}

func TestParseSessionWire_WrappedData(t *testing.T) {
	body := []byte(`{"data":{
		"id":"ses_1",
		"title":"my session",
		"status":{"type":"idle"},
		"model":{"id":"gpt-4o","provider":"openai"}
	}}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_1", s.ID)
	assert.Equal(t, "my session", s.Title)
	assert.Equal(t, session.StatusIdle, s.Status)
	require.NotNil(t, s.Model)
	assert.Equal(t, "gpt-4o", s.Model.ID)
	assert.Equal(t, "openai", s.Model.Provider)
}

func TestParseSessionWire_BareObject(t *testing.T) {
	body := []byte(`{"id":"ses_2","status":{"type":"busy"}}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_2", s.ID)
	assert.Equal(t, session.StatusBusy, s.Status)
}

func TestTranslateStatus_AllVariants(t *testing.T) {
	cases := []struct {
		in  string
		out session.Status
	}{
		{"idle", session.StatusIdle},
		{"busy", session.StatusBusy},
		{"retry", session.StatusBusy}, // retry treated as busy
		{"error", session.StatusError},
		{"compacting", session.StatusCompacting},
		{"archived", session.StatusArchived},
		{"unknown_future_status", session.StatusUnknown},
		{"", session.StatusUnknown},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.out, translateStatus(c.in))
		})
	}
}

func TestTranslateToolStatus_AllVariants(t *testing.T) {
	cases := []struct {
		in  string
		out session.ToolStatus
	}{
		{"pending", session.ToolStatusPending},
		{"running", session.ToolStatusRunning},
		{"completed", session.ToolStatusCompleted},
		{"error", session.ToolStatusError},
		{"future_status", session.ToolStatusPending}, // unknown → pending (safe default)
		{"", session.ToolStatusPending},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.out, translateToolStatus(c.in))
		})
	}
}

// ---------------------------------------------------------------------------
// Issue #730 regression tests: opencode 1.18.10 flat-string tool shape.
//
// The Epic 65 parser declared ocPart.tool as *ocTool (object), but opencode
// 1.18.10 emits "tool" as a bare string (the tool name) with callID/state/
// input/output hoisted onto the part itself. The whole history 502'd.
//
// Golden fixtures in testdata/ are the schema pins: any future opencode
// wire-shape change will fail these tests loudly instead of becoming a Sev1.
// ---------------------------------------------------------------------------

// mustLoadFixture loads a golden payload from testdata/. Golden files are the
// verbatim captured wire shapes and MUST NOT be hand-edited to satisfy a test
// — re-capture from a real pod if opencode's shape changes.
func mustLoadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err, "missing golden fixture testdata/%s", name)
	return b
}

// TestParseHistoryWire_RealShape1_18_10_FlatTool is the PRIMARY regression
// test for issue #730. It feeds the verbatim captured 1.18.10 wire shape
// (tool as a bare string + part-level callID/state/input/output) and asserts
// the translator produces a correct session.ToolPart.
//
// RED pre-fix: fails with the production error
// "cannot unmarshal string into Go struct field ocPart.parts.tool of type
// opencode.ocTool".
// GREEN post-fix: all assertions pass.
func TestParseHistoryWire_RealShape1_18_10_FlatTool(t *testing.T) {
	body := mustLoadFixture(t, "history_1_18_10_flat_tool.json")

	msgs, _, _, err := ParseHistoryWire(body, "ws-1")
	require.NoError(t, err, "flat-string tool shape must decode without error")
	require.Len(t, msgs, 2, "user message + assistant message")

	// Assistant message (index 1) carries the tool part.
	assistant := msgs[1]
	require.Equal(t, session.MessageAssistant, assistant.Type)

	var toolPart *session.ToolPart
	for _, p := range assistant.Parts {
		if p.Type == session.PartTool {
			toolPart = p.Tool
			break
		}
	}
	require.NotNil(t, toolPart, "expected exactly one ToolPart in the assistant message")

	// Name came from the part-level "tool":"bash" string (not nested object).
	assert.Equal(t, "bash", toolPart.Name)

	// CallID came from the part-level callID (hoisted in 1.18.10).
	assert.Equal(t, "call_80396e4d40744245897866a7", toolPart.CallID)

	// State status came from state.status.
	assert.Equal(t, session.ToolStatusCompleted, toolPart.State.Status,
		"state.status must map to completed")

	// Input came from state.input (NOT a top-level part field in 1.18.10).
	require.NotNil(t, toolPart.Input, "state.input must populate ToolPart.Input")
	var in map[string]string
	require.NoError(t, json.Unmarshal(toolPart.Input, &in))
	assert.Contains(t, in["command"], "git clone https://github.com/lenaxia/llmsafespaces.git")

	// Output came from state.output.
	require.NotNil(t, toolPart.Output, "state.output must populate ToolPart.Output")
	var out string
	require.NoError(t, json.Unmarshal(toolPart.Output, &out), "output is a string in 1.18.10")
	assert.Contains(t, out, "Cloning into '/workspace/llmsafespaces'")

	// StartedAt came from state.time.start (epoch-millis number).
	require.NotNil(t, toolPart.State.StartedAt, "state.time.start must populate StartedAt")
	assert.Equal(t, time.UnixMilli(1786374885930), *toolPart.State.StartedAt)

	// CompletedAt came from state.time.end (epoch-millis number).
	require.NotNil(t, toolPart.State.CompletedAt, "state.time.end must populate CompletedAt")
	assert.Equal(t, time.UnixMilli(1786374894033), *toolPart.State.CompletedAt)
}

// TestParseHistoryWire_LegacyNestedTool_StillWorks guards the 1.15.x path.
// Prod still runs workspaces on opencode 1.15.12 (confirmed in cluster logs),
// so the fix must not regress the legacy nested-object tool shape. This test
// must stay GREEN before AND after the fix.
func TestParseHistoryWire_LegacyNestedTool_StillWorks(t *testing.T) {
	body := mustLoadFixture(t, "history_1_15_12_nested_tool.json")

	msgs, _, _, err := ParseHistoryWire(body, "ws-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	assistant := msgs[1]
	require.Equal(t, session.MessageAssistant, assistant.Type)

	var toolPart *session.ToolPart
	for _, p := range assistant.Parts {
		if p.Type == session.PartTool {
			toolPart = p.Tool
			break
		}
	}
	require.NotNil(t, toolPart)

	assert.Equal(t, "bash", toolPart.Name, "name from nested tool.name")
	assert.Equal(t, "call_legacy_1", toolPart.CallID, "callID from nested tool.callID")
	assert.Equal(t, session.ToolStatusCompleted, toolPart.State.Status)

	// Input came from the nested tool.input object.
	require.NotNil(t, toolPart.Input)
	var in map[string]string
	require.NoError(t, json.Unmarshal(toolPart.Input, &in))
	assert.Equal(t, "ls -la", in["command"])

	// Output came from the nested tool.output object.
	require.NotNil(t, toolPart.Output)
	var outMap map[string]string
	require.NoError(t, json.Unmarshal(toolPart.Output, &outMap))
	assert.Contains(t, outMap["stdout"], "total 0")

	// Legacy shape uses ISO-8601 strings for startedAt/completedAt (NOT
	// state.time.{start,end} epoch-millis).
	require.NotNil(t, toolPart.State.StartedAt)
	assert.Equal(t, "2025-08-10T12:00:05Z", toolPart.State.StartedAt.UTC().Format(time.RFC3339))
	require.NotNil(t, toolPart.State.CompletedAt)
	assert.Equal(t, "2025-08-10T12:00:06Z", toolPart.State.CompletedAt.UTC().Format(time.RFC3339))
}

// TestParseHistoryWire_MixedShapesInOneHistory covers the realistic fleet
// case during a rolling upgrade: a single history array containing both the
// legacy nested-object tool shape and the new flat-string tool shape. Both
// must translate correctly — the array is built by concatenating the two
// golden fixtures' messages.
func TestParseHistoryWire_MixedShapesInOneHistory(t *testing.T) {
	flatBody := mustLoadFixture(t, "history_1_18_10_flat_tool.json")
	nestedBody := mustLoadFixture(t, "history_1_15_12_nested_tool.json")

	var flatMsgs, nestedMsgs []json.RawMessage
	require.NoError(t, json.Unmarshal(flatBody, &flatMsgs))
	require.NoError(t, json.Unmarshal(nestedBody, &nestedMsgs))

	// Interleave: nested-user, flat-user, nested-assistant, flat-assistant.
	mixed := []json.RawMessage{
		nestedMsgs[0], flatMsgs[0], nestedMsgs[1], flatMsgs[1],
	}
	body, err := json.Marshal(mixed)
	require.NoError(t, err)

	msgs, _, _, err := ParseHistoryWire(body, "ws-mixed")
	require.NoError(t, err, "mixed shapes must both decode")
	require.Len(t, msgs, 4, "all four messages must survive")

	// mixed = [nested-user(0), flat-user(1), nested-assistant(2), flat-assistant(3)].
	// Indices 2 and 3 carry the tool parts.
	for _, idx := range []int{2, 3} {
		var toolPart *session.ToolPart
		for _, p := range msgs[idx].Parts {
			if p.Type == session.PartTool {
				toolPart = p.Tool
				break
			}
		}
		require.NotNil(t, toolPart, "assistant at mixed index %d must have a tool part", idx)
		assert.Equal(t, "bash", toolPart.Name, "both shapes resolve to name=bash")
		assert.Equal(t, session.ToolStatusCompleted, toolPart.State.Status)
		assert.NotNil(t, toolPart.Input, "input must be populated for both shapes")
		assert.NotNil(t, toolPart.State.StartedAt, "startedAt must be populated for both shapes")
	}
}

// TestParseHistoryWire_OneMalformedPart_DoesNot502 validates Fix 2
// (per-message resilience). One message with an undecodable part must NOT
// fail the whole history — it downgrades to a MessageSystem notice while
// the surrounding messages translate normally. This is the containment
// rule (README §12): one bad upstream shape must never Sev1 the surface.
func TestParseHistoryWire_OneMalformedPart_DoesNot502(t *testing.T) {
	// Index 1 has a part whose "tool" field is a number — a shape neither
	// the legacy nested nor the new flat-string path can decode. Simulates
	// a future opencode schema change.
	body := []byte(`[
		{
			"info": {"role": "assistant", "id": "msg_good_before"},
			"parts": [
				{"type": "tool", "callID": "call_ok_1", "tool": "read", "state": {"status": "completed", "input": {"path": "/a"}, "output": "ok", "time": {"start": 1786374885930, "end": 1786374894033}}}
			]
		},
		{
			"info": {"role": "assistant", "id": "msg_bad"},
			"parts": [
				{"type": "tool", "tool": 42}
			]
		},
		{
			"info": {"role": "assistant", "id": "msg_good_after"},
			"parts": [
				{"type": "tool", "callID": "call_ok_2", "tool": "read", "state": {"status": "completed", "input": {"path": "/b"}, "output": "ok", "time": {"start": 1786374885930, "end": 1786374894033}}}
			]
		}
	]`)

	msgs, _, _, err := ParseHistoryWire(body, "ws-resilience")
	require.NoError(t, err, "one malformed message must NOT fail the whole history")
	require.Len(t, msgs, 3, "all three messages must be present (bad one downgraded, not dropped)")

	// Good messages before and after still translate correctly.
	assert.Equal(t, "msg_good_before", msgs[0].ID)
	var tpBefore *session.ToolPart
	for _, p := range msgs[0].Parts {
		if p.Type == session.PartTool {
			tpBefore = p.Tool
		}
	}
	require.NotNil(t, tpBefore, "well-formed message before the bad one must keep its tool part")
	assert.Equal(t, "read", tpBefore.Name)

	assert.Equal(t, "msg_good_after", msgs[2].ID)
	var tpAfter *session.ToolPart
	for _, p := range msgs[2].Parts {
		if p.Type == session.PartTool {
			tpAfter = p.Tool
		}
	}
	require.NotNil(t, tpAfter, "well-formed message after the bad one must keep its tool part")
	assert.Equal(t, "read", tpAfter.Name)

	// The malformed message is downgraded to a system notice with
	// non-empty text. Its raw bytes are NOT echoed (avoid leaking
	// potentially huge/garbage payloads to the UI).
	assert.Equal(t, session.MessageSystem, msgs[1].Type,
		"undecodable message must be downgraded to MessageSystem")
	assert.NotEmpty(t, msgs[1].Text, "downgraded message must carry an explanatory text")
	assert.NotContains(t, msgs[1].Text, "42",
		"raw malformed bytes must not leak into the downgrade text")
}

// TestParseHistoryWire_TotallyGarbage_StillErrors is defense-in-depth for
// Fix 2: the resilience must NOT over-correct into swallowing genuine
// decode failures. A body that is not a JSON array at all still returns a
// clear top-level error so real bugs surface.
func TestParseHistoryWire_TotallyGarbage_StillErrors(t *testing.T) {
	cases := map[string][]byte{
		"not json":         []byte(`not json`),
		"object not array": []byte(`{"unexpected":"object"}`),
		"html 404":         []byte(`<html>404</html>`),
		"empty":            []byte(``),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := ParseHistoryWire(body, "ws-1")
			require.Error(t, err, "genuinely malformed body must still error")
			assert.Contains(t, err.Error(), "opencode history:",
				"error must carry the established wrap prefix")
		})
	}
}

// TestParseHistoryWire_AllObservedToolNames_1_18_10 is a breadth sweep over
// every tool name observed in the captured production payload. The fix must
// handle all of them, not just bash.
func TestParseHistoryWire_AllObservedToolNames_1_18_10(t *testing.T) {
	observedNames := []string{
		"bash", "edit", "glob", "grep", "question",
		"read", "task", "todowrite", "write",
	}
	for _, name := range observedNames {
		t.Run(name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`[{
				"info": {"role": "assistant", "id": "msg_%s"},
				"parts": [
					{"type": "tool", "callID": "call_%s", "tool": "%s", "state": {"status": "completed", "input": {}, "output": "", "time": {"start": 1786374885930, "end": 1786374894033}}}
				]
			}]`, name, name, name))
			msgs, _, _, err := ParseHistoryWire(body, "ws-names")
			require.NoError(t, err, "tool name %q must decode in the flat shape", name)
			require.Len(t, msgs, 1)
			var tp *session.ToolPart
			for _, p := range msgs[0].Parts {
				if p.Type == session.PartTool {
					tp = p.Tool
				}
			}
			require.NotNil(t, tp)
			assert.Equal(t, name, tp.Name)
			assert.Equal(t, "call_"+name, tp.CallID)
		})
	}
}

// Compile-time check that the wire shapes don't accidentally carry
// agent-specific identifiers into the contract vocabulary. The
// session package's source scan in contract_test.go already pins
// this for pkg/session itself; this test pins it for the translator.
func TestTranslateMessage_OutputHasNoForbiddenIdentifiers(t *testing.T) {
	// Per design 0049 §4.1 rule 1, opencode-specific part types
	// (step-start, step-finish, patch) must not appear in the output.
	// The translator drops them or converts them to Custom; it must
	// never produce a session.Part with one of those types.
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_1"},
		Parts: []ocPart{
			{Type: "step-start"},
			{Type: "text", Text: "x"},
			{Type: "step-finish"},
			{Type: "patch", Files: []string{"/x"}},
		},
	}
	sm, _ := translateMessage(m)
	for _, p := range sm.Parts {
		assert.NotContains(t, []string{"step-start", "step-finish", "patch"}, string(p.Type),
			"opencode-specific part type leaked into contract output")
	}
}

// --- V2 store translation (design 0052) ---

func TestTranslateV2Message_User(t *testing.T) {
	m := V2Message{ID: "m1", Type: "user", Text: "hello v2"}
	m.Time.Created = 1787880243128
	sm := translateV2Message(m)
	assert.Equal(t, session.MessageUser, sm.Type)
	assert.Equal(t, "m1", sm.ID)
	require.Len(t, sm.Parts, 1)
	assert.Equal(t, session.PartText, sm.Parts[0].Type)
	assert.Equal(t, "hello v2", sm.Parts[0].Text)
	require.NotNil(t, sm.CreatedAt)
	assert.Equal(t, int64(1787880243128), sm.CreatedAt.UnixMilli())
}

func TestTranslateV2Message_AssistantContent(t *testing.T) {
	m := V2Message{ID: "m2", Type: "assistant"}
	m.Time.Created = 1787880245324
	m.Time.Completed = 1787880245433
	m.Model = &V2ModelInMessage{ID: "mock-1", ProviderID: "mockprov"}
	m.Tokens = &V2Tokens{}
	m.Tokens.Input = 126
	m.Tokens.Output = 36
	m.Cost = 0.0025

	toolState := &V2ToolState{Status: "completed", Input: json.RawMessage(`{"command":"echo hi"}`)}
	toolState.Content = []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: "hi\n"}, {Type: "text", Text: "Command exited with code 0."}}
	toolState.Time = &struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran,omitempty"`
		Completed int64 `json:"completed,omitempty"`
	}{Created: 1787880245342, Ran: 1787880245353, Completed: 1787880245428}
	m.Content = []V2ContentPart{
		{Type: "text", ID: "text-0", Text: "checking"},
		{Type: "tool", ID: "call_7", Name: "bash", State: toolState},
		{Type: "step-start"},
		{Type: "future-thing", ID: "x1", Raw: json.RawMessage(`{"type":"future-thing"}`)},
	}

	sm := translateV2Message(m)
	assert.Equal(t, session.MessageAssistant, sm.Type)
	require.Len(t, sm.Parts, 3, "step-start dropped, unknown preserved as Custom")

	assert.Equal(t, session.PartText, sm.Parts[0].Type)
	assert.Equal(t, "checking", sm.Parts[0].Text)

	tp := sm.Parts[1].Tool
	require.NotNil(t, tp)
	assert.Equal(t, "call_7", tp.CallID)
	assert.Equal(t, "bash", tp.Name)
	assert.Equal(t, string(session.ToolStatusCompleted), string(tp.State.Status))
	assert.JSONEq(t, `{"command":"echo hi"}`, string(tp.Input))
	assert.JSONEq(t, `"hi\nCommand exited with code 0."`, string(tp.Output))
	require.NotNil(t, tp.State.StartedAt)
	assert.Equal(t, int64(1787880245353), tp.State.StartedAt.UnixMilli())

	assert.Equal(t, session.PartCustom, sm.Parts[2].Type)
	assert.Equal(t, "future-thing", sm.Parts[2].Custom.Kind)

	require.NotNil(t, sm.Model)
	assert.Equal(t, "mock-1", sm.Model.ID)
	assert.Equal(t, "mockprov", sm.Model.Provider)
	require.NotNil(t, sm.Cost)
	assert.InDelta(t, 0.0025, sm.Cost.CostUSD, 1e-9)
	assert.Equal(t, int64(126), sm.Cost.InputTokens)
}

func TestTranslateV2Message_System(t *testing.T) {
	sm := translateV2Message(V2Message{ID: "m3", Type: "system"})
	assert.Equal(t, session.MessageSystem, sm.Type)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
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
	started := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	ended := started.Add(5 * time.Second)
	m := ocMessage{
		Info: ocInfo{Role: "assistant", ID: "msg_t"},
		Time: &ocTime{StartedAt: started, CompletedAt: &ended},
		Cost: &ocCost{InputTokens: 100, OutputTokens: 50, CostUSD: 0.001},
	}
	sm, _ := translateMessage(m)
	assert.Equal(t, started, sm.CreatedAt)
	require.NotNil(t, sm.Cost)
	assert.Equal(t, int64(100), sm.Cost.InputTokens)
	assert.Equal(t, 0.001, sm.Cost.CostUSD)
}

func TestTranslateTool_Nil(t *testing.T) {
	assert.Nil(t, translateTool(nil))
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

	msgs, err := ParseHistoryWire(body, "ws-1")
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	assert.Equal(t, session.MessageUser, msgs[0].Type)
	assert.Equal(t, "msg_0", msgs[0].ID)
	require.Len(t, msgs[0].Parts, 1, "patch part must NOT survive into the contract shape")

	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
	assert.Equal(t, "msg_1", msgs[1].ID)
}

func TestParseHistoryWire_MalformedJSON(t *testing.T) {
	_, err := ParseHistoryWire([]byte(`not json`), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse message array")
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

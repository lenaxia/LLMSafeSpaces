// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// transcripthygiene_test.go — the #2030a born-red pins for the pi
// driver-side serve tiers (pin (a) + (b)): the platform view the pi
// adapter composes from pi's persisted session entries carries ZERO
// identity keys in tool-call arguments, while the /workspace/pi-sessions
// entry (the fixture bytes — the file stand-in) keeps them byte-intact
// for attribution. Existing sessions heal forward on the next serve.

// stampedEntriesJSON is one get_entries payload whose assistant
// message carries a stamped platform tool call — the pi-sessions file
// shape (attribution's durable home), stamps included.
const stampedEntriesJSON = `[{"type":"message","id":"entry_1","timestamp":"2026-11-15T10:00:00Z",
	"message":{"role":"assistant","timestamp":1787000000,"content":[
		{"type":"text","text":"importing the plan"},
		{"type":"toolCall","id":"call_1","name":"llmsafespaces_board_import_plan",
		 "arguments":{"projectId":"prj_9","planFile":"plan.json","lsp_injected_session":"ses_ORIGIN"}},
		{"type":"toolCall","id":"call_2","name":"llmsafespaces_send_message",
		 "arguments":{"destination":"sacred-yacht-dines","message":"hi","sessionId":"ses_CAMEL","session_id":"ses_SNAKE"}}
	]}}]`

func TestAdapter_StripIdentityStamps_BornRed(t *testing.T) {
	a := &Adapter{}
	served := a.StripIdentityStamps([]byte(stampedEntriesJSON))

	assert.NotContains(t, string(served), "lsp_injected_session",
		"the served view must contain zero identity stamps in tool args (pin a)")
	assert.NotContains(t, string(served), "ses_ORIGIN")
	assert.NotContains(t, string(served), "sessionId")
	assert.NotContains(t, string(served), "session_id")

	// Semantic params survive on every block.
	assert.Contains(t, string(served), `"projectId":"prj_9"`)
	assert.Contains(t, string(served), `"planFile":"plan.json"`)
	assert.Contains(t, string(served), `"destination":"sacred-yacht-dines"`)
	assert.Contains(t, string(served), `"message":"hi"`)
	// Non-tool content untouched.
	assert.Contains(t, string(served), `"text":"importing the plan"`)

	// Attribution (pin b): the persisted pi-sessions entry keeps the
	// stamps — the fixture bytes are the file stand-in.
	assert.Contains(t, stampedEntriesJSON, `"lsp_injected_session":"ses_ORIGIN"`)
}

func TestAdapter_StripIdentityStamps_CleanAndMalformedPassthrough(t *testing.T) {
	clean := `[{"type":"message","id":"e1","message":{"role":"assistant","content":[
		{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}]}}]`
	assert.Equal(t, clean, string((&Adapter{}).StripIdentityStamps([]byte(clean))),
		"a stamp-free payload must return byte-identical")

	for name, body := range map[string]string{
		"empty":     "",
		"not json":  "pi error prose",
		"object":    `{"leafId":"x"}`,
		"bare text": `[{"type":"message","id":"e1","message":{"role":"user","content":"plain turn"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, body, string((&Adapter{}).StripIdentityStamps([]byte(body))),
				"malformed/argless payloads pass through unchanged")
		})
	}
}

func TestAdapter_TranscriptHygieneContractSatisfied(t *testing.T) {
	var _ agent.TranscriptHygiene = (*Adapter)(nil)
}

// TestTranslateAssistantMessage_StripsStamps pins the translation
// tier: the served ToolPart built from a persisted entry strips the
// stamp from its arguments while the entry's own bytes (the file
// record) keep it.
func TestTranslateAssistantMessage_StripsStamps(t *testing.T) {
	var entries []piEntry
	require.NoError(t, json.Unmarshal([]byte(stampedEntriesJSON), &entries))
	require.Len(t, entries, 1)

	msg := translateAssistantMessage(&entries[0], entries[0].Message, nil)
	require.NotEmpty(t, msg.Parts)
	for _, p := range msg.Parts {
		if p.Type != "tool" || p.Tool == nil {
			continue
		}
		assert.NotContains(t, string(p.Tool.Input), "lsp_injected_session", p.Tool.CallID)
		assert.NotContains(t, string(p.Tool.Input), "sessionId", p.Tool.CallID)
		assert.NotContains(t, string(p.Tool.Input), "session_id", p.Tool.CallID)
		switch p.Tool.CallID {
		case "call_1":
			assert.Contains(t, string(p.Tool.Input), `"projectId":"prj_9"`)
		case "call_2":
			assert.Contains(t, string(p.Tool.Input), `"destination":"sacred-yacht-dines"`)
		}
	}
	// Attribution (pin b): the decoded entry's persisted arguments
	// bytes still carry the stamps (the strip is serve-side only).
	blk := entries[0].Message.blocks()
	assert.Contains(t, string(blk[1].Arguments), `"lsp_injected_session":"ses_ORIGIN"`)
	assert.Contains(t, string(blk[2].Arguments), `"sessionId":"ses_CAMEL"`)
}

// TestTranslateAssistantMessage_CleanArgsByteStable pins the
// byte-stability arm: unstamped arguments ride through untouched.
func TestTranslateAssistantMessage_CleanArgsByteStable(t *testing.T) {
	entry := &piEntry{Type: "message", ID: "e2", Message: &piAgentMessage{
		Role:    "assistant",
		Content: json.RawMessage(`[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls","workdir":"/w"}}]`),
	}}
	msg := translateAssistantMessage(entry, entry.Message, nil)
	for _, p := range msg.Parts {
		if p.Type == "tool" && p.Tool != nil {
			assert.JSONEq(t, `{"command":"ls","workdir":"/w"}`, string(p.Tool.Input))
		}
	}
}

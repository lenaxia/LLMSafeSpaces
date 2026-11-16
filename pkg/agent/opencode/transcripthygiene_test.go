// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// transcripthygiene_test.go — the #2030a born-red pins for the
// opencode serve tiers: a SERVED transcript built from stamp-carrying
// history contains ZERO identity keys in tool-call args, while the
// stored record (the fixture bytes — the stand-in for opencode's
// session DB) keeps them for attribution (pin (a) + (b)).
//
// The stamp set is the plugin's exact injection
// (runtimes/opencode/plugins/llmsafespaces-origin.js) plus the two
// legacy spellings the accept-side strip carries
// (board_manager_tools.go boardWireBody) — the replay-side mirror.

// flatStampedBody is a V1 message list (the loopback seam's
// SessionMessagesRaw shape) in the flat 1.18.x tool-part shape with a
// stamped board manager call — the live #2030 bleed shape
// (board_import_plan projectId surviving beside the stamp).
const flatStampedBody = `[{"info":{"id":"msg_1","role":"assistant"},"parts":[
	{"type":"text","text":"calling the board"},
	{"type":"tool","tool":"llmsafespaces_board_import_plan","callID":"call_1",
	 "state":{"status":"completed","input":{"projectId":"prj_9","planFile":"plan.json","lsp_injected_session":"ses_ORIGIN"},"output":"{\"ok\":true}","time":{"start":1787000000000,"end":1787000000123}}}
]},{"info":{"id":"msg_2","role":"user"},"parts":[{"type":"text","text":"continue"}]}]`

// legacyStampedBody is the ≤1.15.x nested tool shape, stamped with
// both legacy spellings.
const legacyStampedBody = `[{"info":{"id":"msg_1","role":"assistant"},"parts":[
	{"type":"tool","tool":{"name":"llmsafespaces_send_message","callID":"call_2",
	 "input":{"destination":"sacred-yacht-dines","message":"hi","sessionId":"ses_CAMEL","session_id":"ses_SNAKE"},
	 "state":{"status":"completed"}}}
]}]`

func TestStripIdentityStamps_FlatShapeBornRed(t *testing.T) {
	served := StripIdentityStamps([]byte(flatStampedBody))

	assert.NotContains(t, string(served), "lsp_injected_session",
		"the served view must contain zero identity stamps in tool args (pin a)")
	assert.NotContains(t, string(served), "ses_ORIGIN")

	// The semantic params survive — the bleed variant's whole point:
	// projectId must not be collateral damage of the strip.
	assert.Contains(t, string(served), `"projectId":"prj_9"`)
	assert.Contains(t, string(served), `"planFile":"plan.json"`)
	assert.Contains(t, string(served), `"llmsafespaces_board_import_plan"`)
	// Non-tool parts untouched.
	assert.Contains(t, string(served), `"text":"calling the board"`)
	assert.Contains(t, string(served), `"text":"continue"`)

	// Attribution (pin b): the STORED record keeps the stamp — the
	// fixture bytes are the stand-in for opencode's session DB.
	assert.Contains(t, flatStampedBody, `"lsp_injected_session":"ses_ORIGIN"`,
		"the stored record keeps the stamp (pure serve-side strip)")
}

func TestStripIdentityStamps_LegacyNestedShape(t *testing.T) {
	served := StripIdentityStamps([]byte(legacyStampedBody))
	assert.NotContains(t, string(served), "sessionId")
	assert.NotContains(t, string(served), "session_id")
	assert.NotContains(t, string(served), "ses_CAMEL")
	assert.NotContains(t, string(served), "ses_SNAKE")
	assert.Contains(t, string(served), `"destination":"sacred-yacht-dines"`)
	assert.Contains(t, string(served), `"message":"hi"`)
	assert.Contains(t, legacyStampedBody, `"sessionId":"ses_CAMEL"`,
		"stored record keeps the legacy stamps")
}

func TestStripIdentityStamps_CleanPayloadByteStable(t *testing.T) {
	clean := `[{"info":{"id":"m","role":"assistant"},"parts":[
		{"type":"tool","tool":"bash","callID":"c","state":{"status":"completed","input":{"command":"ls","workdir":"/workspace"}}}
	]}]`
	assert.Equal(t, clean, string(StripIdentityStamps([]byte(clean))),
		"a stamp-free payload must return byte-identical (never-break + cache stability)")
}

func TestStripIdentityStamps_NeverBreaksPayload(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "internal error prose",
		"object not ms": `{"error":"shape drift"}`,
		"parts not arr": `[{"info":{"id":"m"},"parts":"nope"}]`,
		"input not obj": `[{"info":{"id":"m"},"parts":[{"type":"tool","tool":"bash","state":{"input":"ls"}}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, body, string(StripIdentityStamps([]byte(body))),
				"malformed/nonconforming payloads pass through unchanged")
		})
	}
}

func TestStripIdentityStamps_AdapterContractSatisfied(t *testing.T) {
	var _ agent.TranscriptHygiene = (*Adapter)(nil)
	a := &Adapter{}
	served := a.StripIdentityStamps([]byte(flatStampedBody))
	assert.NotContains(t, string(served), "lsp_injected_session")
}

// TestAcpToolCallFromNative_StripsStamps pins the translation tier:
// every ToolPart the platform contract serves (history reads, pages,
// SSE) strips the stamp from native tool args — the native ocTool
// carries Input for BOTH wire generations (flat state.input decodes
// into ocTool.Input too).
func TestAcpToolCallFromNative_StripsStamps(t *testing.T) {
	stamped := json.RawMessage(`{"destination":"amber-heron-waits","message":"go","lsp_injected_session":"ses_X"}`)
	tc := AcpToolCallFromNative(&ocTool{CallID: "call_9", Name: "llmsafespaces_send_message", Input: stamped})
	require.NotNil(t, tc)
	assert.NotContains(t, string(tc.RawInput), "lsp_injected_session")
	assert.NotContains(t, string(tc.RawInput), "ses_X")
	assert.Contains(t, string(tc.RawInput), `"destination":"amber-heron-waits"`)
	assert.Contains(t, string(tc.RawInput), `"message":"go"`)

	clean := json.RawMessage(`{"command":"ls","workdir":"/w"}`)
	tc2 := AcpToolCallFromNative(&ocTool{CallID: "c2", Name: "bash", Input: clean})
	assert.Equal(t, string(clean), string(tc2.RawInput), "clean args stay byte-identical")
}

// TestTranslateV2Tool_StripsStamps pins the V2 history tier.
func TestTranslateV2Tool_StripsStamps(t *testing.T) {
	tp := translateV2Tool(V2ContentPart{
		Type: "tool", ID: "cp_1", Name: "llmsafespaces_board_list",
		State: &V2ToolState{Status: "completed", Input: json.RawMessage(`{"projectId":"prj_2","sessionId":"ses_C"}`)},
	})
	require.NotNil(t, tp)
	assert.NotContains(t, string(tp.Input), "sessionId")
	assert.NotContains(t, string(tp.Input), "ses_C")
	assert.Contains(t, string(tp.Input), `"projectId":"prj_2"`)
}

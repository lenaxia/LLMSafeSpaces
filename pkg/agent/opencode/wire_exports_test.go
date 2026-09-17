// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1372: the sessions-cluster Act path (agentd's actor) consumes the SAME
// exported wire seams the adapter path uses — single source of truth for
// the harness wire shapes (Rule 12 containment; no second translator to
// drift).

// TestParseMessageWire_SingleAssistantMessage pins the single-message
// parser against the captured 1.18.10 flat-tool fixture: the same bytes
// POST /session/:id/message returns, and the same translation
// translateMessage applies on the history path.
func TestParseMessageWire_SingleAssistantMessage(t *testing.T) {
	raw, err := os.ReadFile("testdata/history_1_18_10_flat_tool.json")
	require.NoError(t, err)
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &arr))
	require.GreaterOrEqual(t, len(arr), 2, "fixture carries the assistant exchange")

	msg, files, err := opencode.ParseMessageWire(arr[1])
	require.NoError(t, err)
	assert.Equal(t, "msg_fec3d8058001kyxP8bMldMDn42", msg.ID)
	assert.Equal(t, "ses_015c5a29effeVY00SdXAQIuRIL", msg.SessionID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
	require.NotEmpty(t, msg.Parts, "assistant parts translate")
	// text + reasoning + tool order preserved (fixture order)
	kinds := make([]session.PartType, 0, len(msg.Parts))
	for _, p := range msg.Parts {
		kinds = append(kinds, p.Type)
	}
	assert.Equal(t, []session.PartType{session.PartText, session.PartReasoning, session.PartTool}, kinds)
	assert.Nil(t, files, "no patch parts in this fixture — no changed files")
}

func TestParseMessageWire_RejectsNonObject(t *testing.T) {
	_, _, err := opencode.ParseMessageWire([]byte(`[]`))
	require.Error(t, err)
}

// TestMessageModelOverrideWire pins the message-route model object wire
// ({"modelID","providerID"}) — the split rules the adapter documents
// (provider-authoritative; first-segment split for provider-less slashed
// ids; bare flat and empty-tail forms are unexpressible and must be
// OMITTED so the session default applies).
func TestMessageModelOverrideWire(t *testing.T) {
	cases := []struct {
		name string
		ref  *session.ModelRef
		want map[string]string
		ok   bool
	}{
		{"nil omits", nil, nil, false},
		{"empty id omits", &session.ModelRef{ID: ""}, nil, false},
		{
			"provider-authoritative strips matching prefix",
			&session.ModelRef{ID: "thekaocloud/glm-5.3", Provider: "thekaocloud"},
			map[string]string{"modelID": "glm-5.3", "providerID": "thekaocloud"}, true,
		},
		{
			"provider with slashed id keeps full id (vendor namespace)",
			&session.ModelRef{ID: "anthropic/claude-sonnet-4.5", Provider: "openrouter"},
			map[string]string{"modelID": "anthropic/claude-sonnet-4.5", "providerID": "openrouter"}, true,
		},
		{
			"provider-less slashed splits first segment",
			&session.ModelRef{ID: "a/b/c"},
			map[string]string{"modelID": "b/c", "providerID": "a"}, true,
		},
		{"bare flat omits", &session.ModelRef{ID: "glm-5.3"}, nil, false},
		{"empty tail omits (matching provider)", &session.ModelRef{ID: "prov/", Provider: "prov"}, nil, false},
		{"empty tail omits (other provider)", &session.ModelRef{ID: "x/", Provider: "prov"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := opencode.MessageModelOverrideWire(tc.ref)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseMessageWire_RejectsTrailingBytes pins the strict-decode
// contract (the change rode 87896e3f undisclosed — this row is its
// disclosure pin): the send response must parse as exactly ONE JSON
// value; trailing bytes are wire drift or proxy corruption and must
// error, never parse as phantom success (the #1308 rationale; parity
// with the adapter's decodeStrict).
func TestParseMessageWire_RejectsTrailingBytes(t *testing.T) {
	raw, err := os.ReadFile("testdata/history_1_18_10_flat_tool.json")
	require.NoError(t, err)
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &arr))
	require.GreaterOrEqual(t, len(arr), 2)

	_, _, err = opencode.ParseMessageWire(append(append([]byte{}, arr[1]...), []byte(` trailing garbage`)...))
	require.Error(t, err, "trailing bytes after the JSON value must fail the parse")
	assert.Contains(t, err.Error(), "trailing")

	msg, _, err := opencode.ParseMessageWire(arr[1])
	require.NoError(t, err, "the clean fixture still parses")
	assert.NotEmpty(t, msg.ID)
}

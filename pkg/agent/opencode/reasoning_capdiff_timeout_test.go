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

// TestReasoningPart_ReadsTextField verifies that reasoning content is
// extracted from the "text" field on the 1.18.10 wire shape (#750).
// The golden fixture shows reasoning parts carry content under "text",
// not "reasoning".
func TestReasoningPart_ReadsTextField_1_18_10(t *testing.T) {
	body := loadFixture(t, "history_1_18_10_flat_tool.json")
	msgs, _, _, err := ParseHistoryWire(body, "ws-1")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)

	var foundReasoning *session.Part
	for i := range msgs {
		for j := range msgs[i].Parts {
			if msgs[i].Parts[j].Type == session.PartReasoning {
				foundReasoning = &msgs[i].Parts[j]
				break
			}
		}
		if foundReasoning != nil {
			break
		}
	}
	require.NotNil(t, foundReasoning, "fixture must contain at least one reasoning part")
	assert.NotEmpty(t, foundReasoning.Reasoning, "reasoning content must not be empty (was silently dropped before #750 fix)")
	assert.Contains(t, foundReasoning.Reasoning, "clone", "reasoning content must come from the text field")
}

// TestReasoningPart_LegacyReasoningField verifies backward compat with
// the 1.15.x wire shape where reasoning content uses the "reasoning" key.
func TestReasoningPart_LegacyReasoningField(t *testing.T) {
	raw := []byte(`[{"id":"msg_1","role":"assistant","info":{"role":"assistant"},"parts":[{"type":"reasoning","reasoning":"old style"}]}]`)
	msgs, _, _, err := ParseHistoryWire(raw, "ws-1")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.NotEmpty(t, msgs[0].Parts)
	assert.Equal(t, session.PartReasoning, msgs[0].Parts[0].Type)
	assert.Equal(t, "old style", msgs[0].Parts[0].Reasoning)
}

// TestOcModelRef_UnmarshalJSON_BothProviderKeys is already in
// model_agent_status_test.go but this directly tests the wire round-trip.
func TestParseHistoryWire_ProviderKeyRoundTrip(t *testing.T) {
	raw := []byte(`[{"id":"msg_1","role":"assistant","info":{"role":"assistant","model":{"id":"claude","provider":"anthropic"}},"parts":[{"type":"text","text":"hi"}]}]`)
	msgs, _, _, err := ParseHistoryWire(raw, "ws-1")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
}

// TestCapabilities_NoDiffer_NoCapDiff verifies that CapDiff is NOT
// advertised when the filediff producer is not wired (#745).
func TestCapabilities_NoDiffer_NoCapDiff(t *testing.T) {
	a := &Adapter{}
	caps := a.Capabilities()
	for _, c := range caps {
		assert.NotEqual(t, session.CapDiff, c, "CapDiff must not be advertised when differ is nil")
	}
}

// TestHTTPTclient_NoHardTimeout verifies the adapter HTTP client has no
// hard client-level timeout that would break sync Send on long LLM turns (#746).
func TestHTTPClient_NoHardTimeout(t *testing.T) {
	c := newTunedHTTPClient()
	assert.Equal(t, time.Duration(0), c.Timeout,
		"HTTP client must not have a hard timeout — context deadline is the correct boundary")
}

// TestReasoningWire_RoundTripJSON verifies that a raw JSON reasoning part
// with "text" field is correctly parsed via the JSON decoder (not just
// Go struct literals).
func TestReasoningWire_RoundTripJSON(t *testing.T) {
	rawPart := `{"type":"reasoning","text":"thinking about the approach"}`
	var p ocPart
	err := json.Unmarshal([]byte(rawPart), &p)
	require.NoError(t, err)
	assert.Equal(t, "thinking about the approach", p.Text, "text field must be parsed")
	assert.Equal(t, "", p.Reasoning, "reasoning field must be empty on wire")
}

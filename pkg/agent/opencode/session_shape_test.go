// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "failed to read fixture %s", name)
	return data
}

func TestParseSessionWire_RealShape1_18_10(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err, "ParseSessionWire must not fail on the 1.18.10 wire shape")
	require.NotNil(t, s)
	assert.NotEmpty(t, s.ID)
	assert.Equal(t, "Test Session", s.Title)
}

func TestParseSessionWire_RealShape1_18_10_TokensExtracted(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.Cost, "session.Cost must be populated from the tokens object")
	assert.Equal(t, int64(4868893), s.Cost.InputTokens)
	assert.Equal(t, int64(330410), s.Cost.OutputTokens)
	assert.Equal(t, int64(35310), s.Cost.ReasoningTokens)
	assert.Equal(t, int64(761649152), s.Cost.CacheReadTokens)
}

func TestParseSessionWire_SummaryString_Legacy1_15_12(t *testing.T) {
	body := []byte(`{"id":"ses_old","title":"Old","status":{"type":"idle"},"summary":"legacy string"}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_old", s.ID)
}

func TestParseSessionListWire_RealShape1_18_10(t *testing.T) {
	body := loadFixture(t, "session_list_1_18_10.json")
	sessions, err := ParseSessionListWire(body, "ws-1")
	require.NoError(t, err)
	require.True(t, len(sessions) >= 2)
}

func TestParseSessionWire_SummaryAbsent(t *testing.T) {
	body := []byte(`{"id":"ses_nosummary","status":{"type":"idle"}}`)
	s, err := ParseSessionWire(body, "ws-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, "ses_nosummary", s.ID)
}

// --- Contract tests against golden fixtures ---
//
// These tests lock down the wire-shape contract between opencode 1.18.10
// and our translate layer. If opencode changes any field name, type, or
// shape, these tests break with a clear fixture-vs-code diff rather than
// a mysterious 502 in production.

func TestContract_HistoryFlatTool_1_18_10(t *testing.T) {
	body := loadFixture(t, "history_1_18_10_flat_tool.json")
	msgs, changedFiles, downgraded, err := ParseHistoryStream(bytes.NewReader(body), "ws-contract")
	require.NoError(t, err, "flat tool history fixture must parse without error")
	assert.Equal(t, 0, downgraded, "no messages should be downgraded")
	require.Len(t, msgs, 2, "fixture has 2 messages (user + assistant with tool)")

	msg := msgs[1]
	require.NotEmpty(t, msg.Parts, "assistant message must have parts")
	var toolPart *session.Part
	for i := range msg.Parts {
		if msg.Parts[i].Type == "tool" {
			toolPart = &msg.Parts[i]
			break
		}
	}
	require.NotNil(t, toolPart, "must find at least one tool part")
	assert.NotEmpty(t, toolPart.Tool.Name, "tool name must be extracted from flat 'tool' string field")
	assert.NotEmpty(t, toolPart.Tool.CallID, "callID must be extracted")
	require.NotNil(t, toolPart.Tool.State, "tool state must be populated")
	assert.Contains(t, []string{"completed", "running", "pending", "error"}, string(toolPart.Tool.State.Status),
		"status must be a valid ToolState variant")
	_ = changedFiles
}

func TestContract_HistoryNestedTool_1_15_12(t *testing.T) {
	body := loadFixture(t, "history_1_15_12_nested_tool.json")
	msgs, _, downgraded, err := ParseHistoryStream(bytes.NewReader(body), "ws-contract")
	require.NoError(t, err, "nested tool history fixture must parse without error")
	assert.Equal(t, 0, downgraded, "no messages should be downgraded")
	require.NotEmpty(t, msgs, "fixture must have at least 1 message")

	// Find the tool part in the assistant message and verify real extraction.
	var toolPart *session.Part
	for i := range msgs {
		for j := range msgs[i].Parts {
			if msgs[i].Parts[j].Type == "tool" {
				toolPart = &msgs[i].Parts[j]
				break
			}
		}
		if toolPart != nil {
			break
		}
	}
	require.NotNil(t, toolPart, "must find at least one tool part")
	require.NotNil(t, toolPart.Tool, "Tool must be populated from nested object")
	assert.Equal(t, "bash", toolPart.Tool.Name, "tool name must be extracted from nested 'tool.name'")
	assert.Equal(t, "call_legacy_1", toolPart.Tool.CallID, "callID must be extracted from nested 'tool.callID'")
	require.NotNil(t, toolPart.Tool.State, "tool state must be extracted from nested 'tool.state'")
	assert.Equal(t, "completed", string(toolPart.Tool.State.Status),
		"status must be extracted from nested 'tool.state.status'")
}

func TestContract_SessionGet_1_18_10_AllFields(t *testing.T) {
	body := loadFixture(t, "session_get_1_18_10.json")
	s, err := ParseSessionWire(body, "ws-contract")
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.NotEmpty(t, s.ID)
	assert.NotEmpty(t, s.Title)
	require.NotNil(t, s.Model, "model must be extracted")
	assert.NotEmpty(t, s.Model.Provider, "provider must be extracted (1.18.10 wire shape)")
	assert.NotEmpty(t, s.Model.ID, "model ID must be extracted")
	require.NotNil(t, s.Cost, "cost/tokens must be extracted from tokens object")
	assert.Greater(t, s.Cost.InputTokens, int64(0), "input tokens must be > 0")
	assert.Greater(t, s.Cost.OutputTokens, int64(0), "output tokens must be > 0")
	assert.NotEmpty(t, s.Status, "status must be extracted")
	assert.Equal(t, session.StatusIdle, s.Status,
		"status must be 'idle' extracted from the fixture's status.type field")
}

func TestContract_SessionList_1_18_10_AllFields(t *testing.T) {
	body := loadFixture(t, "session_list_1_18_10.json")
	sessions, err := ParseSessionListWire(body, "ws-contract")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(sessions), 2, "fixture must have at least 2 sessions")

	for _, s := range sessions {
		assert.NotEmpty(t, s.ID, "every session must have an ID")
		assert.NotEmpty(t, s.Status, "every session must have a status extracted")
		assert.Contains(t, []session.Status{session.StatusIdle, session.StatusBusy}, s.Status,
			"status must be a valid known variant, not the 'unknown' default")
	}
}

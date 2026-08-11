// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func TestTranslateSSEEvent_SessionStatus(t *testing.T) {
	data := []byte(`{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"idle"}}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventSessionStatus, evt.Type)
	assert.Equal(t, "ses_1", evt.SessionID)
	assert.Equal(t, session.StatusIdle, evt.Status)
}

func TestTranslateSSEEvent_QuestionAsked(t *testing.T) {
	data := []byte(`{"type":"question.asked","properties":{"id":"que_1","sessionID":"ses_1","questions":[{"question":"Continue?","header":"Confirm","options":[{"label":"Yes","description":"proceed"},{"label":"No","description":"abort"}],"multiple":false,"custom":true}]}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventInputRequest, evt.Type)
	require.NotNil(t, evt.Input)
	assert.Equal(t, "que_1", evt.Input.ID)
	assert.Equal(t, session.InputQuestion, evt.Input.Kind)
	assert.Equal(t, "Continue?", evt.Input.Question)
	assert.Equal(t, "Confirm", evt.Input.Header)
	assert.False(t, evt.Input.Multiple)
	assert.True(t, evt.Input.Custom)
	require.Len(t, evt.Input.Options, 2)
	assert.Equal(t, "Yes", evt.Input.Options[0].Label)
	assert.Equal(t, "proceed", evt.Input.Options[0].Description)
}

func TestTranslateSSEEvent_PermissionAsked(t *testing.T) {
	data := []byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"ses_1","permission":"shell","patterns":["bash"],"always":["/workspace"]}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventInputRequest, evt.Type)
	require.NotNil(t, evt.Input)
	assert.Equal(t, "per_1", evt.Input.ID)
	assert.Equal(t, session.InputPermission, evt.Input.Kind)
	assert.Equal(t, "shell", evt.Input.Permission)
	assert.Contains(t, evt.Input.Patterns, "bash")
	assert.Contains(t, evt.Input.Always, "/workspace")
}

func TestTranslateSSEEvent_Error_StringShape(t *testing.T) {
	data := []byte(`{"type":"session.error","properties":{"sessionID":"ses_1","error":"something went wrong"}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventError, evt.Type)
	require.NotNil(t, evt.Error)
	assert.Equal(t, "something went wrong", evt.Error.Message)
}

func TestTranslateSSEEvent_Error_ObjectShape(t *testing.T) {
	data := []byte(`{"type":"session.error","properties":{"sessionID":"ses_1","error":{"message":"rate limited","code":"429"}}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventError, evt.Type)
	require.NotNil(t, evt.Error)
	assert.Equal(t, "rate limited", evt.Error.Message)
}

func TestTranslateSSEEvent_Delta(t *testing.T) {
	data := []byte(`{"type":"message.part.delta","properties":{"sessionID":"ses_1","messageID":"msg_1","partID":"p1","text":"hello"}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventPartDelta, evt.Type)
	assert.Equal(t, "msg_1", evt.MessageID)
	assert.Equal(t, "p1", evt.PartID)
	assert.Equal(t, "hello", evt.Delta)
}

func TestTranslateSSEEvent_UnknownType_Dropped(t *testing.T) {
	data := []byte(`{"type":"session.diff","properties":{"files":["/workspace/foo"]}}`)
	_, ok := translateSSEEvent(data)
	assert.False(t, ok, "unknown event types must be dropped")
}

func TestTranslateSSEEvent_MalformedJSON_Dropped(t *testing.T) {
	_, ok := translateSSEEvent([]byte(`not json`))
	assert.False(t, ok)
}

func TestTranslateSSEEvent_EmptyProperties(t *testing.T) {
	data := []byte(`{"type":"session.status","properties":{}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)
	assert.Equal(t, session.EventSessionStatus, evt.Type)
	assert.Empty(t, evt.SessionID)
}

func TestTranslateEventType_AllMappings(t *testing.T) {
	cases := []struct {
		in  string
		out session.EventType
	}{
		{"session.status", session.EventSessionStatus},
		{"session.updated", session.EventSessionUpdated},
		{"message.part.delta", session.EventPartDelta},
		{"step.started", session.EventPartStart},
		{"step.ended", session.EventPartEnd},
		{"text.started", session.EventPartStart},
		{"text.ended", session.EventPartEnd},
		{"question.asked", session.EventInputRequest},
		{"question.replied", session.EventInputResolved},
		{"question.rejected", session.EventInputResolved},
		{"permission.asked", session.EventInputRequest},
		{"permission.replied", session.EventInputResolved},
		{"session.error", session.EventError},
		{"session.diff", ""},         // unknown -> dropped
		{"server.heartbeat", ""},     // unknown -> dropped
		{"message.part.updated", ""}, // unknown -> dropped
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.out, translateEventType(c.in))
		})
	}
}

func TestTranslateSSEEvent_RoundTrip(t *testing.T) {
	// Verify a translated event round-trips through JSON.
	data := []byte(`{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"busy"}}}`)
	evt, ok := translateSSEEvent(data)
	require.True(t, ok)

	out, err := json.Marshal(evt)
	require.NoError(t, err)
	var roundTripped session.Event
	require.NoError(t, json.Unmarshal(out, &roundTripped))
	assert.Equal(t, evt.Type, roundTripped.Type)
	assert.Equal(t, evt.SessionID, roundTripped.SessionID)
	assert.Equal(t, evt.Status, roundTripped.Status)
}

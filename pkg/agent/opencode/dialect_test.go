// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDialect_SessionPaths(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{"SessionCreatePath", d.SessionCreatePath(), "/session"},
		{"SessionListPath", d.SessionListPath(), "/session"},
		{"SessionMessagePath", d.SessionMessagePath("ses_abc"), "/session/ses_abc/message"},
		{"SessionPromptAsyncPath", d.SessionPromptAsyncPath("ses_abc"), "/session/ses_abc/prompt_async"},
		{"SessionAbortPath", d.SessionAbortPath("ses_abc"), "/session/ses_abc/abort"},
		{"SessionGetPath", d.SessionGetPath("ses_abc"), "/session/ses_abc"},
		{"EventStreamPath", d.EventStreamPath(), "/event"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.got)
		})
	}
}

func TestDialect_InputRequestPaths(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{"QuestionListPath", d.QuestionListPath(), "/question"},
		{"QuestionReplyPath", d.QuestionReplyPath("que_e74d7e6db001ZI3VDSHthsee0g"), "/question/que_e74d7e6db001ZI3VDSHthsee0g/reply"},
		{"QuestionRejectPath", d.QuestionRejectPath("que_abc"), "/question/que_abc/reject"},
		{"PermissionListPath", d.PermissionListPath(), "/permission"},
		{"PermissionReplyPath", d.PermissionReplyPath("per_xyz"), "/permission/per_xyz/reply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.got)
		})
	}
}

func TestDialect_IsQuestionAsked(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"question.asked", true},
		{"question.replied", false},
		{"session.status", false},
		{"permission.asked", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsQuestionAsked(tt.eventType))
		})
	}
}

func TestDialect_IsQuestionResolved(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"question.replied", true},
		{"question.rejected", true},
		{"question.asked", false},
		{"session.status", false},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsQuestionResolved(tt.eventType))
		})
	}
}

func TestDialect_IsPermissionAsked(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"permission.asked", true},
		{"permission.replied", false},
		{"question.asked", false},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsPermissionAsked(tt.eventType))
		})
	}
}

func TestDialect_IsPermissionResolved(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		eventType string
		expected  bool
	}{
		{"permission.replied", true},
		{"permission.asked", false},
		{"question.replied", false},
	}
	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsPermissionResolved(tt.eventType))
		})
	}
}

func TestDialect_IsSessionIdle(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		name       string
		eventType  string
		properties string
		expected   bool
	}{
		{
			"idle event",
			"session.status",
			`{"sessionID":"ses_x","status":{"type":"idle"}}`,
			true,
		},
		{
			"busy event",
			"session.status",
			`{"sessionID":"ses_x","status":{"type":"busy"}}`,
			false,
		},
		{
			"wrong event type",
			"question.asked",
			`{"sessionID":"ses_x","status":{"type":"idle"}}`,
			false,
		},
		{
			"malformed properties",
			"session.status",
			`{invalid}`,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsSessionIdle(tt.eventType, json.RawMessage(tt.properties)))
		})
	}
}

func TestDialect_IsSessionBusy(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		name       string
		eventType  string
		properties string
		expected   bool
	}{
		{
			"busy event",
			"session.status",
			`{"sessionID":"ses_x","status":{"type":"busy"}}`,
			true,
		},
		{
			"idle event",
			"session.status",
			`{"sessionID":"ses_x","status":{"type":"idle"}}`,
			false,
		},
		{
			"wrong event type",
			"permission.asked",
			`{"sessionID":"ses_x","status":{"type":"busy"}}`,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, d.IsSessionBusy(tt.eventType, json.RawMessage(tt.properties)))
		})
	}
}

func TestDialect_ParseQuestionRequest_RealPayload(t *testing.T) {
	d := &Dialect{}

	// Real payload from worklog 0069 live capture
	properties := json.RawMessage(`{
		"id": "que_e74d7e6db001ZI3VDSHthsee0g",
		"sessionID": "ses_18b28260affeoxXrX1iwPH8wFg",
		"questions": [
			{
				"question": "What programming language do you want to use for your new project?",
				"header": "Choose language",
				"options": [
					{"label": "Python", "description": "Great for data science, ML, automation, and web backends"},
					{"label": "Go", "description": "Excellent for CLI tools, APIs, and concurrent systems"},
					{"label": "Rust", "description": "Ideal for performance-critical systems, CLIs, and WebAssembly"},
					{"label": "TypeScript", "description": "Best for web apps, full-stack, and Node.js projects"}
				]
			}
		],
		"tool": {
			"messageID": "msg_e74d7da37001Nw4A59Ndzegm3A",
			"callID": "call_00_L2Vxenr4keDzvhpYXz9J6233"
		}
	}`)

	req, err := d.ParseQuestionRequest("question.asked", properties)
	require.NoError(t, err)
	require.NotNil(t, req)

	assert.Equal(t, "que_e74d7e6db001ZI3VDSHthsee0g", req.ID)
	assert.Equal(t, "ses_18b28260affeoxXrX1iwPH8wFg", req.SessionID)
	require.Len(t, req.Questions, 1)

	q := req.Questions[0]
	assert.Equal(t, "What programming language do you want to use for your new project?", q.Question)
	assert.Equal(t, "Choose language", q.Header)
	assert.False(t, q.Multiple)
	assert.True(t, q.Custom) // defaults to true when absent
	require.Len(t, q.Options, 4)
	assert.Equal(t, "Python", q.Options[0].Label)
	assert.Equal(t, "TypeScript", q.Options[3].Label)

	require.NotNil(t, req.Tool)
	assert.Equal(t, "msg_e74d7da37001Nw4A59Ndzegm3A", req.Tool.MessageID)
	assert.Equal(t, "call_00_L2Vxenr4keDzvhpYXz9J6233", req.Tool.CallID)
}

func TestDialect_ParseQuestionRequest_CustomExplicitFalse(t *testing.T) {
	d := &Dialect{}

	properties := json.RawMessage(`{
		"id": "que_abc",
		"sessionID": "ses_xyz",
		"questions": [{"question": "Pick one", "header": "Choice", "options": [{"label": "A", "description": "Option A"}], "custom": false}]
	}`)

	req, err := d.ParseQuestionRequest("question.asked", properties)
	require.NoError(t, err)
	assert.False(t, req.Questions[0].Custom)
}

func TestDialect_ParseQuestionRequest_WrongEventType(t *testing.T) {
	d := &Dialect{}

	_, err := d.ParseQuestionRequest("session.status", json.RawMessage(`{}`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a question.asked event")
}

func TestDialect_ParseQuestionRequest_MalformedJSON(t *testing.T) {
	d := &Dialect{}

	_, err := d.ParseQuestionRequest("question.asked", json.RawMessage(`{invalid`))
	assert.Error(t, err)
}

func TestDialect_ParseQuestionRequest_MissingID(t *testing.T) {
	d := &Dialect{}

	_, err := d.ParseQuestionRequest("question.asked", json.RawMessage(`{"sessionID":"ses_x","questions":[]}`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing id or sessionID")
}

func TestDialect_ParsePermissionRequest_RealPayload(t *testing.T) {
	d := &Dialect{}

	properties := json.RawMessage(`{
		"id": "per_1748012345000_abc",
		"sessionID": "ses_18b28260affeoxXrX1iwPH8wFg",
		"permission": "shell",
		"patterns": ["rm -rf /tmp/test"],
		"metadata": {"command": "rm -rf /tmp/test"},
		"always": ["/tmp/*"],
		"tool": {
			"messageID": "msg_abc",
			"callID": "call_xyz"
		}
	}`)

	req, err := d.ParsePermissionRequest("permission.asked", properties)
	require.NoError(t, err)
	require.NotNil(t, req)

	assert.Equal(t, "per_1748012345000_abc", req.ID)
	assert.Equal(t, "ses_18b28260affeoxXrX1iwPH8wFg", req.SessionID)
	assert.Equal(t, "shell", req.Permission)
	assert.Equal(t, []string{"rm -rf /tmp/test"}, req.Patterns)
	assert.Equal(t, map[string]interface{}{"command": "rm -rf /tmp/test"}, req.Metadata)
	assert.Equal(t, []string{"/tmp/*"}, req.Always)
	require.NotNil(t, req.Tool)
	assert.Equal(t, "msg_abc", req.Tool.MessageID)
	assert.Equal(t, "call_xyz", req.Tool.CallID)
}

func TestDialect_ParsePermissionRequest_WrongEventType(t *testing.T) {
	d := &Dialect{}

	_, err := d.ParsePermissionRequest("question.asked", json.RawMessage(`{}`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a permission.asked event")
}

func TestDialect_ParsePermissionRequest_MissingID(t *testing.T) {
	d := &Dialect{}

	_, err := d.ParsePermissionRequest("permission.asked", json.RawMessage(`{"sessionID":"ses_x","permission":"shell"}`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing id or sessionID")
}

func TestDialect_ParseSessionStatus(t *testing.T) {
	d := &Dialect{}

	tests := []struct {
		name       string
		properties string
		wantSID    string
		wantStatus string
		wantErr    bool
	}{
		{
			"idle",
			`{"sessionID":"ses_abc","status":{"type":"idle"}}`,
			"ses_abc", "idle", false,
		},
		{
			"busy",
			`{"sessionID":"ses_xyz","status":{"type":"busy"}}`,
			"ses_xyz", "busy", false,
		},
		{
			"missing sessionID",
			`{"status":{"type":"idle"}}`,
			"", "", true,
		},
		{
			"missing status type",
			`{"sessionID":"ses_abc","status":{}}`,
			"", "", true,
		},
		{
			"malformed JSON",
			`{invalid`,
			"", "", true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid, status, err := d.ParseSessionStatus(json.RawMessage(tt.properties))
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantSID, sid)
				assert.Equal(t, tt.wantStatus, status)
			}
		})
	}
}

func TestDialect_ParseQuestionRequest_MultipleQuestions(t *testing.T) {
	d := &Dialect{}

	properties := json.RawMessage(`{
		"id": "que_multi",
		"sessionID": "ses_123",
		"questions": [
			{"question": "Q1?", "header": "H1", "options": [{"label": "A", "description": "a"}], "multiple": true},
			{"question": "Q2?", "header": "H2", "options": [{"label": "B", "description": "b"}]}
		]
	}`)

	req, err := d.ParseQuestionRequest("question.asked", properties)
	require.NoError(t, err)
	require.Len(t, req.Questions, 2)
	assert.True(t, req.Questions[0].Multiple)
	assert.False(t, req.Questions[1].Multiple)
	assert.True(t, req.Questions[0].Custom) // default
	assert.True(t, req.Questions[1].Custom) // default
}

func TestDialect_ParsePermissionRequest_NoTool(t *testing.T) {
	d := &Dialect{}

	properties := json.RawMessage(`{
		"id": "per_notool",
		"sessionID": "ses_abc",
		"permission": "edit",
		"patterns": ["/workspace/file.go"]
	}`)

	req, err := d.ParsePermissionRequest("permission.asked", properties)
	require.NoError(t, err)
	assert.Nil(t, req.Tool)
	assert.Equal(t, "edit", req.Permission)
}

// 4a-2 (#1302): the seam's kind discriminator — the prefix convention
// the adapter's routing consumes.
func TestDialect_KindByPrefix(t *testing.T) {
	d := &Dialect{}
	assert.Equal(t, "question", d.KindByPrefix("que_abc123"))
	assert.Equal(t, "question", d.KindByPrefix("que_"))
	assert.Equal(t, "permission", d.KindByPrefix("per_abc_123"))
	assert.Equal(t, "permission", d.KindByPrefix("per_"))
	assert.Equal(t, "", d.KindByPrefix("req.plain-1"), "unprefixed ids are agent-agnostic — no kind")
	assert.Equal(t, "", d.KindByPrefix(""))
}

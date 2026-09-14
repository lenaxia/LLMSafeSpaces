// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

// QuestionOption is a single selectable choice within a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// QuestionInfo is a single question with its options.
type QuestionInfo struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple"`
	Custom   bool             `json:"custom"`
}

// QuestionRequest is the normalized, agent-agnostic representation of a pending question.
//
// RootSessionID is the top-level session in the parent chain. For top-level
// sessions it equals SessionID. For subtask/subagent sessions (e.g. opencode's
// `task` tool spawning child sessions) it is the ancestor session that the user
// is actually viewing in the chat UI. The frontend uses this to bubble subtask
// prompts up to the user — without it, prompts would be silently dropped
// because the subtask's SessionID does not match the URL session.
type QuestionRequest struct {
	ID            string         `json:"id"`
	SessionID     string         `json:"session_id"`
	RootSessionID string         `json:"root_session_id,omitempty"`
	Questions     []QuestionInfo `json:"questions"`
	Tool          *ToolRef       `json:"tool,omitempty"`
}

// PermissionRequest is the normalized, agent-agnostic representation of a pending permission.
//
// RootSessionID — see [QuestionRequest] for semantics.
type PermissionRequest struct {
	ID            string                 `json:"id"`
	SessionID     string                 `json:"session_id"`
	RootSessionID string                 `json:"root_session_id,omitempty"`
	Permission    string                 `json:"permission"`
	Patterns      []string               `json:"patterns"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	Always        []string               `json:"always,omitempty"`
	Tool          *ToolRef               `json:"tool,omitempty"`
}

// ToolRef identifies the tool call that triggered the input request.
type ToolRef struct {
	MessageID string `json:"message_id"`
	CallID    string `json:"call_id"`
}

// InputResolution contains the resolution data for a question or permission.
type InputResolution struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Reply     string `json:"reply,omitempty"`
}

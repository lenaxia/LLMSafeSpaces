// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "time"

// MessageType discriminates a Message. Agent/model switches and compaction are
// transcript entries (not side-band config) so the timeline stays coherent
// after a switch (design 0049 §4.5).
type MessageType string

const (
	MessageUser        MessageType = "user"
	MessageAssistant   MessageType = "assistant"
	MessageShell       MessageType = "shell"
	MessageAgentSwitch MessageType = "agent_switch"
	MessageModelSwitch MessageType = "model_switch"
	MessageCompaction  MessageType = "compaction"
	MessageSystem      MessageType = "system"
)

// Message is one entry in a session transcript. It is a flat discriminated
// struct: Type selects which payload fields are meaningful, and the rest are
// omitted from the wire form. Constructors are the documented write path so
// the Type<->field pairing is encoded in one place.
type Message struct {
	ID        string      `json:"id"`
	SessionID string      `json:"sessionId,omitempty"`
	Type      MessageType `json:"type"`
	CreatedAt *time.Time  `json:"createdAt,omitempty"`

	Parts []Part    `json:"parts,omitempty"`
	Model *ModelRef `json:"model,omitempty"`
	Cost  *Cost     `json:"cost,omitempty"`

	Text string `json:"text,omitempty"`

	Command  string `json:"command,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`

	FromAgent string `json:"fromAgent,omitempty"`
	ToAgent   string `json:"toAgent,omitempty"`

	FromModel *ModelRef `json:"fromModel,omitempty"`
	ToModel   *ModelRef `json:"toModel,omitempty"`

	Error *Error `json:"error,omitempty"`
}

func newMessage(id string, t MessageType, createdAt time.Time) Message {
	return Message{ID: id, Type: t, CreatedAt: &createdAt}
}

func UserMessage(id, text string, createdAt time.Time) Message {
	m := newMessage(id, MessageUser, createdAt)
	m.Text = text
	return m
}

func AssistantMessage(id string, parts []Part, createdAt time.Time) Message {
	m := newMessage(id, MessageAssistant, createdAt)
	m.Parts = parts
	return m
}

func ShellMessage(id, command string, exitCode *int, createdAt time.Time) Message {
	m := newMessage(id, MessageShell, createdAt)
	m.Command = command
	m.ExitCode = exitCode
	return m
}

func AgentSwitchMessage(id, fromAgent, toAgent string, createdAt time.Time) Message {
	m := newMessage(id, MessageAgentSwitch, createdAt)
	m.FromAgent = fromAgent
	m.ToAgent = toAgent
	return m
}

func ModelSwitchMessage(id string, fromModel, toModel *ModelRef, createdAt time.Time) Message {
	m := newMessage(id, MessageModelSwitch, createdAt)
	m.FromModel = fromModel
	m.ToModel = toModel
	return m
}

func CompactionMessage(id, text string, createdAt time.Time) Message {
	m := newMessage(id, MessageCompaction, createdAt)
	m.Text = text
	return m
}

func SystemMessage(id, text string, createdAt *time.Time) Message {
	m := Message{ID: id, Type: MessageSystem}
	if createdAt != nil {
		m.CreatedAt = createdAt
	}
	m.Text = text
	return m
}

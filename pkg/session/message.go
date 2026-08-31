// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "time"

// Message and MessageType are generated into contract_gen.go from the
// frozen ABI schema. The constructors below are the documented write path
// (ADR 0056 T3: "thin wrappers where Go ergonomics demand constructors"):
// they encode the Type<->payload-field pairing in one place.

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

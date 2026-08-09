// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"time"
)

// EventType discriminates a streaming Event. The values are pinned in
// event_test.go's TestEventTypeCountMatchesExplicitList.
type EventType string

const (
	EventSessionStatus  EventType = "session.status"
	EventSessionUpdated EventType = "session.updated"
	EventMessageStart   EventType = "message.start"
	EventMessageEnd     EventType = "message.end"
	EventPartStart      EventType = "part.start"
	EventPartDelta      EventType = "part.delta"
	EventPartEnd        EventType = "part.end"
	EventInputRequest   EventType = "input.request"
	EventInputResolved  EventType = "input.resolved"
	EventError          EventType = "error"
)

// Event is one item on a session's streaming event stream. Type selects which
// payload fields are meaningful; the rest are omitted.
type Event struct {
	Type      EventType     `json:"type"`
	Timestamp time.Time     `json:"timestamp"`
	SessionID string        `json:"sessionId,omitempty"`
	MessageID string        `json:"messageId,omitempty"`
	PartID    string        `json:"partId,omitempty"`
	Status    Status        `json:"status,omitempty"`
	Session   *Session      `json:"session,omitempty"`
	Message   *Message      `json:"message,omitempty"`
	Part      *Part         `json:"part,omitempty"`
	Delta     string        `json:"delta,omitempty"`
	Input     *InputRequest `json:"input,omitempty"`
	Error     *Error        `json:"error,omitempty"`
}

// InputKind unifies questions and permissions: both are "the agent needs a
// human" (design 0049 §4.5).
type InputKind string

const (
	InputQuestion   InputKind = "question"
	InputPermission InputKind = "permission"
)

// InputOption is one selectable choice within a question InputRequest.
type InputOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ToolRef identifies the tool call that triggered an InputRequest, if any.
type ToolRef struct {
	MessageID string `json:"messageId,omitempty"`
	CallID    string `json:"callId,omitempty"`
}

// InputRequest is the unified pending-input shape. Question-specific fields
// apply when Kind == InputQuestion; permission-specific fields when
// Kind == InputPermission. Metadata values are raw JSON because permission
// metadata is open-ended extension data, not a known shape.
type InputRequest struct {
	ID            string                     `json:"id"`
	SessionID     string                     `json:"sessionId,omitempty"`
	RootSessionID string                     `json:"rootSessionId,omitempty"`
	Kind          InputKind                  `json:"kind"`
	Question      string                     `json:"question,omitempty"`
	Header        string                     `json:"header,omitempty"`
	Options       []InputOption              `json:"options,omitempty"`
	Multiple      bool                       `json:"multiple,omitempty"`
	Custom        bool                       `json:"custom,omitempty"`
	Permission    string                     `json:"permission,omitempty"`
	Patterns      []string                   `json:"patterns,omitempty"`
	Always        []string                   `json:"always,omitempty"`
	Metadata      map[string]json.RawMessage `json:"metadata,omitempty"`
	Tool          *ToolRef                   `json:"tool,omitempty"`
}

// Admission is a delivery mode on SendOpts (design 0049 §4.4). The zero value
// means an immediate/default send; steer injects at the next safe boundary
// without aborting in-flight tools; queue promotes when the agent would idle.
type Admission string

const (
	AdmissionSteer Admission = "steer"
	AdmissionQueue Admission = "queue"
)

// SendOpts parameterize a message send, including steering admission.
type SendOpts struct {
	Model     *ModelRef `json:"model,omitempty"`
	Admission Admission `json:"admission,omitempty"`
}

// Error is the payload of an error Event (and an assistant Message's Error
// field). It is deliberately NOT a PartType: the part union is capped at 5
// forever (design 0049 §4.1 rule 1); errors flow through the error Event.
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

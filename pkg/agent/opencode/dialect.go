// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Dialect implements agent.Dialect for the opencode agent runtime.
type Dialect struct{}

var _ agent.Dialect = (*Dialect)(nil)

// --- Session route paths ---

func (d *Dialect) SessionCreatePath() string { return "/session" }
func (d *Dialect) SessionListPath() string   { return "/session" }
func (d *Dialect) SessionMessagePath(sessionID string) string {
	return "/session/" + sessionID + "/message"
}
func (d *Dialect) SessionPromptAsyncPath(sessionID string) string {
	return "/session/" + sessionID + "/prompt_async"
}
func (d *Dialect) SessionAbortPath(sessionID string) string {
	return "/session/" + sessionID + "/abort"
}
func (d *Dialect) SessionGetPath(sessionID string) string { return "/session/" + sessionID }
func (d *Dialect) EventStreamPath() string                { return "/event" }

// --- Input request route paths ---

func (d *Dialect) QuestionListPath() string { return "/question" }
func (d *Dialect) QuestionReplyPath(requestID string) string {
	return "/question/" + requestID + "/reply"
}
func (d *Dialect) QuestionRejectPath(requestID string) string {
	return "/question/" + requestID + "/reject"
}
func (d *Dialect) PermissionListPath() string { return "/permission" }
func (d *Dialect) PermissionReplyPath(requestID string) string {
	return "/permission/" + requestID + "/reply"
}

// --- SSE event classification ---

func (d *Dialect) IsQuestionAsked(eventType string) bool { return eventType == "question.asked" }
func (d *Dialect) IsQuestionResolved(eventType string) bool {
	return eventType == "question.replied" || eventType == "question.rejected"
}
func (d *Dialect) IsPermissionAsked(eventType string) bool { return eventType == "permission.asked" }
func (d *Dialect) IsPermissionResolved(eventType string) bool {
	return eventType == "permission.replied"
}

func (d *Dialect) IsSessionIdle(eventType string, properties json.RawMessage) bool {
	if eventType != "session.status" {
		return false
	}
	_, status, err := d.ParseSessionStatus(properties)
	return err == nil && status == "idle"
}

func (d *Dialect) IsSessionBusy(eventType string, properties json.RawMessage) bool {
	if eventType != "session.status" {
		return false
	}
	_, status, err := d.ParseSessionStatus(properties)
	return err == nil && status == "busy"
}

// --- Event parsing ---

// opencode event shapes (from live capture, worklog 0069):
//
// question.asked properties:
//   {"id":"que_...","sessionID":"ses_...","questions":[...],"tool":{...}}
//
// permission.asked properties:
//   {"id":"per_...","sessionID":"ses_...","permission":"shell","patterns":[...],"metadata":{...},"always":[...]}
//
// session.status properties:
//   {"sessionID":"ses_...","status":{"type":"idle"|"busy"}}

type ocQuestionEvent struct {
	ID        string           `json:"id"`
	SessionID string           `json:"sessionID"`
	Questions []ocQuestionInfo `json:"questions"`
	Tool      *ocToolRef       `json:"tool,omitempty"`
}

type ocQuestionInfo struct {
	Question string             `json:"question"`
	Header   string             `json:"header"`
	Options  []ocQuestionOption `json:"options"`
	Multiple bool               `json:"multiple"`
	Custom   *bool              `json:"custom,omitempty"`
}

type ocQuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type ocToolRef struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type ocPermissionEvent struct {
	ID         string                 `json:"id"`
	SessionID  string                 `json:"sessionID"`
	Permission string                 `json:"permission"`
	Patterns   []string               `json:"patterns"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
	Always     []string               `json:"always,omitempty"`
	Tool       *ocToolRef             `json:"tool,omitempty"`
}

type ocSessionStatusEvent struct {
	SessionID string         `json:"sessionID"`
	Status    ocStatusDetail `json:"status"`
}

type ocStatusDetail struct {
	Type string `json:"type"`
}

// ParseQuestionListItem parses one entry of opencode's GET /question list
// into the normalized QuestionRequest (the list entries share the
// question.asked properties shape). Used by the API adapter's ListPending
// and by agentd's sessionstate store reader (US-69.3).
func (d *Dialect) ParseQuestionListItem(raw json.RawMessage) (*agent.QuestionRequest, error) {
	return d.ParseQuestionRequest("question.asked", raw)
}

// ParsePermissionListItem parses one entry of GET /permission (shares the
// permission.asked properties shape).
func (d *Dialect) ParsePermissionListItem(raw json.RawMessage) (*agent.PermissionRequest, error) {
	return d.ParsePermissionRequest("permission.asked", raw)
}

func (d *Dialect) ParseQuestionRequest(eventType string, properties json.RawMessage) (*agent.QuestionRequest, error) {
	if !d.IsQuestionAsked(eventType) {
		return nil, fmt.Errorf("not a question.asked event: %s", eventType)
	}
	var oc ocQuestionEvent
	if err := json.Unmarshal(properties, &oc); err != nil {
		return nil, fmt.Errorf("unmarshal question event: %w", err)
	}
	if oc.ID == "" || oc.SessionID == "" {
		return nil, fmt.Errorf("question event missing id or sessionID")
	}

	req := &agent.QuestionRequest{
		ID:        oc.ID,
		SessionID: oc.SessionID,
		Questions: make([]agent.QuestionInfo, len(oc.Questions)),
	}
	for i, q := range oc.Questions {
		custom := true
		if q.Custom != nil {
			custom = *q.Custom
		}
		opts := make([]agent.QuestionOption, len(q.Options))
		for j, o := range q.Options {
			opts[j] = agent.QuestionOption{Label: o.Label, Description: o.Description}
		}
		req.Questions[i] = agent.QuestionInfo{
			Question: q.Question,
			Header:   q.Header,
			Options:  opts,
			Multiple: q.Multiple,
			Custom:   custom,
		}
	}
	if oc.Tool != nil {
		req.Tool = &agent.ToolRef{MessageID: oc.Tool.MessageID, CallID: oc.Tool.CallID}
	}
	return req, nil
}

func (d *Dialect) ParsePermissionRequest(eventType string, properties json.RawMessage) (*agent.PermissionRequest, error) {
	if !d.IsPermissionAsked(eventType) {
		return nil, fmt.Errorf("not a permission.asked event: %s", eventType)
	}
	var oc ocPermissionEvent
	if err := json.Unmarshal(properties, &oc); err != nil {
		return nil, fmt.Errorf("unmarshal permission event: %w", err)
	}
	if oc.ID == "" || oc.SessionID == "" {
		return nil, fmt.Errorf("permission event missing id or sessionID")
	}

	req := &agent.PermissionRequest{
		ID:         oc.ID,
		SessionID:  oc.SessionID,
		Permission: oc.Permission,
		Patterns:   oc.Patterns,
		Metadata:   oc.Metadata,
		Always:     oc.Always,
	}
	if oc.Tool != nil {
		req.Tool = &agent.ToolRef{MessageID: oc.Tool.MessageID, CallID: oc.Tool.CallID}
	}
	return req, nil
}

func (d *Dialect) ParseSessionStatus(properties json.RawMessage) (string, string, error) {
	var oc ocSessionStatusEvent
	if err := json.Unmarshal(properties, &oc); err != nil {
		return "", "", fmt.Errorf("unmarshal session status: %w", err)
	}
	if oc.SessionID == "" || oc.Status.Type == "" {
		return "", "", fmt.Errorf("session status missing sessionID or status.type")
	}
	return oc.SessionID, oc.Status.Type, nil
}

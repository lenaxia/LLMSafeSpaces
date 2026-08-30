// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// translate_abi.go — the sole opencode-dialect → harness-ABI contract
// translation point (design 0055 M1, Epic 69 US-69.3). Everything the
// sessionstate projection consumes crosses this table; the dialect stays
// pod-internal (I11). Rules encoded here:
//
//   - Store IDs are preserved verbatim (I12 stitch rule): message/part/
//     session identifiers pass through unchanged so history ∪ snapshot ∪
//     events reconcile by entity ID, never timestamps.
//   - Unknown event types pass through the contract's Custom valve (a
//     PART_START carrying a Custom part whose Kind is
//     "opencode.event.<type>" and whose Data is the original payload) —
//     the 14k-unknown lesson relocated, not repeated. Known-but-non-session
//     types (server.*, plugin.*, ...) are deliberately ignored: ok=false
//     without the valve.
//   - Busy/streaming derives from step boundaries and status events; a
//     failed step clears busy.
//   - Billing-relevant fields (tokens/cost on step.ended, message.updated)
//     map to contract Message.Cost (display-only; Epic 33 consumption).
//   - opencode's ascending event IDs are cross-checks only (A6) — carried
//     nowhere in the contract.
//
// Golden coverage: TestTranslateABI_GoldenFixtures replays the pinned live
// captures in testdata/ (REFRESH.md provenance) — the bump-gate pattern.

import (
	"encoding/json"
	"fmt"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ABITranslator translates raw opencode SSE payloads into ABI contract
// events. It satisfies the sessionstate.EventParser seam structurally
// (cmd/workspace-agentd/sessionstate imports nothing from here; agentd
// wiring injects it). Now is injectable for deterministic goldens.
type ABITranslator struct {
	Now func() time.Time
}

// Parse implements the EventParser seam: ok=false means "nothing
// projectable" (ignored or empty), err signals a payload that CLAIMED to be
// projectable but failed to decode (wire drift — logged by the authority,
// never fatal).
func (t ABITranslator) Parse(raw []byte) (*abiv1.Event, bool, error) {
	var env struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, nil
	}
	evt := &abiv1.Event{Timestamp: timestamppb.New(t.now())}
	d := &Dialect{}

	switch env.Type {
	case "session.status":
		sid, status, err := d.ParseSessionStatus(env.Properties)
		if err != nil {
			return nil, true, fmt.Errorf("session.status: %w", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_SESSION_STATUS
		evt.SessionId = sid
		evt.Status = mapSessionStatus(status)
		return evt, evt.SessionId != "", nil

	case "session.idle":
		var p struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil || p.SessionID == "" {
			return nil, true, fmt.Errorf("session.idle: %v", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_SESSION_STATUS
		evt.SessionId = p.SessionID
		evt.Status = abiv1.SessionStatus_SESSION_STATUS_IDLE
		return evt, true, nil

	case "session.created", "session.updated":
		var p struct {
			SessionID string          `json:"sessionID"`
			Info      json.RawMessage `json:"info"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil {
			return nil, true, fmt.Errorf("%s: %w", env.Type, err)
		}
		sess, err := translateSessionABI(p.Info)
		if err != nil {
			return nil, true, fmt.Errorf("%s info: %w", env.Type, err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_SESSION_UPDATED
		evt.SessionId = firstNonEmpty(p.SessionID, sess.GetId())
		evt.Session = sess
		return evt, sess != nil, nil

	case "session.error":
		var p struct {
			SessionID string          `json:"sessionID"`
			Error     json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(env.Properties, &p)
		evt.Type = abiv1.EventType_EVENT_TYPE_ERROR
		evt.SessionId = p.SessionID
		evt.Error = translateErrorPayload(p.Error)
		return evt, evt.Error != nil, nil

	case "session.diff":
		return translateSessionDiff(env, evt)

	case "message.created", "message.updated":
		var p struct {
			SessionID string          `json:"sessionID"`
			Info      json.RawMessage `json:"info"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil {
			return nil, true, fmt.Errorf("%s: %w", env.Type, err)
		}
		msg, err := translateMessageABI(p.Info)
		if err != nil {
			return nil, true, fmt.Errorf("%s info: %w", env.Type, err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_MESSAGE_START
		if env.Type == "message.updated" && msg.GetCost() != nil {
			// A cost-bearing update is the billing-completion signal for
			// the message; both START and END carry the full store truth
			// (upsert semantics — the projection stitches by ID).
			evt.Type = abiv1.EventType_EVENT_TYPE_MESSAGE_END
		}
		evt.SessionId = firstNonEmpty(p.SessionID, msg.GetSessionId())
		evt.Message = msg
		return evt, msg != nil, nil

	case "message.part.updated":
		var p struct {
			SessionID string          `json:"sessionID"`
			Part      json.RawMessage `json:"part"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil {
			return nil, true, fmt.Errorf("message.part.updated: %w", err)
		}
		part, err := translatePartABI(p.Part)
		if err != nil {
			return nil, true, fmt.Errorf("message.part.updated part: %w", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_END
		evt.SessionId = p.SessionID
		evt.Part = part
		bindPartIDs(evt, p.Part)
		return evt, part != nil, nil

	case "message.part.delta":
		var p struct {
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
			PartID    string `json:"partID"`
			Delta     string `json:"delta"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil {
			return nil, true, fmt.Errorf("message.part.delta: %w", err)
		}
		delta := p.Delta
		if delta == "" {
			delta = p.Text
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_DELTA
		evt.SessionId = p.SessionID
		evt.MessageId = p.MessageID
		evt.PartId = p.PartID
		evt.Delta = delta
		return evt, evt.PartId != "" || evt.Delta != "", nil

	case "session.next.prompt.admitted":
		var p struct {
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
			Prompt    struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil {
			return nil, true, fmt.Errorf("session.next.prompt.admitted: %w", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_MESSAGE_START
		evt.SessionId = p.SessionID
		evt.MessageId = p.MessageID
		evt.Message = &abiv1.Message{Id: p.MessageID, SessionId: p.SessionID, Type: abiv1.MessageType_MESSAGE_TYPE_USER, Text: p.Prompt.Text}
		return evt, p.SessionID != "" || p.MessageID != "", nil

	case "session.next.prompted":
		var p struct {
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil || p.SessionID == "" {
			return nil, true, fmt.Errorf("session.next.prompted: %v", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_SESSION_STATUS
		evt.SessionId = p.SessionID
		evt.MessageId = p.MessageID
		evt.Status = abiv1.SessionStatus_SESSION_STATUS_BUSY
		return evt, true, nil

	case "session.next.step.started":
		var p struct {
			SessionID          string `json:"sessionID"`
			AssistantMessageID string `json:"assistantMessageID"`
			Model              *struct {
				ID         string `json:"id"`
				ModelID    string `json:"modelID"`
				ProviderID string `json:"providerID"`
			} `json:"model"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil || p.SessionID == "" {
			return nil, true, fmt.Errorf("session.next.step.started: %v", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_MESSAGE_START
		evt.SessionId = p.SessionID
		evt.MessageId = p.AssistantMessageID
		evt.Message = &abiv1.Message{
			Id: p.AssistantMessageID, SessionId: p.SessionID,
			Type:  abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
			Model: translateStepModel(p.Model),
		}
		return evt, true, nil

	case "session.next.step.ended":
		var p struct {
			SessionID          string  `json:"sessionID"`
			AssistantMessageID string  `json:"assistantMessageID"`
			Tokens             *tokens `json:"tokens"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil || p.SessionID == "" {
			return nil, true, fmt.Errorf("session.next.step.ended: %v", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_MESSAGE_END
		evt.SessionId = p.SessionID
		evt.MessageId = p.AssistantMessageID
		msg := &abiv1.Message{Id: p.AssistantMessageID, SessionId: p.SessionID, Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT}
		if p.Tokens != nil {
			msg.Cost = p.Tokens.cost()
		}
		evt.Message = msg
		return evt, true, nil

	case "session.next.step.failed":
		var p struct {
			SessionID          string          `json:"sessionID"`
			AssistantMessageID string          `json:"assistantMessageID"`
			Error              json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(env.Properties, &p); err != nil || p.SessionID == "" {
			return nil, true, fmt.Errorf("session.next.step.failed: %v", err)
		}
		evt.Type = abiv1.EventType_EVENT_TYPE_ERROR
		evt.SessionId = p.SessionID
		evt.MessageId = p.AssistantMessageID
		evt.Error = translateErrorPayload(p.Error)
		if evt.Error == nil {
			evt.Error = &abiv1.Error{Code: "step.failed", Message: "step failed"}
		}
		return evt, true, nil

	case "session.next.text.started", "session.next.text.delta", "session.next.text.ended":
		return translateNextText(env, evt)

	default:
		if d.IsQuestionAsked(env.Type) {
			return translateQuestionAsked(d, env, evt)
		}
		if d.IsQuestionResolved(env.Type) {
			return translateInputResolved(env, evt, "id")
		}
		if d.IsPermissionAsked(env.Type) {
			return translatePermissionAsked(d, env, evt)
		}
		if d.IsPermissionResolved(env.Type) {
			return translateInputResolved(env, evt, "id")
		}
		if wire.IsKnownEventType(env.Type) {
			// Known taxonomy, non-session surface (server.*, plugin.*,
			// catalog.*, file.*, reference.*, integration.*) — deliberately
			// ignored by the session projection.
			return nil, false, nil
		}
		// Unknown type — the Custom valve (nothing silently dropped).
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_START
		var ids struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(env.Properties, &ids)
		evt.SessionId = ids.SessionID
		evt.Part = &abiv1.Part{
			Type:    abiv1.PartType_PART_TYPE_CUSTOM,
			Payload: &abiv1.Part_Custom{Custom: &abiv1.CustomPart{Kind: "opencode.event." + env.Type, Data: env.Properties}},
		}
		return evt, true, nil
	}
}

func (t ABITranslator) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

// --- shared shape helpers --------------------------------------------------

type tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     *struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

func (t *tokens) cost() *abiv1.Cost {
	if t == nil {
		return nil
	}
	c := &abiv1.Cost{InputTokens: t.Input, OutputTokens: t.Output, ReasoningTokens: t.Reasoning}
	if t.Cache != nil {
		c.CacheReadTokens = t.Cache.Read
		c.CacheWriteTokens = t.Cache.Write
	}
	c.TotalTokens = c.InputTokens + c.OutputTokens + c.ReasoningTokens
	return c
}

// costValue accepts opencode's two cost shapes: a bare number (live SSE
// captures) and {total: n} — the golden fixtures proved both assumptions
// necessary (Rule 7: fixture is truth).
type costValue struct {
	Raw json.RawMessage
}

func (c *costValue) UnmarshalJSON(b []byte) (err error) {
	c.Raw = append(c.Raw[:0], b...)
	return nil
}

func (c *costValue) usd() (float64, bool) {
	if c == nil || len(c.Raw) == 0 {
		return 0, false
	}
	var n float64
	if json.Unmarshal(c.Raw, &n) == nil {
		return n, true
	}
	var obj struct {
		Total float64 `json:"total"`
	}
	if json.Unmarshal(c.Raw, &obj) == nil {
		return obj.Total, true
	}
	return 0, false
}

type sessionInfo struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Cost   *costValue `json:"cost"`
	Tokens *tokens    `json:"tokens"`
	Time   *struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Directory string `json:"directory"`
}

func translateSessionABI(info json.RawMessage) (*abiv1.Session, error) {
	if len(info) == 0 {
		return nil, nil
	}
	var si sessionInfo
	if err := json.Unmarshal(info, &si); err != nil {
		return nil, err
	}
	if si.ID == "" {
		return nil, nil
	}
	sess := &abiv1.Session{Id: si.ID, Title: si.Title}
	if si.Tokens != nil {
		sess.Cost = si.Tokens.cost()
	}
	if si.Time != nil && si.Time.Created > 0 {
		sess.Time = &abiv1.TimeRange{StartedAt: millisToTS(si.Time.Created)}
	}
	return sess, nil
}

type messageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Agent     string `json:"agent"`
	Model     *struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
		ID         string `json:"id"`
	} `json:"model"`
	Cost   *costValue `json:"cost"`
	Tokens *tokens    `json:"tokens"`
	Time   *struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
	Error json.RawMessage `json:"error"`
}

func translateMessageABI(info json.RawMessage) (*abiv1.Message, error) {
	if len(info) == 0 {
		return nil, nil
	}
	var mi messageInfo
	if err := json.Unmarshal(info, &mi); err != nil {
		return nil, err
	}
	if mi.ID == "" {
		return nil, nil
	}
	msg := &abiv1.Message{Id: mi.ID, SessionId: mi.SessionID, Type: translateMessageRole(mi.Role)}
	if mi.Model != nil {
		id := mi.Model.ModelID
		if id == "" {
			id = mi.Model.ID
		}
		msg.Model = &abiv1.ModelRef{Id: id, Provider: mi.Model.ProviderID}
	}
	switch {
	case mi.Tokens != nil:
		msg.Cost = mi.Tokens.cost()
	case mi.Cost != nil:
		msg.Cost = &abiv1.Cost{}
	}
	if usd, ok := mi.Cost.usd(); ok {
		if msg.Cost == nil {
			msg.Cost = &abiv1.Cost{}
		}
		msg.Cost.CostUsd = usd
	}
	if mi.Time != nil && mi.Time.Created > 0 {
		msg.CreatedAt = millisToTS(mi.Time.Created)
	}
	if len(mi.Error) > 0 {
		msg.Error = translateErrorPayload(mi.Error)
	}
	return msg, nil
}

type partInfo struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	SessionID string `json:"sessionID"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	CallID    string `json:"callID"`
	Tool      string `json:"tool"`
	State     *struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"state"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
}

// translatePart maps opencode part shapes onto the closed 5-type contract
// union. Unknown part types ride the Custom valve (Kind
// "opencode.part.<type>", original payload preserved).
func translatePartABI(raw json.RawMessage) (*abiv1.Part, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p partInfo
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" && p.Type == "" {
		return nil, nil
	}
	out := &abiv1.Part{Id: p.ID}
	switch p.Type {
	case "text":
		out.Type = abiv1.PartType_PART_TYPE_TEXT
		out.Payload = &abiv1.Part_Text{Text: p.Text}
	case "reasoning":
		out.Type = abiv1.PartType_PART_TYPE_REASONING
		out.Payload = &abiv1.Part_Reasoning{Reasoning: p.Text}
	case "tool":
		out.Type = abiv1.PartType_PART_TYPE_TOOL
		tool := &abiv1.ToolPart{CallId: p.CallID, Name: p.Tool, Input: p.Input, Output: p.Output}
		if p.State != nil {
			tool.State = &abiv1.ToolState{Status: mapToolStatus(p.State.Status), Error: p.State.Error}
		}
		out.Payload = &abiv1.Part_Tool{Tool: tool}
	default:
		out.Type = abiv1.PartType_PART_TYPE_CUSTOM
		out.Payload = &abiv1.Part_Custom{Custom: &abiv1.CustomPart{Kind: "opencode.part." + p.Type, Data: raw}}
	}
	return out, nil
}

func mapToolStatus(s string) abiv1.ToolStatus {
	switch s {
	case "pending":
		return abiv1.ToolStatus_TOOL_STATUS_PENDING
	case "running":
		return abiv1.ToolStatus_TOOL_STATUS_RUNNING
	case "completed":
		return abiv1.ToolStatus_TOOL_STATUS_COMPLETED
	case "error":
		return abiv1.ToolStatus_TOOL_STATUS_ERROR
	default:
		return abiv1.ToolStatus_TOOL_STATUS_UNSPECIFIED
	}
}

func translateMessageRole(role string) abiv1.MessageType {
	switch role {
	case "user":
		return abiv1.MessageType_MESSAGE_TYPE_USER
	case "assistant":
		return abiv1.MessageType_MESSAGE_TYPE_ASSISTANT
	case "system":
		return abiv1.MessageType_MESSAGE_TYPE_SYSTEM
	default:
		return abiv1.MessageType_MESSAGE_TYPE_UNSPECIFIED
	}
}

func translateStepModel(m *struct {
	ID         string `json:"id"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
}) *abiv1.ModelRef {
	if m == nil {
		return nil
	}
	id := m.ModelID
	if id == "" {
		id = m.ID
	}
	return &abiv1.ModelRef{Id: id, Provider: m.ProviderID}
}

// translateSessionDiff maps session.diff file entries to FileChange part
// events (design 0049 rule 4: patch text authoritative). An empty diff
// carries nothing projectable.
func translateSessionDiff(env struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}, evt *abiv1.Event) (*abiv1.Event, bool, error) {
	var p struct {
		SessionID string `json:"sessionID"`
		Diff      []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
			Patch  string `json:"patch"`
			Before string `json:"before"`
			After  string `json:"after"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(env.Properties, &p); err != nil {
		return nil, true, fmt.Errorf("session.diff: %w", err)
	}
	if len(p.Diff) == 0 {
		return nil, false, nil
	}
	// One event per file entry; the caller (projection) folds by path.
	first := p.Diff[0]
	evt.Type = abiv1.EventType_EVENT_TYPE_PART_END
	evt.SessionId = p.SessionID
	evt.Part = &abiv1.Part{
		Type: abiv1.PartType_PART_TYPE_FILE_CHANGE,
		Payload: &abiv1.Part_FileChange{FileChange: &abiv1.FileDiff{
			Path:   first.Path,
			Status: mapChangeStatus(first.Status),
			Patch:  first.Patch,
		}},
	}
	return evt, true, nil
}

func mapChangeStatus(s string) abiv1.ChangeStatus {
	switch s {
	case "added":
		return abiv1.ChangeStatus_CHANGE_STATUS_ADDED
	case "modified":
		return abiv1.ChangeStatus_CHANGE_STATUS_MODIFIED
	case "deleted":
		return abiv1.ChangeStatus_CHANGE_STATUS_DELETED
	case "renamed":
		return abiv1.ChangeStatus_CHANGE_STATUS_RENAMED
	default:
		return abiv1.ChangeStatus_CHANGE_STATUS_UNSPECIFIED
	}
}

func translateNextText(env struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}, evt *abiv1.Event) (*abiv1.Event, bool, error) {
	var p struct {
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		PartID    string `json:"partID"`
		Text      string `json:"text"`
		Delta     string `json:"delta"`
	}
	if err := json.Unmarshal(env.Properties, &p); err != nil {
		return nil, true, fmt.Errorf("%s: %w", env.Type, err)
	}
	switch env.Type {
	case "session.next.text.started":
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_START
		evt.Part = &abiv1.Part{Id: p.PartID, Type: abiv1.PartType_PART_TYPE_TEXT, Payload: &abiv1.Part_Text{Text: p.Text}}
	case "session.next.text.delta":
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_DELTA
		evt.Delta = firstNonEmpty(p.Delta, p.Text)
	case "session.next.text.ended":
		evt.Type = abiv1.EventType_EVENT_TYPE_PART_END
		evt.Part = &abiv1.Part{Id: p.PartID, Type: abiv1.PartType_PART_TYPE_TEXT, Payload: &abiv1.Part_Text{Text: p.Text}}
	}
	evt.SessionId = p.SessionID
	evt.MessageId = p.MessageID
	evt.PartId = p.PartID
	return evt, evt.SessionId != "" || evt.MessageId != "", nil
}

func translateQuestionAsked(d *Dialect, env struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}, evt *abiv1.Event) (*abiv1.Event, bool, error) {
	req, err := d.ParseQuestionRequest(env.Type, env.Properties)
	if err != nil {
		return nil, true, err
	}
	evt.Type = abiv1.EventType_EVENT_TYPE_INPUT_REQUEST
	evt.SessionId = req.SessionID
	evt.Input = &abiv1.InputRequest{Id: req.ID, SessionId: req.SessionID, Kind: abiv1.InputKind_INPUT_KIND_QUESTION}
	if len(req.Questions) > 0 {
		q := req.Questions[0]
		evt.Input.Question = q.Question
		evt.Input.Header = q.Header
		evt.Input.Multiple = q.Multiple
		evt.Input.Custom = q.Custom
		for _, o := range q.Options {
			evt.Input.Options = append(evt.Input.Options, &abiv1.InputOption{Label: o.Label, Description: o.Description})
		}
	}
	return evt, true, nil
}

func translatePermissionAsked(d *Dialect, env struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}, evt *abiv1.Event) (*abiv1.Event, bool, error) {
	req, err := d.ParsePermissionRequest(env.Type, env.Properties)
	if err != nil {
		return nil, true, err
	}
	evt.Type = abiv1.EventType_EVENT_TYPE_INPUT_REQUEST
	evt.SessionId = req.SessionID
	evt.Input = &abiv1.InputRequest{
		Id: req.ID, SessionId: req.SessionID, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: req.Permission, Patterns: req.Patterns, Always: req.Always,
	}
	if req.Tool != nil {
		evt.Input.Tool = &abiv1.ToolRef{MessageId: req.Tool.MessageID, CallId: req.Tool.CallID}
	}
	return evt, true, nil
}

func translateInputResolved(env struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}, evt *abiv1.Event, idField string) (*abiv1.Event, bool, error) {
	var p struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(env.Properties, &p); err != nil {
		return nil, true, fmt.Errorf("%s: %w", env.Type, err)
	}
	evt.Type = abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED
	evt.SessionId = p.SessionID
	evt.Input = &abiv1.InputRequest{Id: p.ID, SessionId: p.SessionID}
	return evt, p.ID != "", nil
}

// bindPartIDs lifts the part envelope's IDs onto the event (I12).
func bindPartIDs(evt *abiv1.Event, raw json.RawMessage) {
	var p struct {
		MessageID string `json:"messageID"`
		SessionID string `json:"sessionID"`
	}
	if json.Unmarshal(raw, &p) == nil {
		if evt.MessageId == "" {
			evt.MessageId = p.MessageID
		}
		if evt.SessionId == "" {
			evt.SessionId = p.SessionID
		}
	}
}

func translateErrorPayload(raw json.RawMessage) *abiv1.Error {
	if len(raw) == 0 {
		return nil
	}
	var errStr string
	if json.Unmarshal(raw, &errStr) == nil {
		return &abiv1.Error{Message: errStr}
	}
	var errObj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &errObj) == nil && (errObj.Message != "" || errObj.Type != "") {
		return &abiv1.Error{Code: errObj.Type, Message: errObj.Message}
	}
	return nil
}

func mapSessionStatus(s string) abiv1.SessionStatus {
	switch s {
	case "idle":
		return abiv1.SessionStatus_SESSION_STATUS_IDLE
	case "busy", "retry":
		return abiv1.SessionStatus_SESSION_STATUS_BUSY
	case "error":
		return abiv1.SessionStatus_SESSION_STATUS_ERROR
	case "compacting":
		return abiv1.SessionStatus_SESSION_STATUS_COMPACTING
	default:
		return abiv1.SessionStatus_SESSION_STATUS_UNSPECIFIED
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func millisToTS(ms int64) *timestamppb.Timestamp { return timestamppb.New(time.UnixMilli(ms)) }

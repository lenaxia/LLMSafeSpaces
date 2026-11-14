// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// client_events.go — translation of pi session events to CONTRACT
// ClientEvents (the US-65.8 bridge): ClientEventsFromEvent for the
// proxy's raw-record hot path, plus the streaming state machine Stream
// runs (part correlation across message_update / tool_execution_*).
//
// Streaming-id honesty note: pi's event stream carries NO message ids —
// ids exist only on persisted entries (get_entries / the session file).
// Stream therefore mints per-subscription pseudo ids (pi-msg-N /
// c<index> parts); GetHistory surfaces the durable entry ids. Clients
// correlate live parts by stream position, exactly as pi's own TUI and
// reference RPC client do (json.md "Use contentIndex to identify the
// content block").
//
// sessionId injection: pi event records carry no sessionId either. The
// transport that knows which child a record came from injects a
// top-level "sessionId" string into the raw record bytes (additive;
// defensive decode both directions) so the proxy's usagestream path —
// which needs a session id from the event (ContextUsageFromEvent must
// return a non-empty id when ok) — can key it. The fixture transport
// does the injection; S5's production transport must too.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// clientEvents translates one parsed event into zero or more contract
// events for a known session (Stream) or an unknown one (the raw path;
// SessionID then comes from the injected field only).
func clientEvents(ev piEvent, sessionID string, now time.Time, tr *streamTranslator) []session.Event {
	if ev.Type == "" {
		return nil
	}
	sid := sessionID
	if sid == "" {
		sid = ev.SessionID
	}
	mk := func(t session.EventType, f func(*session.Event)) session.Event {
		e := session.Event{Type: t, Timestamp: now, SessionID: sid}
		f(&e)
		return e
	}
	switch ev.Type {
	case evAgentStart:
		return []session.Event{mk(session.EventSessionStatus, func(e *session.Event) { e.Status = session.StatusBusy })}
	case evAgentSettled:
		return []session.Event{mk(session.EventSessionStatus, func(e *session.Event) { e.Status = session.StatusIdle })}
	case evMessageStart:
		if ev.Message == nil {
			return nil
		}
		role := messageRole(ev.Message)
		if role != "user" && role != "assistant" {
			return nil
		}
		mid := tr.beginMessage()
		return []session.Event{mk(session.EventMessageStart, func(e *session.Event) {
			e.MessageID = mid
			e.Message = &session.Message{ID: mid, Type: messageTypeForRole(role)}
		})}
	case evMessageEnd:
		if ev.Message == nil {
			return nil
		}
		msg := tr.endMessage(ev.Message, sid, now)
		if msg == nil {
			return nil
		}
		return []session.Event{mk(session.EventMessageEnd, func(e *session.Event) {
			e.MessageID = msg.ID
			e.Message = msg
		})}
	case evMessageUpdate:
		if ev.AssistantMessageEvent == nil {
			return nil
		}
		return tr.assistantUpdate(ev, sid, now)
	case evToolExecutionStart, evToolExecutionUpdate, evToolExecutionEnd:
		return tr.toolExecution(ev, sid, now)
	case evCompactionStart:
		return []session.Event{mk(session.EventSessionStatus, func(e *session.Event) { e.Status = session.StatusCompacting })}
	case evCompactionEnd:
		return tr.compactionEnd(ev, sid, now)
	case evSessionInfoChanged:
		if ev.Name == nil {
			return nil
		}
		return []session.Event{mk(session.EventSessionUpdated, func(e *session.Event) {
			e.Session = &session.Session{ID: sid, Title: *ev.Name}
		})}
	case evExtensionError:
		msg := ev.Error
		if msg == "" {
			msg = "pi extension error"
		}
		if ev.ExtensionPath != "" {
			msg = ev.ExtensionPath + ": " + msg
		}
		return []session.Event{mk(session.EventError, func(e *session.Event) {
			e.Error = &session.Error{Code: "extension_error", Message: msg}
		})}
	default:
		// agent_end (NOT settle — json.md), turn_start/turn_end,
		// queue_update, thinking_level_changed, entry_appended,
		// bash_execution_update (the RPC bash sideband the platform
		// does not drive), summarization_retry_*, extension UI
		// records, and unknown types: no client-facing signal.
		return nil
	}
}

// messageRole probes a raw AgentMessage for its role.
func messageRole(raw json.RawMessage) string {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Role
}

func messageTypeForRole(role string) session.MessageType {
	if role == "user" {
		return session.MessageUser
	}
	return session.MessageAssistant
}

// streamTranslator carries one Stream subscription's correlation state:
// the current streaming message's pseudo id and its content-index →
// part-kind map, plus the toolCallId → partID map built from
// toolcall_start so tool_execution_* can target the same part.
type streamTranslator struct {
	seq          int
	msgSeq       int
	currentMsgID string
	// kinds maps contentIndex → part kind ("text"|"thinking"|"tool").
	kinds map[int]string
	// toolParts maps toolCallId → the part id minted at toolcall_start.
	toolParts map[string]string
}

func newStreamTranslator() *streamTranslator {
	return &streamTranslator{kinds: map[int]string{}, toolParts: map[string]string{}}
}

func (tr *streamTranslator) beginMessage() string {
	tr.msgSeq++
	tr.currentMsgID = fmt.Sprintf("pi-msg-%d", tr.msgSeq)
	tr.kinds = map[int]string{}
	return tr.currentMsgID
}

// endMessage builds the authoritative final message from message_end's
// payload. Non-user/assistant roles produce nothing. A subscription
// that started mid-message (no message_start seen) still mints an id
// so no contract Message ever carries an empty one.
func (tr *streamTranslator) endMessage(raw json.RawMessage, sessionID string, now time.Time) *session.Message {
	var msg piAgentMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	if tr.currentMsgID == "" {
		tr.beginMessage()
	}
	switch msg.Role {
	case "user":
		m := session.UserMessage(tr.currentMsgID, msg.textContent(), msg.createdAt(nil))
		m.SessionID = sessionID
		return &m
	case "assistant":
		m := translateAssistantMessage(&piEntry{ID: tr.currentMsgID}, &msg, map[string]*piAgentMessage{})
		m.SessionID = sessionID
		return &m
	default:
		return nil
	}
}

// partID mints the deterministic part id for one content index.
func (tr *streamTranslator) partID(contentIndex int) string {
	return fmt.Sprintf("%s:c%d", orDefault(tr.currentMsgID, "pi"), contentIndex)
}

// assistantUpdate maps one message_update.assistantMessageEvent.
func (tr *streamTranslator) assistantUpdate(ev piEvent, sessionID string, now time.Time) []session.Event {
	ame := ev.AssistantMessageEvent
	base := func(t session.EventType, f func(*session.Event)) session.Event {
		e := session.Event{Type: t, Timestamp: now, SessionID: sessionID, MessageID: tr.currentMsgID, PartID: tr.partID(ame.ContentIndex)}
		f(&e)
		return e
	}
	switch ame.Type {
	case ameTextStart:
		tr.kinds[ame.ContentIndex] = "text"
		return []session.Event{base(session.EventPartStart, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartText, ID: e.PartID}
		})}
	case ameTextDelta:
		return []session.Event{base(session.EventPartDelta, func(e *session.Event) { e.Delta = ame.Delta })}
	case ameTextEnd:
		return []session.Event{base(session.EventPartEnd, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartText, ID: e.PartID, Text: ame.Content}
		})}
	case ameThinkStart:
		tr.kinds[ame.ContentIndex] = "thinking"
		return []session.Event{base(session.EventPartStart, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartReasoning, ID: e.PartID}
		})}
	case ameThinkDelta:
		return []session.Event{base(session.EventPartDelta, func(e *session.Event) { e.Delta = ame.Delta })}
	case ameThinkEnd:
		return []session.Event{base(session.EventPartEnd, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartReasoning, ID: e.PartID, Reasoning: ame.Content}
		})}
	case ameToolcallStart:
		tr.kinds[ame.ContentIndex] = "tool"
		if ame.ID != "" {
			tr.toolParts[ame.ID] = tr.partID(ame.ContentIndex)
		}
		return []session.Event{base(session.EventPartStart, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartTool, ID: e.PartID, Tool: &session.ToolPart{
				CallID: ame.ID, Name: ame.ToolName, State: session.ToolState{Status: session.ToolStatusPending},
			}}
		})}
	case ameToolcallDelta:
		return []session.Event{base(session.EventPartDelta, func(e *session.Event) { e.Delta = ame.Delta })}
	case ameToolcallEnd:
		if ame.ToolCall != nil && ame.ToolCall.ID != "" {
			tr.toolParts[ame.ToolCall.ID] = tr.partID(ame.ContentIndex)
		}
		var input json.RawMessage
		if ame.ToolCall != nil {
			input = ame.ToolCall.Arguments
		}
		name := ame.ToolName
		callID := ""
		if ame.ToolCall != nil {
			name = orDefault(ame.ToolCall.Name, name)
			callID = ame.ToolCall.ID
		}
		return []session.Event{base(session.EventPartEnd, func(e *session.Event) {
			e.Part = &session.Part{Type: session.PartTool, ID: e.PartID, Tool: &session.ToolPart{
				CallID: callID, Name: name, Input: input,
				State: session.ToolState{Status: session.ToolStatusPending},
			}}
		})}
	case ameError:
		msg := ame.Reason
		if ame.Error != nil && ame.Error.Message != "" {
			msg = orDefault(ame.Error.Message, msg)
		}
		if msg == "" {
			msg = "provider stream error"
		}
		e := session.Event{Type: session.EventError, Timestamp: now, SessionID: sessionID, MessageID: tr.currentMsgID}
		e.Error = &session.Error{Code: "provider_error", Message: msg}
		return []session.Event{e}
	default:
		// start, done, and drifted sub-types: no per-part signal.
		return nil
	}
}

// toolExecution maps the tool_execution_* lifecycle onto the part
// minted at toolcall_start (part updates ride part.end snapshots —
// the opencode part.updated mapping posture).
func (tr *streamTranslator) toolExecution(ev piEvent, sessionID string, now time.Time) []session.Event {
	pid, known := tr.toolParts[ev.ToolCallID]
	if !known {
		// A tool executing with no streamed call block (deferred /
		// codemode tools, or a subscription that started mid-turn):
		// mint the part on the fly keyed by the call id.
		pid = "call-" + ev.ToolCallID
		tr.toolParts[ev.ToolCallID] = pid
	}
	tool := &session.ToolPart{CallID: ev.ToolCallID, Name: ev.ToolName, Input: ev.Args}
	mk := func() session.Event {
		return session.Event{Type: session.EventPartEnd, Timestamp: now, SessionID: sessionID,
			MessageID: tr.currentMsgID, PartID: pid,
			Part: &session.Part{Type: session.PartTool, ID: pid, Tool: tool}}
	}
	switch ev.Type {
	case evToolExecutionStart:
		tool.State = session.ToolState{Status: session.ToolStatusRunning}
	case evToolExecutionUpdate:
		tool.Output = ev.Partial
		tool.State = session.ToolState{Status: session.ToolStatusRunning}
	case evToolExecutionEnd:
		tool.Output = ev.Result
		tool.State = session.ToolState{Status: session.ToolStatusCompleted, CompletedAt: &now}
		if ev.IsError {
			tool.State.Status = session.ToolStatusError
		}
	default:
		return nil
	}
	return []session.Event{mk()}
}

// compactionEnd surfaces the compaction summary as a transcript
// message (parity with history, where compaction entries translate to
// MessageCompaction) — start/end of the pair share the minted id.
func (tr *streamTranslator) compactionEnd(ev piEvent, sessionID string, now time.Time) []session.Event {
	if len(ev.Result) == 0 {
		return nil
	}
	res := piCompactionResult{}
	if err := json.Unmarshal(ev.Result, &res); err != nil || res.Summary == "" {
		return nil
	}
	tr.seq++
	id := "pi-compaction-" + strconv.Itoa(tr.seq)
	m := session.CompactionMessage(id, res.Summary, now)
	m.SessionID = sessionID
	start := session.Event{Type: session.EventMessageStart, Timestamp: now, SessionID: sessionID,
		MessageID: id, Message: &session.Message{ID: id, Type: session.MessageCompaction}}
	end := session.Event{Type: session.EventMessageEnd, Timestamp: now, SessionID: sessionID,
		MessageID: id, Message: &m}
	return []session.Event{start, end}
}

// --- Raw-record decode entry points (agent.Adapter surface) ----------

// injectSessionID merges a sessionId into raw record bytes (the
// transport-side annotation the usagestream path keys on).
func injectSessionID(raw []byte, sessionID string) []byte {
	if sessionID == "" || len(raw) == 0 {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return raw
	}
	if _, exists := obj["sessionId"]; exists {
		return raw
	}
	obj["sessionId"] = []byte(`"` + sessionID + `"`)
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// recordSessionID extracts the injected sessionId, when present.
func recordSessionID(raw []byte) string {
	var probe struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.SessionID
}

// contextUsageFromRecord derives the live context occupancy from one
// record: message_update's cumulative usage, Used = input + cacheRead +
// cacheWrite (the running request's occupancy — the same sum the
// opencode seam uses). ok requires BOTH usage and a keyed session id
// (the interface contract: ok=true ⇒ non-empty sessionID, non-nil
// usage).
func contextUsageFromRecord(raw []byte) (string, *session.ContextUsage, bool) {
	ev, ok := parseEvent(raw)
	if !ok || ev.Type != evMessageUpdate || ev.Usage == nil {
		return "", nil, false
	}
	sid := ev.SessionID
	if sid == "" {
		return "", nil, false
	}
	used := ev.Usage.promptTokens()
	if used <= 0 {
		return "", nil, false
	}
	return sid, &session.ContextUsage{Used: used}, true
}

// retryFromRecord derives the platform retry signal from
// auto_retry_start / auto_retry_end (json.md "Retry events"):
// attempt/maxAttempts/delayMs/errorMessage on start; success/finalError
// on end. NextAt is epoch millis (now + delayMs).
func retryFromRecord(raw []byte, now time.Time) (string, *agent.ClientRetryStatus, bool) {
	ev, ok := parseEvent(raw)
	if !ok {
		return "", nil, false
	}
	switch ev.Type {
	case evAutoRetryStart:
		sid := ev.SessionID
		if sid == "" {
			return "", nil, false
		}
		r := &agent.ClientRetryStatus{
			Attempt: ev.Attempt,
			Message: ev.ErrorMessage,
			NextAt:  now.Add(time.Duration(ev.DelayMs) * time.Millisecond).UnixMilli(),
			Action:  "retry",
		}
		if ev.MaxAttempts > 0 {
			r.Message = fmt.Sprintf("%s (attempt %d/%d)", r.Message, ev.Attempt, ev.MaxAttempts)
		}
		return sid, r, true
	case evAutoRetryEnd:
		sid := ev.SessionID
		if sid == "" {
			return "", nil, false
		}
		r := &agent.ClientRetryStatus{Attempt: ev.Attempt, Message: ev.ErrorMessage}
		if ev.Success {
			r.Action = "resolved"
		} else {
			r.Action = "failed"
			r.Message = orDefault(ev.FinalError, "retry exhausted")
		}
		return sid, r, true
	default:
		return "", nil, false
	}
}

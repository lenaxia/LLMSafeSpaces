// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// events.go — decode of the pi session-event stream (json.md). One JSON
// object per stdout line; the payload rides the envelope TOP level
// (`message_update.assistantMessageEvent`, `agent_settled.aborted`, …)
// — only command responses nest under `data`.
//
// Defensive-decode posture (scoping report §7): pi evolves its wire
// additively with no protocol-version handshake, so every decode here
// (a) ignores unknown fields, (b) classifies unknown event TYPES as
// tolerated non-events, and (c) never fails a stream because one
// record's payload drifted. The session-entry posture is pi's own:
// "treat unknown kind values as normal usage rather than rejecting
// them" (session-format.md).

import (
	"encoding/json"
)

// Event type names (json.md "Event sequence" / per-family tables).
const (
	evAgentStart          = "agent_start"
	evAgentEnd            = "agent_end"
	evAgentSettled        = "agent_settled"
	evTurnStart           = "turn_start"
	evTurnEnd             = "turn_end"
	evMessageStart        = "message_start"
	evMessageUpdate       = "message_update"
	evMessageEnd          = "message_end"
	evToolExecutionStart  = "tool_execution_start"
	evToolExecutionUpdate = "tool_execution_update"
	evToolExecutionEnd    = "tool_execution_end"
	evBashExecutionUpdate = "bash_execution_update"
	evQueueUpdate         = "queue_update"
	evEntryAppended       = "entry_appended"
	evSessionInfoChanged  = "session_info_changed"
	evThinkingLevelChange = "thinking_level_changed"
	evCompactionStart     = "compaction_start"
	evCompactionEnd       = "compaction_end"
	evAutoRetryStart      = "auto_retry_start"
	evAutoRetryEnd        = "auto_retry_end"
	evExtensionError      = "extension_error"
)

// assistantMessageEvent sub-types (json.md "Reconstruct streaming
// messages"). The union member's discriminator is `type`.
const (
	ameStart         = "start"
	ameTextStart     = "text_start"
	ameTextDelta     = "text_delta"
	ameTextEnd       = "text_end"
	ameThinkStart    = "thinking_start"
	ameThinkDelta    = "thinking_delta"
	ameThinkEnd      = "thinking_end"
	ameToolcallStart = "toolcall_start"
	ameToolcallDelta = "toolcall_delta"
	ameToolcallEnd   = "toolcall_end"
	ameDone          = "done"
	ameError         = "error"
)

// piUsage is the cumulative provider usage object (message-types.md).
// reasoning is already included in output; cost.total is USD.
type piUsage struct {
	Input      int64      `json:"input"`
	Output     int64      `json:"output"`
	CacheRead  int64      `json:"cacheRead"`
	CacheWrite int64      `json:"cacheWrite"`
	Reasoning  int64      `json:"reasoning,omitempty"`
	Total      int64      `json:"totalTokens,omitempty"`
	Cost       *piCostUSD `json:"cost,omitempty"`
}

// promptTokens is the context-occupancy numerator for the running
// request — the same input+cacheRead+cacheWrite sum the opencode seam
// uses for ContextUsage.Used.
func (u *piUsage) promptTokens() int64 {
	if u == nil {
		return 0
	}
	return u.Input + u.CacheRead + u.CacheWrite
}

type piCostUSD struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// totalCostUSD tolerates a missing cost object and the totalFromProvider
// drift field observed in recorded sessions (cost.total==0 with
// totalFromProvider:false means genuinely zero, not malformed).
func (u *piUsage) totalCostUSD() float64 {
	if u == nil || u.Cost == nil {
		return 0
	}
	return u.Cost.Total
}

// piEvent is the loose envelope every session event decodes into. One
// struct, every known field, unknown fields ignored — cheaper and more
// drift-tolerant than per-type structs on the hot path.
type piEvent struct {
	Type string `json:"type"`

	// Agent lifecycle.
	WillRetry bool `json:"willRetry"`
	Aborted   bool `json:"aborted"`

	// Messages. Role discriminates; Content is raw (string or block
	// array — translated lazily by translate.go).
	Message json.RawMessage `json:"message,omitempty"`
	Usage   *piUsage        `json:"usage,omitempty"`

	// Streaming deltas.
	AssistantMessageEvent *piAME `json:"assistantMessageEvent,omitempty"`

	// Tool execution lifecycle.
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Partial    json.RawMessage `json:"partialResult,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"` // tool_execution_end AND compaction_end (overloaded by type; compactionEnd decodes its shape lazily)
	IsError    bool            `json:"isError"`
	DurationMs *int64          `json:"durationMs,omitempty"`

	// Queue / session info.
	Queue     *piQueueData `json:"queue,omitempty"`
	SessionID string       `json:"sessionId,omitempty"`
	Name      *string      `json:"name,omitempty"`
	Level     string       `json:"level,omitempty"`

	// Compaction.
	Reason string `json:"reason,omitempty"`

	// Auto-retry.
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts,omitempty"`
	DelayMs      int64  `json:"delayMs,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	FinalError   string `json:"finalError,omitempty"`
	Success      bool   `json:"success"`

	// Extension errors.
	ExtensionPath string `json:"extensionPath,omitempty"`
	Error         string `json:"error,omitempty"`
}

// piAME is one message_update.assistantMessageEvent (json.md). Delta
// fields are present per sub-type; absent fields zero out.
type piAME struct {
	Type         string          `json:"type"`
	ContentIndex int             `json:"contentIndex"`
	Delta        string          `json:"delta,omitempty"`
	Content      string          `json:"content,omitempty"`
	ID           string          `json:"id,omitempty"` // toolcall_start
	ToolName     string          `json:"toolName,omitempty"`
	ToolCall     *piToolCall     `json:"toolCall,omitempty"` // toolcall_end
	Reason       string          `json:"reason,omitempty"`   // done/error
	Message      json.RawMessage `json:"message,omitempty"`  // done (absent on the wire in normal runs)
	Error        *piAbortMessage `json:"error,omitempty"`    // error
}

// piToolCall is the complete ToolCall a toolcall_end carries
// (message-types.md).
type piToolCall struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// piAbortMessage is the error-payload object an ameError may carry.
type piAbortMessage struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// piCompactionResult is compaction_end.result / the compact response's
// shared shape.
type piCompactionResult struct {
	Summary          string `json:"summary,omitempty"`
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int64  `json:"tokensBefore,omitempty"`
	EstimatedAfter   int64  `json:"estimatedTokensAfter,omitempty"`
	Aborted          bool   `json:"aborted"`
	WillRetry        bool   `json:"willRetry"`
	ErrorMessage     string `json:"errorMessage,omitempty"`
}

// parseEvent decodes one raw record. ok=false for undecodable bytes
// (the caller drops them silently — one bad line never kills a stream)
// and for the response family (not an event; routed elsewhere).
func parseEvent(raw []byte) (piEvent, bool) {
	// Cheap pre-probe for the envelope type without allocating a second
	// decode on the hot path: probe decodes only {type}.
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return piEvent{}, false
	}
	if probe.Type == "" || probe.Type == "response" {
		return piEvent{}, false
	}
	ev := piEvent{Type: probe.Type}
	if err := json.Unmarshal(raw, &ev); err != nil {
		// The probe succeeded but the full decode failed (conflicting
		// types on a drifted field). The type is still known: deliver
		// the envelope with zeroed payloads — lifecycle events
		// (agent_start/turn_start) carry none anyway.
		return ev, true
	}
	return ev, true
}

// knownEventTypes is the drift gate (the opencode wire package's
// knownEventTypes posture): every event type the pinned pi version
// (v1.5.x, json.md) emits, asserted by fixture coverage in
// events_test.go — a fixture refresh that introduces a new type must
// extend this map in the same change, and tolerance of unknown types
// (parseEvent) is the runtime backstop, not an excuse to skip the gate.
var knownEventTypes = map[string]bool{
	evAgentStart:          true,
	evAgentEnd:            true,
	evAgentSettled:        true,
	evTurnStart:           true,
	evTurnEnd:             true,
	evMessageStart:        true,
	evMessageUpdate:       true,
	evMessageEnd:          true,
	evToolExecutionStart:  true,
	evToolExecutionUpdate: true,
	evToolExecutionEnd:    true,
	evBashExecutionUpdate: true,
	evQueueUpdate:         true,
	evEntryAppended:       true,
	evSessionInfoChanged:  true,
	evThinkingLevelChange: true,
	evCompactionStart:     true,
	evCompactionEnd:       true,
	evAutoRetryStart:      true,
	evAutoRetryEnd:        true,
	evExtensionError:      true,
	// json.md's summarization-retry family (compaction/branch-summary
	// backoff) — tolerated, no platform signal.
	"summarization_retry_scheduled":     true,
	"summarization_retry_attempt_start": true,
	"summarization_retry_finished":      true,
	// RPC extension-UI records (rpc-extension-ui.md) ride the same
	// stream; the platform drives no extension UI today.
	"extension_ui_request": true,
}

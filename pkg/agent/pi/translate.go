// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// translate.go — pure translation from pi wire shapes (AgentMessage,
// session entries) to the platform session contract. Bytes in,
// session.* out; no I/O. Mirrors the opencode seam's translate.go
// discipline: ses_/msg_ style prefixes are preserved verbatim — pi's
// durable ENTRY ids are the platform's message ids (the only stable ids
// pi mints), surfaced without re-prefixing.
//
// Fusion rule (the one structural difference from opencode): pi models
// a tool call (assistant content block) and its result (a separate
// toolResult message) as two records; the platform contract carries
// both as one ToolPart on the assistant message. History translation
// therefore walks the active branch twice — once to index toolResults
// by toolCallId, once to emit messages with results fused in. On the
// live event path the same fusion happens incrementally (part state
// pending → running → completed/error across tool_execution_* events).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// piEntry is one persisted session entry (session-format.md). The tree
// id/parentId pair is the durable-cursor substrate get_entries exposes.
type piEntry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp,omitempty"`

	// message entries.
	Message *piAgentMessage `json:"message,omitempty"`

	// model_change entries.
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`

	// thinking_level_change entries.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`

	// compaction / branch_summary entries.
	Summary          string          `json:"summary,omitempty"`
	FirstKeptEntryID string          `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int64           `json:"tokensBefore,omitempty"`
	SystemMessage    json.RawMessage `json:"systemMessage,omitempty"`

	// custom / custom_message entries.
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Display    *bool           `json:"display,omitempty"`

	// session_info entries (and session_info_changed events carry the
	// same field at the envelope top level).
	Name *string `json:"name,omitempty"`
}

// ts parses the entry's ISO-8601 timestamp (millisecond precision).
func (e *piEntry) ts() time.Time {
	if e == nil || e.Timestamp == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// piAgentMessage is the AgentMessage union (message-types.md), decoded
// loosely: role discriminates, unknown roles are skipped by the
// translator, unknown fields are ignored everywhere.
type piAgentMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"` // unix millis

	// Assistant fields.
	API           string   `json:"api,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	ResponseModel string   `json:"responseModel,omitempty"`
	Usage         *piUsage `json:"usage,omitempty"`
	StopReason    string   `json:"stopReason,omitempty"`
	ErrorMessage  string   `json:"errorMessage,omitempty"`
	ThinkingLevel string   `json:"thinkingLevel,omitempty"`
	DurationMs    *int64   `json:"durationMs,omitempty"`

	// ToolResult fields.
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError"`

	// BashExecution fields. pi's wire also carries a British-spelled
	// cancellation boolean (message-types.md BashExecutionMessage) that
	// the platform shape does not consume — it is deliberately NOT
	// modeled, so it stays an ignored unknown field (the
	// defensive-decode posture; modeling it would put the British
	// spelling in a struct tag, and the raw misspell fixer rewrites
	// tags — the r1 Lint gate finding).
	Command  string `json:"command,omitempty"`
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`

	// Custom message fields.
	CustomType string `json:"customType,omitempty"`
	Display    *bool  `json:"display,omitempty"`

	// BranchSummary / CompactionSummary fields.
	Summary      string `json:"summary,omitempty"`
	TokensBefore int64  `json:"tokensBefore,omitempty"`
}

// piContentBlock is the content-block union (message-types.md): text,
// thinking, toolCall, image. One struct; Type discriminates.
type piContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Redacted  bool            `json:"redacted,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Data      string          `json:"data,omitempty"`
	MimeType  string          `json:"mimeType,omitempty"`
}

// blocks decodes Content, which is either a bare string (user/system
// shorthand) or a block array. A bare string becomes one text block.
func (m *piAgentMessage) blocks() []piContentBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if s == "" {
			return nil
		}
		return []piContentBlock{{Type: "text", Text: s}}
	}
	var blocks []piContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// textContent concatenates the text blocks (the platform user-message
// payload; images have no user-message carrier and are dropped — the
// text-only-history posture, documented in the package doc).
func (m *piAgentMessage) textContent() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.blocks() {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// createdAt converts the nested unix-millis timestamp; zero falls back
// to the entry's ISO timestamp (both call sites have the entry).
func (m *piAgentMessage) createdAt(entry *piEntry) time.Time {
	if m != nil && m.Timestamp > 0 {
		return time.UnixMilli(m.Timestamp).UTC()
	}
	return entry.ts()
}

// toolResultOutput renders a toolResult's content as the ToolPart
// Output payload: the raw content-block array verbatim when it is one,
// else a JSON string of the concatenated text.
func (m *piAgentMessage) toolResultOutput() json.RawMessage {
	if m == nil {
		return nil
	}
	if len(m.Content) > 0 {
		var arr []json.RawMessage
		if err := json.Unmarshal(m.Content, &arr); err == nil && arr != nil {
			return m.Content
		}
	}
	out, _ := json.Marshal(m.textContent())
	if string(out) == `""` {
		return nil
	}
	return out
}

// translateAssistantMessage builds the platform assistant Message from
// one pi assistant message, fusing toolResults by toolCallId. Entry is
// the message's own entry (id + timestamp fallback).
func translateAssistantMessage(entry *piEntry, msg *piAgentMessage, results map[string]*piAgentMessage) session.Message {
	m := session.AssistantMessage(entry.ID, nil, msg.createdAt(entry))
	parts := make([]session.Part, 0, 4)
	for i, blk := range msg.blocks() {
		partID := fmt.Sprintf("%s:%d", entry.ID, i)
		switch blk.Type {
		case "text":
			parts = append(parts, session.Part{Type: session.PartText, ID: partID, Text: blk.Text})
		case "thinking":
			parts = append(parts, session.Part{Type: session.PartReasoning, ID: partID, Reasoning: blk.Thinking})
		case "toolCall":
			tool := &session.ToolPart{
				CallID: blk.ID,
				Name:   blk.Name,
				Input:  blk.Arguments,
				State:  session.ToolState{Status: session.ToolStatusCompleted},
			}
			if res, ok := results[blk.ID]; ok && res != nil {
				tool.Output = res.toolResultOutput()
				if res.IsError {
					tool.State.Status = session.ToolStatusError
				}
			} else {
				// Call without a persisted result yet: keep it pending
				// (mid-run history reads) rather than lying completed.
				tool.State.Status = session.ToolStatusPending
			}
			parts = append(parts, session.Part{Type: session.PartTool, ID: partID, Tool: tool})
		default:
			// image/unknown blocks in an assistant message: no contract
			// carrier; tolerated and dropped.
		}
	}
	if len(parts) > 0 {
		m.Parts = parts
	}
	if msg.Model != "" || msg.Provider != "" {
		m.Model = &session.ModelRef{ID: msg.Model, Provider: msg.Provider}
	}
	if msg.Usage != nil {
		m.Cost = usageToCost(msg.Usage)
	}
	switch msg.StopReason {
	case "error":
		m.Error = &session.Error{Code: "provider_error", Message: orDefault(msg.ErrorMessage, "assistant turn failed")}
	case "aborted":
		m.Error = &session.Error{Code: "aborted", Message: "assistant turn aborted"}
	}
	return m
}

// translateEntry converts one active-branch entry into a platform
// message. ok=false for entries with no transcript surface (system
// loadout messages, labels, usage bookkeeping, custom state, ...).
// toolResults are handled separately (fusion), so their ok is false too.
func translateEntry(e *piEntry, msg *piAgentMessage, results map[string]*piAgentMessage) (session.Message, bool) {
	switch {
	case msg != nil && msg.Role == "user":
		return session.UserMessage(e.ID, msg.textContent(), msg.createdAt(e)), true
	case msg != nil && msg.Role == "assistant":
		return translateAssistantMessage(e, msg, results), true
	case msg != nil && msg.Role == "toolResult":
		return session.Message{}, false // fused into the assistant message
	case msg != nil && msg.Role == "bashExecution":
		exit := msg.ExitCode
		return session.ShellMessage(e.ID, msg.Command, exit, msg.createdAt(e)), true
	case msg != nil && msg.Role == "compactionSummary":
		return session.CompactionMessage(e.ID, msg.Summary, msg.createdAt(e)), true
	case msg != nil && msg.Role == "custom":
		if msg.Display != nil && *msg.Display {
			return session.SystemMessage(e.ID, msg.textContent(), timePtr(msg.createdAt(e))), true
		}
		return session.Message{}, false
	case e.Type == "compaction":
		return session.CompactionMessage(e.ID, e.Summary, e.ts()), true
	case e.Type == "model_change":
		return session.ModelSwitchMessage(e.ID, nil, &session.ModelRef{ID: e.ModelID, Provider: e.Provider}, e.ts()), true
	default:
		// system loadout, thinking_level_change, usage, label, custom
		// state, branch_summary, context_edit, unknown kinds: no
		// transcript surface (defensive tolerance — never an error).
		return session.Message{}, false
	}
}

// translateEntries walks the active branch (leaf-to-root chain, emitted
// root-to-leaf) and produces the platform transcript. results indexes
// toolResult messages by toolCallId ahead of the emit pass.
func translateEntries(entries []piEntry, leafID string) []session.Message {
	byID := make(map[string]*piEntry, len(entries))
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}
	branch := activeBranch(byID, leafID)

	results := make(map[string]*piAgentMessage)
	for _, e := range branch {
		if e.Message != nil && e.Message.Role == "toolResult" && e.Message.ToolCallID != "" {
			results[e.Message.ToolCallID] = e.Message
		}
	}

	var msgs []session.Message
	for _, e := range branch {
		if m, ok := translateEntry(e, e.Message, results); ok {
			msgs = append(msgs, m)
		}
	}
	return msgs
}

// activeBranch resolves the leaf-to-root entry chain and returns it
// root-to-leaf. A cycle or a broken parent chain truncates at the
// break (defensive: persisted files are append-only trees, but a
// hand-edited file must never spin the reader).
func activeBranch(byID map[string]*piEntry, leafID string) []*piEntry {
	if leafID == "" {
		return nil
	}
	var chain []*piEntry
	seen := make(map[string]bool)
	cur, ok := byID[leafID]
	for ok && !seen[cur.ID] {
		seen[cur.ID] = true
		chain = append(chain, cur)
		if cur.ParentID == nil || *cur.ParentID == "" {
			break
		}
		cur, ok = byID[*cur.ParentID]
	}
	// Reverse in place: chain was collected leaf→root.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// usageToCost maps pi's cumulative Usage to the display-only contract
// Cost (reasoning rides inside output per message-types.md).
func usageToCost(u *piUsage) *session.Cost {
	if u == nil {
		return nil
	}
	return &session.Cost{
		InputTokens:      u.Input,
		OutputTokens:     u.Output,
		ReasoningTokens:  u.Reasoning,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
		TotalTokens:      u.Total,
		CostUSD:          u.totalCostUSD(),
	}
}

// modelObjectToInfo maps pi's full Model object to the platform
// catalog entry.
func modelObjectToInfo(m piModelObject) session.ModelInfo {
	return session.ModelInfo{
		ID:            m.ID,
		Provider:      m.Provider,
		DisplayName:   m.Name,
		ContextWindow: m.ContextWindow,
		MaxOutput:     m.MaxTokens,
	}
}

// modelRefFromSplit resolves the platform's provider/model pair from a
// ModelRef, splitting a slashed bare ID on the first segment (the
// opencode modelOverride posture: provider-authoritative prefix, first
// segment split, empty-tail forms unexpressible — pi's set_model needs
// BOTH fields, so a provider-less flat id cannot ride).
func modelRefFromSplit(m *session.ModelRef) (provider, modelID string, ok bool) {
	if m == nil {
		return "", "", false
	}
	if m.Provider != "" && m.ID != "" {
		return m.Provider, m.ID, true
	}
	if m.ID == "" {
		return "", "", false
	}
	if i := strings.Index(m.ID, "/"); i > 0 && i < len(m.ID)-1 {
		return m.ID[:i], m.ID[i+1:], true
	}
	return "", "", false
}

func timePtr(t time.Time) *time.Time { return &t }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

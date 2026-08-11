// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package opencode — translate.go
//
// Pure functions translating opencode's wire shapes (info+parts
// message objects, session records, SSE event envelopes) to and from
// the platform-owned pkg/session contract.
//
// These functions have NO I/O — they take bytes or structs and return
// session.* values. The adapter methods in adapter.go call them after
// HTTP round-trips; tests call them directly with fixture bytes.
//
// Discipline (design 0049 §4.1, §7):
//   - 5 part types forever. opencode-specific part types (step-start,
//     step-finish, patch) are dropped or translated, NEVER added as
//     new PartType constants.
//   - No opencode identifiers leak: ses_/msg_/per_/que_ prefixes are
//     preserved verbatim by the adapter (the contract does not
//     re-prefix) but the function doc strings do not bake them into
//     the contract's vocabulary.
//   - Diffs are unified-diff text. opencode's `patch` part carries
//     only file paths; FileChange parts are produced separately via
//     filediff.Producer from `git diff` on the PVC.

package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// ocMessage is the wire shape opencode returns for one entry in
// GET /session/:id/message (and the top-level shape for
// POST /session/:id/message). Info carries role + IDs; Parts is the
// ordered content list.
type ocMessage struct {
	Info  ocInfo         `json:"info"`
	Parts []ocPart       `json:"parts"`
	Model *ocModelRef    `json:"model,omitempty"`
	Cost  *ocCost        `json:"cost,omitempty"`
	Time  *ocTime        `json:"time,omitempty"`
	Error *session.Error `json:"error,omitempty"`
}

type ocInfo struct {
	Role      string `json:"role"`
	ID        string `json:"id"`
	SessionID string `json:"sessionID,omitempty"`
	Title     string `json:"title,omitempty"`

	// Shell-message fields (role=="shell"):
	Command  string `json:"command,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`

	// Type-discriminator for non-text/non-tool messages (agent_switch,
	// model_switch, compaction, system). Empty for user/assistant/shell.
	Type string `json:"type,omitempty"`

	// agent_switch:
	FromAgent string `json:"fromAgent,omitempty"`
	ToAgent   string `json:"toAgent,omitempty"`

	// model_switch:
	FromModel *ocModelRef `json:"fromModel,omitempty"`
	ToModel   *ocModelRef `json:"toModel,omitempty"`
}

type ocModelRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

type ocCost struct {
	InputTokens      int64   `json:"input,omitempty"`
	OutputTokens     int64   `json:"output,omitempty"`
	ReasoningTokens  int64   `json:"reasoning,omitempty"`
	CacheReadTokens  int64   `json:"cacheRead,omitempty"`
	CacheWriteTokens int64   `json:"cacheWrite,omitempty"`
	TotalTokens      int64   `json:"total,omitempty"`
	CostUSD          float64 `json:"cost,omitempty"`
}

type ocTime struct {
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// ocPart is one entry in opencode's parts array. Type discriminates.
// Unknown types (step-start, step-finish, patch, custom extensions)
// are passed through as-is and the translator decides what to keep.
//
// opencode changed the tool-part wire shape between 1.15.x and 1.18.10:
//   - 1.15.x (legacy nested): "tool": {"name": ..., "callID": ..., ...}
//   - 1.18.10 (flat string):  "tool": "bash" (bare name string) with
//     callID/state/input/output hoisted to the part level.
//
// UnmarshalJSON normalizes both shapes into the canonical ocTool so
// translateTool and the downstream session.ToolPart contract stay
// unchanged (issue #730).
type ocPart struct {
	Type string `json:"type"`

	// Common identifying fields:
	ID        string `json:"id,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	MessageID string `json:"messageID,omitempty"`

	// text / reasoning:
	Text      string `json:"text,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`

	// tool (populated by UnmarshalJSON from either wire shape):
	Tool *ocTool `json:"tool,omitempty"`

	// patch (file paths only — diff text comes from filediff):
	Files []string `json:"files,omitempty"`

	// Custom pass-through:
	Custom *session.CustomPart `json:"custom,omitempty"`
}

// UnmarshalJSON normalizes the two opencode tool-part wire shapes into
// the canonical ocPart.Tool. Non-tool parts decode identically to the
// default. For tool parts:
//
//   - Flat string (1.18.10+): {"type":"tool","tool":"bash","callID":"...",
//     "state":{"status":"...","input":{...},"output":"...","time":{...}}}
//     → Tool.Name = "bash", Tool.CallID = part-level callID,
//     Tool.Input/Output from state, Tool.State.{StartedAt,CompletedAt}
//     from state.time.{start,end} (epoch-millis).
//
//   - Legacy nested (≤1.15.x): {"type":"tool","tool":{"name":"bash",
//     "callID":"...","input":{...},"state":{"status":"...",
//     "startedAt":"...","completedAt":"..."}}}
//     → decoded directly into ocTool.
func (p *ocPart) UnmarshalJSON(data []byte) error {
	// Intermediate: same fields as ocPart but Tool is raw so we can
	// inspect its JSON kind before committing to a decode path. The
	// extra fields (CallID, State) are the flat-shape hoisted fields.
	var intermediate struct {
		Type      string              `json:"type"`
		ID        string              `json:"id,omitempty"`
		SessionID string              `json:"sessionID,omitempty"`
		MessageID string              `json:"messageID,omitempty"`
		Text      string              `json:"text,omitempty"`
		Reasoning string              `json:"reasoning,omitempty"`
		Tool      json.RawMessage     `json:"tool,omitempty"`
		Files     []string            `json:"files,omitempty"`
		Custom    *session.CustomPart `json:"custom,omitempty"`
		CallID    string              `json:"callID,omitempty"`
		State     json.RawMessage     `json:"state,omitempty"`
	}
	if err := json.Unmarshal(data, &intermediate); err != nil {
		return err
	}

	p.Type = intermediate.Type
	p.ID = intermediate.ID
	p.SessionID = intermediate.SessionID
	p.MessageID = intermediate.MessageID
	p.Text = intermediate.Text
	p.Reasoning = intermediate.Reasoning
	p.Files = intermediate.Files
	p.Custom = intermediate.Custom

	if len(intermediate.Tool) == 0 {
		return nil
	}

	trimmed := bytes.TrimSpace(intermediate.Tool)
	if bytes.HasPrefix(trimmed, []byte("\"")) {
		// Flat string shape (opencode 1.18.10+): "tool":"<name>".
		var toolName string
		if err := json.Unmarshal(intermediate.Tool, &toolName); err != nil {
			return fmt.Errorf("decode flat tool name: %w", err)
		}
		tool := &ocTool{
			Name:   toolName,
			CallID: intermediate.CallID,
		}
		if len(intermediate.State) > 0 {
			var fs ocFlatToolState
			if err := json.Unmarshal(intermediate.State, &fs); err != nil {
				return fmt.Errorf("decode flat tool state: %w", err)
			}
			tool.Input = fs.Input
			tool.Output = fs.Output
			tool.State = &ocToolState{
				Status: fs.Status,
				Error:  fs.Error,
			}
			if fs.Time != nil {
				if fs.Time.Start > 0 {
					t := time.UnixMilli(fs.Time.Start)
					tool.State.StartedAt = &t
				}
				if fs.Time.End > 0 {
					t := time.UnixMilli(fs.Time.End)
					tool.State.CompletedAt = &t
				}
			}
		}
		p.Tool = tool
		return nil
	}

	// Legacy nested-object shape (opencode ≤1.15.x): "tool":{...}.
	var nested ocTool
	if err := json.Unmarshal(intermediate.Tool, &nested); err != nil {
		return fmt.Errorf("decode nested tool object: %w", err)
	}
	p.Tool = &nested
	return nil
}

type ocTool struct {
	CallID string          `json:"callID,omitempty"`
	Name   string          `json:"name,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	State  *ocToolState    `json:"state,omitempty"`
}

type ocToolState struct {
	Status      string     `json:"status,omitempty"`
	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// ocFlatToolState is the state object on a flat-shape (1.18.10+) tool
// part. Compared to the legacy ocToolState, input/output live INSIDE
// state (not on the tool object), and times are epoch-millis numbers
// under state.time.{start,end} (not ISO-8601 strings).
type ocFlatToolState struct {
	Status   string          `json:"status,omitempty"`
	Error    string          `json:"error,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Title    string          `json:"title,omitempty"`
	Time     *ocFlatToolTime `json:"time,omitempty"`
}

// ocFlatToolTime carries the epoch-millis start/end of a flat-shape
// tool call (opencode 1.18.10+).
type ocFlatToolTime struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

// translateMessage converts one opencode wire message to a platform
// session.Message. File paths from `patch` parts are collected and
// returned so the caller can run filediff.Producer against them to
// produce FileChange parts (the function itself does no I/O).
//
// opencode-specific parts dropped:
//   - step-start, step-finish: turn-boundary markers, no content
//   - patch: file paths only; translated to FileChange parts by the
//     caller via filediff (the paths come back in changedFiles)
//
// opencode-specific parts kept:
//   - text → PartText
//   - reasoning → PartReasoning
//   - tool → PartTool (with full state)
//   - custom → PartCustom (pressure-relief valve, preserved verbatim)
//
// changedFiles is nil/empty when the message had no patch part or the
// patch listed no files. Duplicate paths are de-duplicated to keep
// filediff's input clean.
func translateMessage(m ocMessage) (session.Message, []string) {
	sm := session.Message{
		ID:        m.Info.ID,
		SessionID: m.Info.SessionID,
		CreatedAt: time.Time{},
	}

	switch m.Info.Role {
	case "user":
		sm.Type = session.MessageUser
	case "assistant":
		sm.Type = session.MessageAssistant
	case "shell":
		sm.Type = session.MessageShell
		sm.Command = m.Info.Command
		sm.ExitCode = m.Info.ExitCode
	default:
		// info.type is the discriminator for non-standard messages.
		switch m.Info.Type {
		case "agent_switch":
			sm.Type = session.MessageAgentSwitch
			sm.FromAgent = m.Info.FromAgent
			sm.ToAgent = m.Info.ToAgent
		case "model_switch":
			sm.Type = session.MessageModelSwitch
			if m.Info.FromModel != nil {
				sm.FromModel = &session.ModelRef{ID: m.Info.FromModel.ID, Provider: m.Info.FromModel.Provider}
			}
			if m.Info.ToModel != nil {
				sm.ToModel = &session.ModelRef{ID: m.Info.ToModel.ID, Provider: m.Info.ToModel.Provider}
			}
		case "compaction":
			sm.Type = session.MessageCompaction
		case "system":
			sm.Type = session.MessageSystem
		default:
			// Unknown role+type — preserve as system so the timeline
			// is coherent. Adapter logs at translate-time so unknown
			// shapes are visible without failing the request.
			sm.Type = session.MessageSystem
		}
	}

	if m.Model != nil {
		sm.Model = &session.ModelRef{ID: m.Model.ID, Provider: m.Model.Provider}
	}
	if m.Cost != nil {
		sm.Cost = translateCost(*m.Cost)
	}
	if m.Time != nil {
		// Use the wire-provided StartedAt if non-zero; otherwise leave
		// CreatedAt zero (caller may fill from Info).
		if !m.Time.StartedAt.IsZero() {
			sm.CreatedAt = m.Time.StartedAt
		}
	}
	if m.Error != nil {
		sm.Error = m.Error
	}

	// Translate parts. step-start/step-finish are skipped (turn
	// boundary markers carry no renderable content). patch parts
	// contribute their file list to changedFiles; the caller turns
	// those into FileChange parts via filediff.
	parts := make([]session.Part, 0, len(m.Parts))
	seen := map[string]struct{}{}
	var changedFiles []string
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			parts = append(parts, session.Part{
				Type: session.PartText,
				ID:   p.ID,
				Text: p.Text,
			})
		case "reasoning":
			parts = append(parts, session.Part{
				Type:      session.PartReasoning,
				ID:        p.ID,
				Reasoning: p.Reasoning,
			})
		case "tool":
			parts = append(parts, session.Part{
				Type: session.PartTool,
				ID:   p.ID,
				Tool: translateTool(p.Tool),
			})
		case "custom":
			if p.Custom != nil && p.Custom.Kind != "" {
				parts = append(parts, session.Part{
					Type:   session.PartCustom,
					ID:     p.ID,
					Custom: p.Custom,
				})
			}
		case "patch":
			for _, f := range p.Files {
				if f == "" {
					continue
				}
				if _, dup := seen[f]; dup {
					continue
				}
				seen[f] = struct{}{}
				changedFiles = append(changedFiles, f)
			}
		case "step-start", "step-finish":
			// dropped — turn boundaries carry no renderable content
		default:
			// Unknown part type — preserve as Custom with the kind
			// set to the opencode type string so future extensions
			// surface in the UI rather than silently disappearing.
			// This is the discipline rule from design 0049 §4.3: the
			// Custom part is the pressure-relief valve.
			// NOTE: we do not have the raw bytes here (unmarshal
			// already happened), so we synthesize a Data payload with
			// the known fields. If the original part had additional
			// fields they are lost — the adapter logs at debug level.
			if p.Type != "" {
				customData, _ := json.Marshal(map[string]any{
					"type":      p.Type,
					"id":        p.ID,
					"sessionID": p.SessionID,
					"messageID": p.MessageID,
					"text":      p.Text,
					"reasoning": p.Reasoning,
				})
				parts = append(parts, session.Part{
					Type: session.PartCustom,
					ID:   p.ID,
					Custom: &session.CustomPart{
						Kind: p.Type,
						Data: customData,
					},
				})
			}
		}
	}
	sm.Parts = parts

	return sm, changedFiles
}

// translateTool converts an opencode tool state to the platform
// ToolPart shape. Nil opencode tool yields nil platform tool — the
// caller skips the part entirely in that case.
func translateTool(t *ocTool) *session.ToolPart {
	if t == nil {
		return nil
	}
	tp := &session.ToolPart{
		CallID: t.CallID,
		Name:   t.Name,
		Input:  t.Input,
		Output: t.Output,
		State:  session.ToolState{Status: session.ToolStatusPending}, // default
	}
	if t.State != nil {
		tp.State = session.ToolState{
			Status:      translateToolStatus(t.State.Status),
			Error:       t.State.Error,
			StartedAt:   t.State.StartedAt,
			CompletedAt: t.State.CompletedAt,
		}
	}
	return tp
}

// translateToolStatus maps opencode's tool-status strings to the
// platform ToolStatus enum. Unknown values map to ToolStatusPending
// (safe default — UI renders "working").
func translateToolStatus(s string) session.ToolStatus {
	switch s {
	case "pending":
		return session.ToolStatusPending
	case "running":
		return session.ToolStatusRunning
	case "completed":
		return session.ToolStatusCompleted
	case "error":
		return session.ToolStatusError
	default:
		return session.ToolStatusPending
	}
}

// translateCost converts an opencode cost record to a session.Cost.
// All fields are display-only (design 0049 §4.1 rule 5); billing is
// cgroup-based and never reads from these.
func translateCost(c ocCost) *session.Cost {
	return &session.Cost{
		InputTokens:      c.InputTokens,
		OutputTokens:     c.OutputTokens,
		ReasoningTokens:  c.ReasoningTokens,
		CacheReadTokens:  c.CacheReadTokens,
		CacheWriteTokens: c.CacheWriteTokens,
		TotalTokens:      c.TotalTokens,
		CostUSD:          c.CostUSD,
	}
}

// ocSession is the wire shape opencode returns for GET /session and
// GET /session/:id. Fields not consumed by the platform are ignored.
type ocSession struct {
	ID        string      `json:"id"`
	Title     string      `json:"title,omitempty"`
	Model     *ocModelRef `json:"model,omitempty"`
	Time      *ocTime     `json:"time,omitempty"`
	Cost      *ocCost     `json:"cost,omitempty"`
	Status    ocStatus    `json:"status"`
	IsSubtask bool        `json:"isSubtask,omitempty"`
	Summary   string      `json:"summary,omitempty"`
	ParentID  string      `json:"parentID,omitempty"`
	// Archived is absent in opencode 1.18.10; left for forward-compat.
	Archived bool `json:"archived,omitempty"`
}

type ocStatus struct {
	Type string `json:"type"`
}

// translateSession converts one opencode session record to the
// platform session.Session shape.
func translateSession(s ocSession, workspaceID string) session.Session {
	out := session.Session{
		ID:          s.ID,
		WorkspaceID: workspaceID,
		ParentID:    s.ParentID,
		Title:       s.Title,
		Status:      translateStatus(s.Status.Type),
		Summary:     s.Summary,
		Archived:    s.Archived,
	}
	if s.Model != nil {
		out.Model = &session.ModelRef{ID: s.Model.ID, Provider: s.Model.Provider}
	}
	if s.Time != nil {
		out.Time = &session.TimeRange{
			StartedAt:   s.Time.StartedAt,
			CompletedAt: s.Time.CompletedAt,
		}
	}
	if s.Cost != nil {
		out.Cost = translateCost(*s.Cost)
	}
	return out
}

// translateStatus maps opencode session-status types to the platform
// Status enum. "retry" maps to Busy (the platform treats retries as
// busy until they succeed or fail).
func translateStatus(s string) session.Status {
	switch s {
	case "idle":
		return session.StatusIdle
	case "busy":
		return session.StatusBusy
	case "retry":
		return session.StatusBusy
	case "error":
		return session.StatusError
	case "compacting":
		return session.StatusCompacting
	case "archived":
		return session.StatusArchived
	default:
		return session.StatusUnknown
	}
}

// ParseHistoryWire is the testable boundary: bytes in, contract out.
// Used by Adapter.GetHistory after the HTTP round-trip. Returns the
// translated messages AND a parallel slice of changed-file path lists
// (one entry per message; nil for messages with no patch part). The
// caller uses the changed-files to produce FileChange parts via
// filediff.Producer.
//
// Exported for the package's own test consumers (translate_test.go,
// adapter_test.go).
//
// Resilience (issue #730, README §12 containment): the body is decoded
// in two stages. Stage 1 splits the top-level JSON array into raw
// messages — this only fails if the body is not a JSON array at all (a
// genuine error, surfaced to the caller). Stage 2 decodes each message
// independently; a message that fails to decode (e.g. a future opencode
// wire-shape change in one part) is downgraded to a session.MessageSystem
// notice rather than failing the entire history. This ensures one bad
// upstream shape never Sev1s the history surface again.
func ParseHistoryWire(body []byte, workspaceID string) (msgs []session.Message, changedFilesPerMsg [][]string, err error) {
	// Stage 1: split into raw messages. Cannot fail on part-shape drift
	// because it does not descend into parts. Only fails if the body is
	// not a JSON array at all.
	var rawMessages []json.RawMessage
	if err = json.Unmarshal(body, &rawMessages); err != nil {
		return nil, nil, fmt.Errorf("opencode history: parse message array: %w", err)
	}

	// Stage 2: decode each message independently. A decode failure
	// (part-shape drift from a future opencode version change) degrades
	// that single message to a system notice; the rest translate
	// normally.
	msgs = make([]session.Message, 0, len(rawMessages))
	changedFilesPerMsg = make([][]string, 0, len(rawMessages))
	for i, rawMsg := range rawMessages {
		var m ocMessage
		if dErr := json.Unmarshal(rawMsg, &m); dErr != nil {
			msgs = append(msgs, session.SystemMessage(
				fmt.Sprintf("decode-failed-msg-%d", i),
				"This message could not be decoded (the agent history shape may have changed). "+
					"Other messages in this conversation are unaffected.",
				time.Time{},
			))
			changedFilesPerMsg = append(changedFilesPerMsg, nil)
			continue
		}
		sm, files := translateMessage(m)
		msgs = append(msgs, sm)
		changedFilesPerMsg = append(changedFilesPerMsg, files)
	}
	return msgs, changedFilesPerMsg, nil
}

// ParseSessionListWire is the testable boundary for GET /session.
//
// opencode 1.18.10 returns a bare array; some earlier versions wrap
// in {data: [...]}. We try the wrapped format first (parse succeeds →
// use it, including when Data is empty); fall back to the bare array
// only when the wrapped parse fails (body was not an object).
func ParseSessionListWire(body []byte, workspaceID string) ([]session.Session, error) {
	var wrapped struct {
		Data []ocSession `json:"data"`
	}
	// Wrapped-format parse succeeded → use it. len(Data)==0 is a
	// valid wrapped response (zero sessions), distinct from "the body
	// was not wrapped". The previous logic conflated these two and
	// returned an error on {"data": []} (PR #714 review C2).
	if err := json.Unmarshal(body, &wrapped); err == nil {
		out := make([]session.Session, 0, len(wrapped.Data))
		for _, s := range wrapped.Data {
			out = append(out, translateSession(s, workspaceID))
		}
		return out, nil
	}
	// Wrapped parse failed → try bare array.
	var bare []ocSession
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("opencode session list: parse: %w", err)
	}
	out := make([]session.Session, 0, len(bare))
	for _, s := range bare {
		out = append(out, translateSession(s, workspaceID))
	}
	return out, nil
}

// ParseSessionWire is the testable boundary for GET /session/:id.
func ParseSessionWire(body []byte, workspaceID string) (*session.Session, error) {
	// Try wrapped {data: {...}} first then bare object.
	var wrapped struct {
		Data ocSession `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data.ID != "" {
		s := translateSession(wrapped.Data, workspaceID)
		return &s, nil
	}
	var bare ocSession
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("opencode session: parse: %w", err)
	}
	s := translateSession(bare, workspaceID)
	return &s, nil
}

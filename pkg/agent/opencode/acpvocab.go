// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// The ACP sessionUpdate vocabulary as the adapter layer's internal event
// shapes (design 0063, Stage A: vocabulary + mapping only, NO transport).
//
// Direction: native opencode dialect -> ACP-shaped -> contract Part.
// The five contract part types remain canonical (design 0049); nothing
// inverts. The vocabulary carries the wire names of Agent Client
// Protocol (the surface opencode commits to for external clients) so
// US-65.8 and #1582 build against spec-shaped types instead of opencode
// dialect, and a future transport migration (design 0063 §6.2) can feed
// these shapes directly.
//
// Pins carried from the spike (design 0063 §3.2/§5, evidence
// design/evidence/acp-spike/):
//   - user_message_chunk is REPLAY-ONLY (fork/load history ingestion).
//     The native->ACP direction has no producer for it; a zero-echo
//     assumption cannot silently take the replay path.
//   - todowrite maps to kind "other" — opencode's todo tool emits a
//     generic tool_call, never the plan variant.
//   - plan is schema-present but never emitted by opencode 1.18.x; it
//     maps to the Tool row (0049 discipline: no PartPlan).
//   - unknown sessionUpdate kinds map to Custom(kind "acp.unknown")
//     and are never dropped (design 0063 §8 tolerant-forward).

// AcpUpdateKind is the sessionUpdate discriminator (ACP wire names).
type AcpUpdateKind string

const (
	AcpUpdateAgentMessageChunk AcpUpdateKind = "agent_message_chunk"
	AcpUpdateAgentThoughtChunk AcpUpdateKind = "agent_thought_chunk"
	AcpUpdateUserMessageChunk  AcpUpdateKind = "user_message_chunk" // replay-only
	AcpUpdateToolCall          AcpUpdateKind = "tool_call"
	AcpUpdateToolCallUpdate    AcpUpdateKind = "tool_call_update"
	AcpUpdateAvailableCommands AcpUpdateKind = "available_commands_update"
	AcpUpdateUsage             AcpUpdateKind = "usage_update" // session-level, never a Part
	AcpUpdatePlan              AcpUpdateKind = "plan"         // zod-only: never emitted by opencode 1.18.x
)

// IsNeverEmittedByNative reports kinds the native direction cannot
// synthesize. plan is schema-present but opencode 1.18.x emits todowrite
// as a generic tool_call (kind "other") — the plan variant never fires
// ([T:lifecycle 15.76], design 0063 §5.5).
func (k AcpUpdateKind) IsNeverEmittedByNative() bool {
	return k == AcpUpdatePlan
}

// NativeChunkKinds returns the chunk kinds the native (SSE dialect)
// direction produces. user_message_chunk is deliberately absent: it is
// replay-only (fork/load), and the native stream never echoes user
// input ([T:fork 13.86-13.87] — emitted at fork/load only, design 0063
// §3.2). Structural pin: a future zero-echo assumption cannot silently
// take the replay path because this set is the producer's contract.
func NativeChunkKinds() []AcpUpdateKind {
	return []AcpUpdateKind{AcpUpdateAgentMessageChunk, AcpUpdateAgentThoughtChunk}
}

// AcpContentBlock is one streamed content block (ACP shape: the chunk
// kinds carry one block each; text is the only variant opencode emits).
type AcpContentBlock struct {
	Type string `json:"type"` // "text" (image/audio/resource_* are contract Tool-output per 0049)
	Text string `json:"text,omitempty"`
}

// AcpToolKind is the toolCall kind union (ACP wire values).
type AcpToolKind string

const (
	AcpToolKindRead       AcpToolKind = "read"
	AcpToolKindEdit       AcpToolKind = "edit"
	AcpToolKindDelete     AcpToolKind = "delete"
	AcpToolKindMove       AcpToolKind = "move"
	AcpToolKindSearch     AcpToolKind = "search"
	AcpToolKindExecute    AcpToolKind = "execute"
	AcpToolKindThink      AcpToolKind = "think"
	AcpToolKindFetch      AcpToolKind = "fetch"
	AcpToolKindSwitchMode AcpToolKind = "switch_mode"
	AcpToolKindOther      AcpToolKind = "other"
)

// AcpToolStatus is the toolCall status state machine (ACP wire values).
type AcpToolStatus string

const (
	AcpToolStatusPending    AcpToolStatus = "pending"
	AcpToolStatusInProgress AcpToolStatus = "in_progress"
	AcpToolStatusCompleted  AcpToolStatus = "completed"
	AcpToolStatusFailed     AcpToolStatus = "failed"
)

// AcpToolStatusFromNative normalizes opencode's native tool-status
// strings to ACP names ("running" -> "in_progress", "error" ->
// "failed"); unknown values map to pending (the safe default the
// inline translateToolStatus used — UI renders "working").
func AcpToolStatusFromNative(s string) AcpToolStatus {
	switch s {
	case "pending":
		return AcpToolStatusPending
	case "running":
		return AcpToolStatusInProgress
	case "completed":
		return AcpToolStatusCompleted
	case "error":
		return AcpToolStatusFailed
	default:
		return AcpToolStatusPending
	}
}

// ToContractStatus maps an ACP status to the contract ToolStatus 1:1.
func (s AcpToolStatus) ToContractStatus() session.ToolStatus {
	switch s {
	case AcpToolStatusInProgress:
		return session.ToolStatusRunning
	case AcpToolStatusCompleted:
		return session.ToolStatusCompleted
	case AcpToolStatusFailed:
		return session.ToolStatusError
	default:
		return session.ToolStatusPending
	}
}

// AcpToolKindFromName infers the ACP kind from a tool name — the
// kind table. todowrite pins to "other" (never "plan": opencode emits
// todos as generic tool calls, design 0063 §3.2). Unknown names map to
// "other" (the ACP catch-all), never dropped.
func AcpToolKindFromName(name string) AcpToolKind {
	switch strings.ToLower(name) {
	case "bash", "shell", "terminal", "exec":
		return AcpToolKindExecute
	case "edit", "write", "apply":
		return AcpToolKindEdit
	case "read", "glob", "list", "ls":
		return AcpToolKindRead
	case "grep", "search", "find":
		return AcpToolKindSearch
	case "delete", "remove", "rm":
		return AcpToolKindDelete
	case "move", "rename", "mv":
		return AcpToolKindMove
	case "webfetch", "fetch", "curl", "wget":
		return AcpToolKindFetch
	case "todowrite", "todo", "task", "plan_enter", "subagent", "spawn":
		return AcpToolKindOther
	default:
		return AcpToolKindOther
	}
}

// AcpLocation is one toolCall location (path + optional line).
type AcpLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// AcpToolContentKind discriminates toolCall content entries.
type AcpToolContentKind string

const (
	AcpToolContentContent  AcpToolContentKind = "content"
	AcpToolContentDiff     AcpToolContentKind = "diff"
	AcpToolContentTerminal AcpToolContentKind = "terminal"
)

// AcpToolContent is one toolCall content entry (content|diff|terminal).
// Diff entries carry old/new text; on edit-existing operations they are
// granular hunks, on creates absent entirely (the permission request is
// the only diff carrier there) — design 0063 §3.3.
type AcpToolContent struct {
	Kind    AcpToolContentKind `json:"type"`
	Path    string             `json:"path,omitempty"`
	OldText string             `json:"oldText,omitempty"`
	NewText string             `json:"newText,omitempty"`
	Text    string             `json:"text,omitempty"` // content/terminal variants
}

// AcpToolCall is the toolCall shape shared by tool_call and
// tool_call_update (ACP discriminator fields; RawInput/RawOutput are
// pre-extracted JSON like the contract's ToolPart fields).
type AcpToolCall struct {
	ToolCallID  string           `json:"toolCallId"`
	Title       string           `json:"title,omitempty"`
	Kind        AcpToolKind      `json:"kind,omitempty"`
	Status      AcpToolStatus    `json:"status,omitempty"`
	Error       string           `json:"error,omitempty"`
	Locations   []AcpLocation    `json:"locations,omitempty"`
	RawInput    json.RawMessage  `json:"rawInput,omitempty"`
	RawOutput   json.RawMessage  `json:"rawOutput,omitempty"`
	Content     []AcpToolContent `json:"content,omitempty"`
	StartedAt   *time.Time       `json:"startedAt,omitempty"`
	CompletedAt *time.Time       `json:"completedAt,omitempty"`
}

// AcpCommand is one available-commands entry.
type AcpCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AcpUsage is the usage_update payload (session-level context gauge;
// NEVER a Part — design 0063 §3.2 maps it to ContextUsage wiring).
type AcpUsage struct {
	Used int64           `json:"used"`
	Size int64           `json:"size"`
	Cost *session.Cost   `json:"cost,omitempty"`
	Raw  json.RawMessage `json:"-"`
}

// AcpPlanEntry is one plan entry (the never-emitted variant).
type AcpPlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"` // high|medium|low
	Status   string `json:"status,omitempty"`   // pending|in_progress|completed
}

// AcpUpdate is one sessionUpdate event in the internal vocabulary.
// Exactly one payload field is meaningful per Kind.
type AcpUpdate struct {
	Kind      AcpUpdateKind   `json:"sessionUpdate"`
	MessageID string          `json:"messageId,omitempty"`
	Content   AcpContentBlock `json:"content,omitempty"`
	ToolCall  *AcpToolCall    `json:"toolCall,omitempty"`
	Commands  []AcpCommand    `json:"availableCommands,omitempty"`
	Usage     *AcpUsage       `json:"usage,omitempty"`
	Plan      []AcpPlanEntry  `json:"entries,omitempty"`
}

// ToPart maps one update to the contract Part per design 0063 §4.
// ok=false means the update is not a Part (usage_update — session-level)
// or carries no renderable payload. Unknown kinds map to
// Custom(kind "acp.unknown") and are never dropped.
func (u AcpUpdate) ToPart() (session.Part, bool) {
	switch u.Kind {
	case AcpUpdateAgentMessageChunk, AcpUpdateUserMessageChunk:
		if u.Content.Text == "" {
			return session.Part{}, false
		}
		return session.Part{Type: session.PartText, Text: u.Content.Text}, true
	case AcpUpdateAgentThoughtChunk:
		if u.Content.Text == "" {
			return session.Part{}, false
		}
		return session.Part{Type: session.PartReasoning, Reasoning: u.Content.Text}, true
	case AcpUpdateToolCall, AcpUpdateToolCallUpdate:
		if u.ToolCall == nil {
			return session.Part{}, false
		}
		return session.Part{Type: session.PartTool, Tool: u.ToolCall.ToToolPart()}, true
	case AcpUpdateAvailableCommands:
		data, err := json.Marshal(u.Commands)
		if err != nil {
			return session.Part{}, false
		}
		return session.Part{Type: session.PartCustom, Custom: &session.CustomPart{Kind: "acp.available_commands", Data: data}}, true
	case AcpUpdatePlan:
		// Never emitted by opencode 1.18.x (todowrite is a generic
		// tool_call); mapped for forward-compat to the Tool row —
		// 0049 discipline: no PartPlan, todos/plan are tool output.
		data, err := json.Marshal(u.Plan)
		if err != nil {
			return session.Part{}, false
		}
		return session.Part{Type: session.PartTool, Tool: &session.ToolPart{
			Name:  "plan",
			Input: data,
			State: session.ToolState{Status: session.ToolStatusCompleted},
		}}, true
	case AcpUpdateUsage:
		return session.Part{}, false // session-level gauge, never a Part
	default:
		// Tolerant-forward (design 0063 §8): unknown kinds become
		// Custom(acp.unknown) — the stream is never silently dropped.
		data, _ := json.Marshal(string(u.Kind))
		return session.Part{Type: session.PartCustom, Custom: &session.CustomPart{Kind: "acp.unknown", Data: data}}, true
	}
}

// ToToolPart maps the toolCall state machine to the contract ToolPart.
// Status defaults to pending; Title carries the tool name (the ACP
// field opencode fills with the tool name or the command line).
func (tc *AcpToolCall) ToToolPart() *session.ToolPart {
	if tc == nil {
		return nil
	}
	status := tc.Status
	if status == "" {
		status = AcpToolStatusPending
	}
	return &session.ToolPart{
		CallID: tc.ToolCallID,
		Name:   tc.Title,
		Input:  tc.RawInput,
		Output: tc.RawOutput,
		State: session.ToolState{
			Status:      status.ToContractStatus(),
			Error:       tc.Error,
			StartedAt:   tc.StartedAt,
			CompletedAt: tc.CompletedAt,
		},
	}
}

// FileChange converts the first diff content entry to the contract
// FileDiff (unified patch from old->new). Returns nil when the call
// carries no diff (creates never do — design 0063 §3.3).
func (tc *AcpToolCall) FileChange() *session.FileDiff {
	if tc == nil {
		return nil
	}
	for _, c := range tc.Content {
		if c.Kind != AcpToolContentDiff {
			continue
		}
		return &session.FileDiff{
			Path:   c.Path,
			Status: session.ChangeModified,
			Patch:  unifiedPatch(c.OldText, c.NewText),
		}
	}
	return nil
}

// AcpUpdateFromPart maps a contract Part back to the vocabulary (the
// reverse direction; used by consumers that must emit ACP-shaped
// events from contract state). Text maps to agent_message_chunk —
// NEVER user_message_chunk (replay-only pin: that kind has no producer
// in either direction here; it is ingest-only from fork/load replay).
func AcpUpdateFromPart(p session.Part) (AcpUpdate, bool) {
	switch p.Type {
	case session.PartText:
		if p.Text == "" {
			return AcpUpdate{}, false
		}
		return AcpUpdate{Kind: AcpUpdateAgentMessageChunk, Content: AcpContentBlock{Type: "text", Text: p.Text}}, true
	case session.PartReasoning:
		if p.Reasoning == "" {
			return AcpUpdate{}, false
		}
		return AcpUpdate{Kind: AcpUpdateAgentThoughtChunk, Content: AcpContentBlock{Type: "text", Text: p.Reasoning}}, true
	case session.PartTool:
		if p.Tool == nil {
			return AcpUpdate{}, false
		}
		status := AcpToolStatusPending
		switch p.Tool.State.Status {
		case session.ToolStatusRunning:
			status = AcpToolStatusInProgress
		case session.ToolStatusCompleted:
			status = AcpToolStatusCompleted
		case session.ToolStatusError:
			status = AcpToolStatusFailed
		}
		return AcpUpdate{Kind: AcpUpdateToolCall, ToolCall: &AcpToolCall{
			ToolCallID:  p.Tool.CallID,
			Title:       p.Tool.Name,
			Kind:        AcpToolKindFromName(p.Tool.Name),
			Status:      status,
			Error:       p.Tool.State.Error,
			RawInput:    p.Tool.Input,
			RawOutput:   p.Tool.Output,
			StartedAt:   p.Tool.State.StartedAt,
			CompletedAt: p.Tool.State.CompletedAt,
		}}, true
	default:
		return AcpUpdate{}, false
	}
}

// AcpToolCallFromNative converts a native ocTool (the opencode history/
// SSE dialect) to the vocabulary shape. This is the Stage A input seam
// for the tool event class: status strings normalize to ACP names, the
// kind is inferred from the tool name (todowrite -> "other"), and times
// ride through unchanged.
func AcpToolCallFromNative(t *ocTool) *AcpToolCall {
	if t == nil {
		return nil
	}
	tc := &AcpToolCall{
		ToolCallID: t.CallID,
		Title:      t.Name,
		Kind:       AcpToolKindFromName(t.Name),
		Status:     AcpToolStatusPending,
	}
	if t.State != nil {
		tc.Status = AcpToolStatusFromNative(t.State.Status)
		tc.Error = t.State.Error
		tc.StartedAt = t.State.StartedAt
		tc.CompletedAt = t.State.CompletedAt
	}
	if len(t.Input) > 0 {
		tc.RawInput = t.Input
	}
	if len(t.Output) > 0 {
		tc.RawOutput = t.Output
	}
	return tc
}

// unifiedPatch renders a minimal unified diff for an old->new text
// replacement (one hunk, full content). Deterministic; used only for
// vocabulary-carried diffs — the authoritative FileChange patches for
// history remain the filediff producer's (git-based).
func unifiedPatch(oldText, newText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--- old\n+++ new\n")
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", countLines(oldText), countLines(newText))
	for _, l := range splitLines(oldText) {
		if l != "" {
			b.WriteString("-" + l + "\n")
		}
	}
	for _, l := range splitLines(newText) {
		if l != "" {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

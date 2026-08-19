// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session defines the platform-owned agent session contract.
//
// It is the single surface that web, mobile, SDK, and MCP clients consume,
// and the single output shape every agent adapter (pkg/agent/<name>) must
// produce. It contains zero agent-specific identifiers: agent-specific ID
// prefixes, snapshot patch parts, and provider naming live entirely behind
// the adapter seam. See design/0049_2026-08-09_agent-session-contract.md.
//
// Discipline (load-bearing, design 0049 §4.1 + §7):
//   - 5 part types forever (Text, Reasoning, Tool, FileChange, Custom).
//   - All tools are ToolPart discriminated by Name.
//   - Diffs are unified-diff text (Patch string), not hunk structs.
//   - Cost is display-only and never billing.
//   - Agent-specific operations are pass-through until a second adapter
//     validates a typed shape.
package session

import "time"

// Status is the lifecycle state of an agent session.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusIdle       Status = "idle"
	StatusBusy       Status = "busy"
	StatusError      Status = "error"
	StatusCompacting Status = "compacting"
	StatusArchived   Status = "archived"
)

// Session is the platform-owned view of one agent session. It is the unit a
// client lists, opens, and renders. Agent/model switches and compaction are
// carried as Message transcript entries, not side-band fields here.
type Session struct {
	ID           string        `json:"id"`
	WorkspaceID  string        `json:"workspaceId"`
	ParentID     string        `json:"parentId,omitempty"`
	Title        string        `json:"title,omitempty"`
	AgentID      string        `json:"agentId,omitempty"`
	Model        *ModelRef     `json:"model,omitempty"`
	Status       Status        `json:"status"`
	Cost         *Cost         `json:"cost,omitempty"`
	ContextUsage *ContextUsage `json:"contextUsage,omitempty"`
	Time         *TimeRange    `json:"time,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	Archived     bool          `json:"archived,omitempty"`
}

// TimeRange bounds a session or message. CompletedAt is nil while busy.
type TimeRange struct {
	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// Cost is display-only token/cost data. Billing is cgroup-based; these fields
// are never authoritative for metering (design 0049 §4.1 rule 5).
type Cost struct {
	InputTokens      int64   `json:"inputTokens,omitempty"`
	OutputTokens     int64   `json:"outputTokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoningTokens,omitempty"`
	CacheReadTokens  int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64   `json:"cacheWriteTokens,omitempty"`
	TotalTokens      int64   `json:"totalTokens,omitempty"`
	CostUSD          float64 `json:"costUsd,omitempty"`
}

// ContextUsage is the session's live context occupancy — the numerator for
// the "context: 45% used" display that ModelInfo.ContextWindow denominates.
// Used is semantic (tokens of window currently occupied), computed by the
// adapter from the agent's own accounting conventions; raw token ledgers
// live in Cost. Non-monotonic by design: compaction resets it.
type ContextUsage struct {
	Used   int64 `json:"used"`
	Window int64 `json:"window,omitempty"`
}

// ModelRef identifies a model an adapter selected for a session or message.
type ModelRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

// ModelInfo describes a model the client may select, including the context
// and output limits needed for "context: 45% used" display.
type ModelInfo struct {
	ID            string `json:"id"`
	Provider      string `json:"provider,omitempty"`
	DisplayName   string `json:"displayName,omitempty"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxOutput     int64  `json:"maxOutput,omitempty"`
}

// Capability advertises an optional agent behavior. Clients render or hide
// affordances based on the set an adapter reports (design 0049 §4.2/§4.6).
// Agent-specific operations (rewind/fork/stash) are pass-through until a
// second adapter validates a typed result shape.
type Capability string

const (
	CapSteer     Capability = "steer"
	CapQueue     Capability = "queue"
	CapRewind    Capability = "rewind"
	CapFork      Capability = "fork"
	CapStash     Capability = "stash"
	CapDiff      Capability = "diff"
	CapReasoning Capability = "reasoning"
)

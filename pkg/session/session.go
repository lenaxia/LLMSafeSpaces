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
//
// The contract types themselves (Status, Session, Message, Part, Event,
// InputRequest, ...) are GENERATED from the frozen ABI schema into
// contract_gen.go (ADR 0056 T3, issue #1161): the schema is the single
// source of truth; this package is its dialect-preserving Go view that the
// API's JSON egress serves until the S3 frontend cutover. Only the pieces
// with no schema counterpart (ModelInfo, Capability) and the Message
// constructors (message.go) / send options (event.go) are hand-written.
package session

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

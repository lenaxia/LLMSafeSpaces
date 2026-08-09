// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"time"
)

// PartType discriminates the closed part union. The union is capped at 5
// forever (design 0049 §4.1 rule 1); adding a type is a contract change.
type PartType string

const (
	PartText       PartType = "text"
	PartReasoning  PartType = "reasoning"
	PartTool       PartType = "tool"
	PartFileChange PartType = "file_change"
	PartCustom     PartType = "custom"
)

// Part is one typed piece of an assistant message. Exactly one payload field
// is set, matching Type; the rest are omitted from the wire form.
type Part struct {
	Type       PartType    `json:"type"`
	ID         string      `json:"id,omitempty"`
	Text       string      `json:"text,omitempty"`
	Reasoning  string      `json:"reasoning,omitempty"`
	Tool       *ToolPart   `json:"tool,omitempty"`
	FileChange *FileDiff   `json:"fileChange,omitempty"`
	Custom     *CustomPart `json:"custom,omitempty"`
}

// ToolStatus is the tool-call state-machine value (design 0049 §4.3):
// pending -> running -> completed | error.
type ToolStatus string

const (
	ToolStatusPending   ToolStatus = "pending"
	ToolStatusRunning   ToolStatus = "running"
	ToolStatusCompleted ToolStatus = "completed"
	ToolStatusError     ToolStatus = "error"
)

// ToolState is the lifecycle state of one tool call, separate from the call's
// identity and input/output.
type ToolState struct {
	Status      ToolStatus `json:"status"`
	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// ToolPart is the payload of a Tool part. Every tool call — bash, edit, read,
// grep, todos, plan mode, subagent spawn — is a ToolPart discriminated by Name
// (design 0049 §4.1 rule 2). Input/Output are raw JSON because tool schemas are
// open-ended; adapters and renderers decode them per Name.
type ToolPart struct {
	CallID string          `json:"callId,omitempty"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	State  ToolState       `json:"state"`
}

// ChangeStatus classifies one file's change in a FileDiff.
type ChangeStatus string

const (
	ChangeAdded    ChangeStatus = "added"
	ChangeModified ChangeStatus = "modified"
	ChangeDeleted  ChangeStatus = "deleted"
	ChangeRenamed  ChangeStatus = "renamed"
)

// FileDiff is the payload of a FileChange part: one file's unified diff. Patch
// is authoritative unified-diff text (design 0049 §4.1 rule 4) — renderers
// (GitHub, monaco-diff, terminal) all consume it directly; no hunk structs.
type FileDiff struct {
	Path      string       `json:"path"`
	OldPath   string       `json:"oldPath,omitempty"`
	Status    ChangeStatus `json:"status"`
	Patch     string       `json:"patch"`
	Additions int          `json:"additions,omitempty"`
	Deletions int          `json:"deletions,omitempty"`
}

// CustomPart is the pressure-relief valve for extension-defined semantics
// (design 0049 §4.3). Kind is a required discriminator so extensions do not
// force new PartType constants; Data carries the extension-specific payload.
type CustomPart struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import "time"

// WSConnection represents a WebSocket connection
type WSConnection interface {
	// ReadMessage reads a message from the connection
	ReadMessage() (messageType int, p []byte, err error)

	// WriteMessage writes a message to the connection
	WriteMessage(messageType int, data []byte) error

	// Close closes the connection
	Close() error
}

// Session represents a WebSocket session
type Session struct {
	// Session ID
	ID string

	// User ID
	UserID string

	// Workspace ID
	WorkspaceID string

	// WebSocket connection
	Conn WSConnection

	// Creation time
	CreatedAt time.Time
}

type Message struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// SessionStatusItem describes a session reported by the workspace agent.
type SessionStatusItem struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
	ContextUsed int64  `json:"contextUsed"`
}

// SessionListItem is sidebar metadata for a session (NOT message bodies).
//
// ParentID, when non-empty, is the session_id of the user-visible parent
// session. Populated for subagent/subtask sessions spawned by the agent
// during a turn. The sidebar nests children under their parent for
// navigation. NULL/empty means the session is top-level.
//
// ContextUsed, when non-nil, is the prompt token count from the last
// completed step event persisted by the API proxy. nil means no LLM
// step has completed yet for this session (distinguishable from 0).
type SessionListItem struct {
	ID            string     `json:"id"`
	Title         string     `json:"title,omitempty"`
	ParentID      string     `json:"parentId,omitempty"`
	LastMessageAt *time.Time `json:"lastMessageAt,omitempty"`
	MessageCount  int        `json:"messageCount"`
	Status        string     `json:"status"` // "active" | "idle"
	LastSeenAt    *time.Time `json:"lastSeenAt,omitempty"`
	HasUnread     bool       `json:"hasUnread"`
	ContextUsed   *int64     `json:"contextUsed,omitempty"`
	Origin        string     `json:"origin,omitempty"` // manual | routine | workflow | api
}

// ActiveSessionsResponse is returned by GET /workspaces/:id/sessions/active.
type ActiveSessionsResponse struct {
	Active    []string `json:"active"`
	MaxActive int      `json:"maxActive"`
}

// EnsureSessionResponse is returned by POST /workspaces/:id/sessions/new.
// It guarantees the workspace is active with a running pod, returning the
// workspace ID and session ID for immediate use.
type EnsureSessionResponse struct {
	WorkspaceID    string `json:"workspaceId"`
	WorkspacePhase string `json:"workspacePhase"`
	SessionID      string `json:"sessionId"`
	Resumed        bool   `json:"resumed"`
}

type CredentialStateResult struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

type AgentHealthResult struct {
	Status              string   `json:"status"`
	ProvidersConfigured int      `json:"providersConfigured"`
	AgentVersion        string   `json:"agentVersion,omitempty"`
	Connected           []string `json:"connected,omitempty"`
	Message             string   `json:"message,omitempty"`
	LastCheckedAt       string   `json:"lastCheckedAt,omitempty"`
}

// WorkspaceConditionResult carries a single workspace condition.
type WorkspaceConditionResult struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

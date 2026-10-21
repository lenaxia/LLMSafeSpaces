// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import "time"

// HeaderSessionRefResolved is the #1816 resolved-id bridge header: the
// session-ref middleware sets it (value = the canonical harness id)
// whenever a caller-addressed session ref was RESOLVED — phrase or
// alias rewrite, never a byte-identical passthrough. The platform MCP
// client (pkg/mcp) consumes it as one key of its dual-space contract
// stream match: post-#1824, frames are phrase-keyed on refreshed pods
// (harness additive as internal_session_id) and harness-keyed on
// heal-window/legacy pods — the client admits either. ONE definition,
// imported by both sides of the seam — a rename must redden the
// bridge continuity test, not silently break phrase sends. Retires
// with a phrase-only ABI.
const HeaderSessionRefResolved = "X-Session-Ref-Resolved"

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
	// ID is THE identifier on the wire (#1756): the platform-minted
	// 3-word phrase. During the backfill window a row whose phrase is
	// not yet minted degrades to the harness id here (converged by
	// the reconcile sweep's registration pass).
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// ParentID is the PARENT's phrase (#1756 spec §1: full ids leave
	// default output); InternalParentID carries the harness key the
	// platform joins on.
	ParentID      string     `json:"parentId,omitempty"`
	LastMessageAt *time.Time `json:"lastMessageAt,omitempty"`
	MessageCount  int        `json:"messageCount"`
	Status        string     `json:"status"` // "active" | "idle"
	LastSeenAt    *time.Time `json:"lastSeenAt,omitempty"`
	HasUnread     bool       `json:"hasUnread"`
	ContextUsed   *int64     `json:"contextUsed,omitempty"`
	Origin        string     `json:"origin,omitempty"` // manual | routine | workflow | api
	// InternalID is the harness session id — the platform's internal
	// key (reconcile, counts, active-status, backfill joins). Never
	// serialized (#1756 spec §3: internal key unchanged, external
	// surface short-only).
	InternalID string `json:"-"`
	// InternalParentID is the parent's harness id (internal join key).
	InternalParentID string `json:"-"`
	// ShortID is the minted phrase — internal mirror of ID kept for
	// the mint/backfill machinery; not serialized (ID carries it).
	ShortID string `json:"-"`
	// Archived is the platform-level read-only marker (#1627). ABSENT
	// means NOT archived — the flag is omitempty for backward
	// compatibility with every existing client.
	Archived bool `json:"archived,omitempty"`
	// Workspace carries the owning workspace id on cross-workspace
	// suggestion candidates only (design 0066 §4.6) — internal, never
	// serialized on the per-workspace list this type serves.
	Workspace string `json:"-"`
}

// ActiveSessionsResponse is returned by GET /workspaces/:id/sessions/active.
type ActiveSessionsResponse struct {
	Active    []string `json:"active"`
	MaxActive int      `json:"maxActive"`
}

// SessionAlert is one persisted D6 (#998) escalation. Rows are appended
// by the API-side escalation sweep each time a notify-only
// workspace.alert (session_hung) is published, so alerts survive SSE
// disconnects and remain readable by workflow surfaces after the fact.
type SessionAlert struct {
	ID                string    `json:"id"`
	WorkspaceID       string    `json:"workspaceId"`
	SessionID         string    `json:"sessionId"`
	Alert             string    `json:"alert"`
	OldestBusySeconds int       `json:"oldestBusySeconds"`
	CreatedAt         time.Time `json:"createdAt"`
	// ResolvedAt is null while the alert's condition is still live.
	// Writers (all server-side): the D6 sweep observing the hang end,
	// the Active→non-Active watch event for an alerted workspace, and
	// the read-side heal aging a lost resolution past its trust window.
	// The feed stays append-only history; consumers read CURRENT
	// condition as "session_hung with ResolvedAt == nil" instead of
	// reconstructing it from busy snapshots client-side.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

// EnsureSessionResponse is returned by POST /workspaces/:id/sessions/new.
// It guarantees the workspace is active with a running pod, returning the
// workspace ID and session ID for immediate use.
type EnsureSessionResponse struct {
	WorkspaceID    string `json:"workspaceId"`
	WorkspacePhase string `json:"workspacePhase"`
	// SessionID is THE identifier (#1756): the minted 3-word phrase.
	// The harness full id leaves default output; degraded window
	// (registration seam unavailable) falls back to it here with the
	// deprecation notice set.
	SessionID string `json:"sessionId"`
	Resumed   bool   `json:"resumed"`
	// DeprecatedSessionID carries the harness full id ONLY when the
	// response had to degrade to it (seam unavailable) — the
	// one-release input-deprecation notice travels with it (#1756 §2).
	DeprecatedSessionID string `json:"deprecatedSessionId,omitempty"`
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
	// Warnings are boot-time degradation notices relayed from agentd
	// (e.g. the default model could not be resolved — incident
	// 2026-08-16). Present with Status "Healthy": the agent is fine but
	// running degraded, and the user must see why. Structured form of
	// the "; warnings: ..." condition-message suffix.
	Warnings []string `json:"warnings,omitempty"`
}

// WorkspaceConditionResult carries a single workspace condition.
type WorkspaceConditionResult struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// SupervisorStatusResult is the supervisor/watchdog agent-restart slice
// of the workspace status DTO (#1760). AgentRestarts is a lifetime
// count since the pod's agentd boot (supervisor-watchdog restarts plus
// supervisor-attributed process-crash restarts — reason crash/oom — in
// both container topologies) — never a resetting crash-loop
// counter.
type SupervisorStatusResult struct {
	AgentRestarts int `json:"agentRestarts"`
	// LastAgentRestart: attribution of the most recent recorded restart;
	// nil when none was recorded this boot.
	LastAgentRestart *AgentRestartEventResult `json:"lastAgentRestart,omitempty"`
	// SuppressedRestarts: total would-fire watchdog restarts suppressed
	// by corroborating evidence (the agent was alive) since agentd boot.
	SuppressedRestarts int `json:"suppressedRestarts"`
	// LastSuppressedVerdict: the verdict of the most recent suppression
	// (hung | starved | flat | respawn | unknown); empty when none.
	LastSuppressedVerdict string `json:"lastSuppressedVerdict,omitempty"`
}

// AgentRestartEventResult is one recorded agent restart (#1760).
// Reason is the closed reason set (health_watchdog |
// health_watchdog_unknown_episode | crash | oom); Verdict is the
// supervisor watchdog's classification (hung | starved | flat |
// respawn | unknown) — empty when the restart was not
// watchdog-initiated or was uncorroborated.
type AgentRestartEventResult struct {
	Reason    string    `json:"reason"`
	Verdict   string    `json:"verdict,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	At        time.Time `json:"at"`
}

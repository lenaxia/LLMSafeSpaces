// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Adapter is the single seam between platform code (proxy handlers,
// MCP server, services) and one agent runtime. It folds the existing
// AgentRuntime + Dialect + AgentClient + AgentConfigWriter interfaces
// into one surface and adds the session/messaging methods the proxy
// migrates to under US-65.4.
//
// Per design 0049 §4.6, the full Adapter has ~18 methods. This file
// defines the interface shape; the opencode implementation lands in
// pkg/agent/opencode/adapter.go (US-65.3 follow-up). Platform code
// holds an Adapter (interface) and never imports an implementation.
//
// Composition: AgentConfigWriter (US-65.1) is embedded because its
// two methods (Apply, HasRelay) are conceptually part of Adapter. The
// other three existing interfaces (AgentRuntime, Dialect, AgentClient)
// are NOT embedded — they carry agent-specific shapes (opencode path
// strings, map[string]any config patches, []byte raw model lists) that
// Adapter's purpose is to eliminate. Their methods become private to
// the opencode adapter; platform code calls the platform-shaped
// methods below.
//
// Rule 12 (containment before abstraction): pass-through operations
// (Rewind, Fork) are NOT included until a second adapter validates
// their shape or a forcing UX need lands one.
type Adapter interface {
	AgentConfigWriter

	// --- Sessions (design 0049 §4.6) ---
	//
	// Platform-shaped session.Session values; the adapter keeps the
	// agent-specific ID mapping (e.g. opencode's ses_ prefix) internal.

	// CreateSession creates a new session in the agent and returns the
	// platform view. The adapter generates or translates the ID.
	CreateSession(ctx context.Context, userID, workspaceID, title string) (*session.Session, error)

	// GetSession returns the current state of one session.
	GetSession(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error)

	// ListSessions returns the platform view of all sessions on the
	// workspace, ordered by recency.
	ListSessions(ctx context.Context, userID, workspaceID string) ([]session.Session, error)

	// RenameSession updates a session's title.
	RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error

	// DeleteSession removes a session from the agent. Subsequent Get
	// calls return a not-found error.
	DeleteSession(ctx context.Context, userID, workspaceID, sessionID string) error

	// --- Messaging (design 0049 §4.6) ---

	// Send delivers a user message synchronously and returns the
	// assistant's completed response. Streaming callers use Stream
	// instead; Send is for request-response callers (MCP, SDK).
	Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error)

	// SendAsync delivers a user message without waiting for the
	// assistant response. The returned message ID lets the caller
	// correlate via Stream or GetHistory. Use this for long-running
	// tasks where the caller will poll.
	SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (messageID string, err error)

	// Abort stops any in-flight work on the session. The abort is
	// non-destructive: queued user input survives and runs on the
	// next wake (design 0049 §3 — loss of revoke is accepted).
	Abort(ctx context.Context, userID, workspaceID, sessionID string) error

	// GetHistory returns the session transcript in platform shape.
	// The adapter drops agent-specific parts (e.g. opencode's patch
	// part) and produces FileChange parts from git diff where the
	// agent reported file changes.
	GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error)

	// --- Streaming / Input (design 0049 §4.6) ---

	// Stream subscribes to the session's event stream, translating
	// agent-specific events to session.Event values. The channel
	// closes when the context is canceled or the agent ends the
	// stream. Errors during translation are emitted as session.EventError
	// events, not returned across the channel as Go errors.
	Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error)

	// ListPending returns the currently-blocking InputRequests on a
	// session (questions or permissions). The adapter unifies the
	// agent's question/permission shapes into session.InputRequest.
	ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error)

	// Resolve settles a pending InputRequest with the user's reply.
	// For questions, reply carries the selected option(s) or custom
	// text. For permissions, reply carries "allow" / "deny" (the
	// adapter translates to the agent's accept/reject endpoints).
	Resolve(ctx context.Context, userID, workspaceID, requestID, reply string) error

	// --- Models (design 0049 §4.6) ---

	// ListAvailableModels returns the catalog of models the agent can
	// select, including context/output limits for "context: X% used"
	// display. The adapter converts agent-side cost/relay/provider
	// data into the platform ModelInfo shape.
	ListAvailableModels(ctx context.Context, userID, workspaceID string) ([]session.ModelInfo, error)

	// SetModel changes the session's active model. Subsequent Send
	// calls use the new model.
	SetModel(ctx context.Context, userID, workspaceID, sessionID string, model session.ModelRef) error

	// --- Capabilities ---

	// Capabilities reports the optional agent behaviors the client
	// may render affordances for (steer, queue, rewind, fork, stash,
	// diff, reasoning). The set is per-adapter, not per-session.
	Capabilities() []session.Capability

	// --- Credentials (folded from AgentRuntime, US-65.3) ---
	//
	// These methods replace the existing AgentRuntime's loose-typed
	// equivalents. The adapter owns the agent-specific credential
	// format and validation rules.

	// FormatProviderConfig renders the supplied providers in the
	// agent's native config format. Returns nil if the agent has no
	// provider config concept for these credentials.
	FormatProviderConfig(providers []LLMProviderData) ([]byte, error)

	// ValidateCredentials checks whether the supplied raw config
	// (already agent-formatted) is structurally valid. Returns nil
	// on success, an error describing the validation failure
	// otherwise. Used at bind time to reject malformed credentials
	// before they reach the workspace.
	ValidateCredentials(rawConfig []byte) (*CredentialCheckResult, error)
}

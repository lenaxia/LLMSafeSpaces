// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Adapter is the single seam between platform code (proxy handlers,
// MCP server, services) and one agent runtime. It folds the existing
// AgentRuntime + Dialect + AgentClient interface shapes into one
// surface and adds the session/messaging methods the proxy migrates
// to under US-65.4.
//
// Per design 0049 §4.6, the full Adapter has ~18 methods. This file
// defines the interface shape; the opencode implementation lives in
// pkg/agent/opencode/adapter.go. Platform code holds an Adapter
// (interface) and never imports an implementation.
//
// AgentConfigWriter is NOT embedded here. The two seams run in
// different processes: AgentConfigWriter is held by agentd (the
// in-pod supervisor that writes agent-config.json); Adapter is held
// by the API server (proxy handlers that translate HTTP calls). The
// API pod has no filesystem access to the workspace PVC and cannot
// write agent config. Composing the two into one interface would
// force every Adapter implementation to provide panic-stubs for
// Apply/HasRelay — code that lies about its capabilities. design 0049
// §4.6's "folds AgentConfigWriter" described the long-term intent
// (one unified seam per agent); the as-built architecture has two
// seams because the two processes have different capabilities.
//
// Rule 12 (containment before abstraction): pass-through operations
// (Rewind, Fork) are NOT included until a second adapter validates
// their shape or a forcing UX need lands one.
type Adapter interface {
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

	// SetSessionModel sets the session's default model. Under agents
	// whose async-delivery endpoint strips per-prompt model overrides
	// (opencode V2, verified 2026-08-29), this is the ONLY way a
	// message's model selection reaches the agent — the delivery path
	// applies it to the session before admission. On the interface (not
	// a local assertion) per the #1127 lesson: production adapters are
	// wrapped, and seam assertions fail silently through wrappers.
	// Adapters without session-model support return nil (no-op) rather
	// than failing the delivery.
	SetSessionModel(ctx context.Context, userID, workspaceID, sessionID string, m *session.ModelRef) error

	// VerifyDelivery reports whether a user message whose text equals
	// `text` was persisted in the agent's transcript at or after
	// `since`. delivered=true proves the agent holds it;
	// definitive=true with delivered=false proves absence (a re-send
	// cannot duplicate); both false means inconclusive — recheck, never
	// re-send. This is the #987 ambiguity oracle and the #1119
	// promotion oracle.
	//
	// Promoted from a handlers-local type assertion to the interface by
	// the 2026-08-29 incident: production adapters are WRAPPED
	// (systemnotices.Wrap embeds this interface), and the handlers'
	// deliveryVerifier assertion silently failed against the wrapper —
	// every verification returned inconclusive without touching the
	// network, breaking all V2 delivery. The wrapper was the "second
	// consumer" the old comment said would fund this promotion.
	// Adapters without transcript verification return the inconclusive
	// shape (false, false, nil).
	VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (delivered, definitive bool, err error)

	// GetHistory returns the session transcript in platform shape.
	// The adapter drops agent-specific parts (e.g. opencode's patch
	// part) and produces FileChange parts from git diff where the
	// agent reported file changes.
	GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error)

	// GetHistoryPage returns the NEWEST `limit` messages of a session,
	// oldest-first within the page, using the agent's native pagination
	// when it has one (#971: fetching+decoding the full transcript to
	// slice a page was ~95% of session-load latency on large sessions).
	// Implementations without native pagination may return the full
	// transcript; callers slice.
	GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error)

	// --- Streaming / Input (design 0049 §4.6) ---

	// Stream subscribes to the session's event stream, translating
	// agent-specific events to session.Event values. The channel
	// closes when the context is canceled or the agent ends the
	// stream. Unknown/malformed events are dropped silently.
	// Connection-level errors (scanner failure) are emitted as
	// session.EventError events before the channel closes.
	Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error)

	// ListPending returns the currently-blocking InputRequests on a
	// session (questions or permissions). The adapter unifies the
	// agent's question/permission shapes into session.InputRequest.
	//
	// Contract: a non-nil error means the pending set is UNKNOWN
	// (agent unreachable, endpoint error) — never treat it as an
	// authoritative empty. A 404-not-implemented endpoint yields an
	// authoritative empty with a nil error. Callers that broadcast
	// snapshots (the SSE input snapshot) must mark failed fetches as
	// non-authoritative so they cannot wipe live pending prompts.
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

	// ContextUsageFromEvent translates one raw agent SSE event into the
	// session's live context occupancy. ok=false when the event carries
	// no usage signal (most events don't); callers skip silently — that
	// is normal traffic, not drift. When ok=true, sessionID is non-empty
	// and usage is non-nil (implementations must uphold this; the SSE
	// event path dereferences both). The adapter owns both the wire
	// shapes and the semantic mapping to ContextUsage.Used; platform
	// code never inspects the raw bytes.
	ContextUsageFromEvent(eventType string, rawData string) (sessionID string, usage *session.ContextUsage, ok bool)

	// ClientEventsFromEvent translates one raw agent SSE event into
	// zero or more CONTRACT events (pkg/session) for client
	// consumption — the US-65.8 bridge. Clients render contract shapes
	// only; agent wire names, envelopes, and part shapes stay behind
	// this seam. nil for events carrying no client-facing signal (the
	// majority — implementations must type-check before parsing so the
	// streaming hot path never pays a parse for them).
	ClientEventsFromEvent(eventType string, rawData string) []session.Event

	// RetryFromEvent decodes an agent backoff/retry notification into
	// the platform retry shape. Retry detail is UI-shaped and rides the
	// platform session.status SSE event rather than the contract.
	RetryFromEvent(eventType string, rawData string) (sessionID string, retry *ClientRetryStatus, ok bool)

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

// SessionUsage is session-level CUMULATIVE token usage and model
// attribution decoded from one agent event — the metering signal.
// Counters only grow within a session; delta computation is caller
// policy. CostMalformed reports a cost field that decoded to 0 because
// its shape was unrecognized — billing proceeds at 0 but the caller
// should warn (wire-drift signal).
type SessionUsage struct {
	SessionID     string
	ModelID       string
	ProviderID    string
	InputTokens   int64
	OutputTokens  int64
	CostUSD       float64
	CostMalformed bool
}

// ClientRetryStatus is the platform retry/backoff signal decoded from
// an agent event (US-65.8). UI-shaped by nature: NextAt is epoch millis
// for the client's countdown; Action is the agent's follow-up verb.
type ClientRetryStatus struct {
	Attempt int
	Message string
	NextAt  int64
	Action  string
}

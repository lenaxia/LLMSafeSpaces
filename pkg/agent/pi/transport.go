// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// transport.go — the seam between the API-server Adapter and one
// workspace's pi runtime. The Adapter (adapter.go) is constructed by
// platform wiring with a Transport; every byte of pi RPC wire knowledge
// (command names, response envelope, event records, session-file JSONL)
// stays inside this package, so the production Transport implementation
// (S5 harness-selection plumbing, bridging to agentd's supervise-pi
// supervisor) is a dumb pipe: it carries bytes and spawn/remove/list
// operations, and this package parses.
//
// Process model (pi-scoping report §1.4, S2 ruling): pi RPC mode is a
// single-conversation process. The platform drives one child per
// session, pinned at launch via --session-id (the supervisor's
// process-per-session policy). Session() is therefore the per-session
// attach point; the supervisor spawns-or-resumes on first touch.
//
// pi has no list-sessions RPC at all ("sessions are found by scanning
// the session dir", session-format.md) and deletion is a file unlink —
// ListSessionFiles / OpenSessionFile / RemoveSessionFile model that
// file-level surface directly, which is also why VerifyDelivery and
// CountMessages answer while pi is down: the persisted .jsonl IS the
// store.
//
// The fixture transport in fixture_test.go implements this interface
// over recorded transcripts (testdata/), which is what the contract
// suite runs against — the recorded-fixture-transport stance of
// pkg/abi/abitest (a reference implementation the generated tests run
// against) applied to the pi seam.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Transport is the per-workspace pi runtime seam. All methods take the
// platform identity pair; implementations resolve it to the workspace's
// pod/supervisor (the opencode adapter's PodIPResolver role, carried by
// the transport instead of the adapter because pi has no URL to build).
type Transport interface {
	// Session resolves the RPC command surface for one existing
	// session id (process-per-session: attach spawns-or-resumes the
	// child pinned to the id). A session whose .jsonl is absent from
	// the session dir surfaces an error wrapping agent.ErrSessionNotFound.
	Session(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error)

	// CreateSession spawns a NEW session with the caller-supplied id
	// (the platform mints the id; pi pins it via --session-id — "Opens
	// the current directory's existing session with the ID, or creates
	// it otherwise", cli.md). Callers must validate the id first
	// (validateSessionID).
	CreateSession(ctx context.Context, userID, workspaceID, sessionID string) (SessionClient, error)

	// Control resolves a sessionless RPC surface (a --no-session
	// child) for workspace-level commands that address no session —
	// get_available_models. Implementations may pool one control
	// child per workspace.
	Control(ctx context.Context, userID, workspaceID string) (SessionClient, error)

	// ListSessionFiles lists the persisted session files (the
	// no-list-sessions-RPC answer: the session-dir scan). Order is
	// unspecified; the adapter sorts by recency.
	ListSessionFiles(ctx context.Context, userID, workspaceID string) ([]SessionFileMeta, error)

	// OpenSessionFile opens one persisted .jsonl for reading. This is
	// the VerifyDelivery / CountMessages / title-scan path and works
	// while pi is down. An absent session file surfaces an error
	// wrapping agent.ErrSessionNotFound.
	OpenSessionFile(ctx context.Context, userID, workspaceID, sessionID string) (io.ReadCloser, error)

	// RemoveSessionFile unlinks one persisted .jsonl ("Sessions can be
	// removed by deleting their .jsonl files", session-format.md) and
	// detaches any live child pinned to it.
	RemoveSessionFile(ctx context.Context, userID, workspaceID, sessionID string) error
}

// SessionFileMeta is one persisted session file as the dir scan sees it.
type SessionFileMeta struct {
	// ID is the session id parsed from the file name
	// (<timestamp>_<session-id>.jsonl; the id is everything after the
	// first '_' — ids may contain '_', timestamps never do).
	ID string
	// Name is the base file name.
	Name string
	// ModifiedAt is the file mtime (the recency signal for ListSessions).
	ModifiedAt time.Time
	// SizeBytes is the file size.
	SizeBytes int64
}

// EventRecord is one item off a session's live event stream. Raw is one
// complete JSONL record (stdout line, LF stripped); Err is set once on
// a terminal stream failure, after which the channel closes — the
// Stream scanner-error semantics of the opencode SSE path.
type EventRecord struct {
	Raw []byte
	Err error
}

// SessionClient is one pi session's RPC surface: id-correlated commands
// in, the session's event records out.
type SessionClient interface {
	// Call issues one RPC command and returns the full response record
	// (the {type:"response", command, success, error, data} envelope —
	// data stays inside it; call() in this package decodes the
	// envelope). A success:false response surfaces as an error carrying
	// the response's error string; implementations wrap session-gone
	// phrasings with errPiSessionGone (agent.ErrSessionNotFound).
	Call(ctx context.Context, command map[string]any) (json.RawMessage, error)

	// Events subscribes to the session's live event records from the
	// child's stdout. Subscribing BEFORE issuing a prompt is the
	// documented order ("Subscribe before sending a prompt to avoid
	// missing a fast completion", rpc.md); implementations must
	// support multiple simultaneous subscribers (fan-out) and close
	// the channel when ctx ends.
	Events(ctx context.Context) (<-chan EventRecord, error)
}

// errPiSessionGone marks a pi-side report that the session no longer
// exists. Callers classify with errors.Is(err, agent.ErrSessionNotFound).
var errPiSessionGone = fmt.Errorf("pi session gone: %w", agent.ErrSessionNotFound)

// piCallError is a success:false command response.
type piCallError struct {
	command string
	message string
}

func (e *piCallError) Error() string {
	return fmt.Sprintf("pi command %s failed: %s", e.command, e.message)
}

// piSessionIDCharset is pi's --session-id constraint: "letters, numbers,
// `.`, `_`, `-`" (cli.md). The platform surface is stricter (no dots) to
// keep ids path-safe for every transport implementation.
const piSessionIDCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// validateSessionID guards every id that reaches a Transport (they
// become file names and spawn args). Same posture as the opencode
// adapter's pre-URL-interpolation guard.
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("empty session id")
	}
	if len(sessionID) > 128 {
		return fmt.Errorf("session id too long (max 128): %d bytes", len(sessionID))
	}
	for _, r := range sessionID {
		if !strings.ContainsRune(piSessionIDCharset, r) {
			return fmt.Errorf("session id %q contains character %q outside pi's charset %q",
				sessionID, string(r), piSessionIDCharset)
		}
	}
	return nil
}

// --- Command builders -------------------------------------------------
//
// Commands are map[string]any literals (the S2 supervisor's precedent:
// pi's RPC has no schema worth typing on the write side; the read side —
// responses and events — IS typed below and in events.go). Builders
// centralize the field names so no call site can drift a key.

func cmdPrompt(message string, behavior string, images []piImage) map[string]any {
	cmd := map[string]any{"type": "prompt", "message": message}
	if behavior != "" {
		cmd["streamingBehavior"] = behavior
	}
	if len(images) > 0 {
		cmd["images"] = images
	}
	return cmd
}

func cmdSteer(message string) map[string]any {
	return map[string]any{"type": "steer", "message": message}
}

func cmdFollowUp(message string) map[string]any {
	return map[string]any{"type": "follow_up", "message": message}
}

func cmdAbort() map[string]any      { return map[string]any{"type": "abort"} }
func cmdClearQueue() map[string]any { return map[string]any{"type": "clear_queue"} }
func cmdGetState() map[string]any   { return map[string]any{"type": "get_state"} }
func cmdGetStats() map[string]any   { return map[string]any{"type": "get_session_stats"} }
func cmdGetEntries(since string) map[string]any {
	if since == "" {
		return map[string]any{"type": "get_entries"}
	}
	return map[string]any{"type": "get_entries", "since": since}
}

func cmdSetSessionName(name string) map[string]any {
	return map[string]any{"type": "set_session_name", "name": name}
}

func cmdSetModel(provider, modelID string) map[string]any {
	return map[string]any{"type": "set_model", "provider": provider, "modelId": modelID}
}

func cmdGetAvailableModels() map[string]any {
	return map[string]any{"type": "get_available_models"}
}

// piImage is pi's ImageContent (message-types.md) — the Send attachment
// path's wire shape.
type piImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// --- Response envelope --------------------------------------------------

// piDisposition is prompt/steer/follow_up's acceptance verdict
// (rpc-commands.md): started | queued | handled.
type piDisposition string

const (
	dispositionStarted piDisposition = "started"
	dispositionQueued  piDisposition = "queued"
	dispositionHandled piDisposition = "handled"
)

// piCallResult is the decoded envelope common to every command response.
// Data stays raw; per-command decodes live beside their consumers.
type piCallResult struct {
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// call issues a command and decodes the shared envelope. A nil-data
// success is legal (abort, set_session_name, ...).
func call(ctx context.Context, c SessionClient, command map[string]any) (json.RawMessage, error) {
	raw, err := c.Call(ctx, command)
	if err != nil {
		if errors.Is(err, errPiSessionGone) {
			return nil, errPiSessionGone
		}
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	res := piCallResult{}
	if err := json.Unmarshal(raw, &res); err != nil {
		// Tolerate a bare (non-envelope) payload: some transports
		// hand back the data object directly.
		return raw, nil
	}
	if !res.Success {
		msg := res.Error
		if msg == "" {
			msg = "unknown error"
		}
		if isSessionGoneMessage(msg) {
			return nil, errPiSessionGone
		}
		return nil, &piCallError{command: commandName(command), message: msg}
	}
	return res.Data, nil
}

func commandName(command map[string]any) string {
	if v, ok := command["type"].(string); ok {
		return v
	}
	return "?"
}

// isSessionGoneMessage recognizes pi's not-found phrasings so the typed
// gone sentinel (#1479 reconciliation input) survives the stringly
// response surface.
func isSessionGoneMessage(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "session not found") ||
		strings.Contains(m, "no such session") ||
		strings.Contains(m, "session file not found")
}

// decodeInto decodes a response's data into dst (tolerating absent data).
func decodeInto(data json.RawMessage, dst any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}

// --- Typed response payloads -------------------------------------------

// piStateData is get_state's data (rpc-commands.md #get_state). Fields
// the adapter does not consume (steeringMode, followUpMode,
// autoCompactionEnabled, pendingMessageCount) are tolerated silently —
// defensive decode, unknown fields never rejected (§7 posture; pi has
// no protocol handshake, so a newer binary adding fields must not
// break this reader).
type piStateData struct {
	Model        *piModelObject `json:"model,omitempty"`
	Thinking     string         `json:"thinkingLevel,omitempty"`
	IsStreaming  bool           `json:"isStreaming"`
	IsCompacting bool           `json:"isCompacting"`
	SessionFile  string         `json:"sessionFile,omitempty"`
	SessionID    string         `json:"sessionId,omitempty"`
	SessionName  string         `json:"sessionName,omitempty"`
	MessageCount int            `json:"messageCount"`
}

// piStatsData is get_session_stats' data (rpc-commands.md
// #get_session_stats). contextUsage is omitted when no model/window is
// available and its tokens/percent are null right after compaction —
// hence pointers throughout.
type piStatsData struct {
	SessionID         string          `json:"sessionId,omitempty"`
	UserMessages      int             `json:"userMessages"`
	AssistantMessages int             `json:"assistantMessages"`
	ToolCalls         int             `json:"toolCalls"`
	ToolResults       int             `json:"toolResults"`
	TotalMessages     int             `json:"totalMessages"`
	Tokens            *piUsage        `json:"tokens,omitempty"`
	Cost              float64         `json:"cost"`
	ContextUsage      *piContextUsage `json:"contextUsage,omitempty"`
	SessionFile       string          `json:"sessionFile,omitempty"`
}

type piContextUsage struct {
	Tokens        *int64 `json:"tokens"`
	ContextWindow *int64 `json:"contextWindow"`
	Percent       *int64 `json:"percent"`
}

// piModelObject is the full Model object every model command returns
// (rpc-commands.md #model-object). Only the fields the platform ModelInfo
// needs are modeled.
type piModelObject struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	API           string   `json:"api,omitempty"`
	Provider      string   `json:"provider,omitempty"`
	BaseURL       string   `json:"baseUrl,omitempty"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input,omitempty"`
	ContextWindow int64    `json:"contextWindow,omitempty"`
	MaxTokens     int64    `json:"maxTokens,omitempty"`
}

// piModelsData is get_available_models' data.
type piModelsData struct {
	Models []piModelObject `json:"models"`
}

// piDispositionData is prompt/steer/follow_up's data.
type piDispositionData struct {
	Disposition piDisposition `json:"disposition"`
}

// piQueueData is clear_queue's data: the drained queue contents that
// Abort re-queues (the queued-input-survives dance).
type piQueueData struct {
	Steering []string `json:"steering"`
	FollowUp []string `json:"followUp"`
}

// piEntriesData is get_entries' data. leafId is null for an empty
// session; a since that matches no entry id is a success:false response
// (handled in history.go by full-refetching).
type piEntriesData struct {
	Entries []piEntry `json:"entries"`
	LeafID  *string   `json:"leafId"`
}

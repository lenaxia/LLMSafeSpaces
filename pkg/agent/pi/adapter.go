// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// adapter.go — the API-server-side agent.Adapter for pi
// (earendil-works/pi, RPC mode). The full interface (pkg/agent
// adapter.go) minus the declared gaps:
//
//   - ListPending / AnswerQuestion / ReplyPermission / RejectInput: pi
//     has no built-in permission system ("Pi does not include a
//     built-in permission system for restricting filesystem, process,
//     network, or credential access", README) — the §1.5 scoping
//     ruling: NO ask/permission affordances (Capabilities advertises
//     none; ListPending is an authoritative empty; the reply methods
//     are typed not-supported). The sandbox is the boundary.
//   - No instance-dispose: credential rotation is restart-based
//     (matches the restartRequired flow; models.json `!command` keys
//     are the hot-reload exception, owned by the ConfigWriter seam).
//
// Construction (S5 wiring): NewAdapter(transport, logger). Platform
// wiring builds the Transport per workspace (the registry-driven
// construction story) and hands the adapter to systemnotices.Wrap,
// which stays harness-neutral (it embeds the interface).
//
// Sessions map 1:1 onto pi child processes (the process-per-session
// ruling, §1.4): the platform mints the id, the transport pins the
// child via --session-id, and the persisted .jsonl is the store —
// which is why CountMessages, VerifyDelivery and ListSessions answer
// from the file while pi is down.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"go.uber.org/zap"
)

// ErrInputNotSupported is the typed verdict for the ask/permission
// surface pi cannot carry (§1.5 ruling): callers that reach it despite
// Capabilities advertising no ask affordances get a definitive
// not-supported, never a hang or a silent no-op.
var ErrInputNotSupported = errors.New("pi: no in-band question/permission surface (permission-less harness; sandbox is the boundary)")

// Adapter is the pi implementation of agent.Adapter.
type Adapter struct {
	transport Transport
	logger    *zap.Logger
	runtime   *piAgentRuntime

	ringsMu sync.Mutex
	rings   map[ringKey]*historyRing
}

// Compile-time interface assertion — the same posture as the opencode
// seam (and the systemnotices wrapper type-asserts nothing: it embeds).
var _ agent.Adapter = (*Adapter)(nil)

// NewAdapter constructs the pi adapter over one workspace transport
// seam. The transport is per-workspace by construction (S5 wiring);
// sharing one adapter across workspaces is legal only when the
// transport multiplexes them.
func NewAdapter(t Transport, logger *zap.Logger) *Adapter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Adapter{
		transport: t,
		logger:    logger,
		runtime:   &piAgentRuntime{},
		rings:     map[ringKey]*historyRing{},
	}
}

// --- Sessions -----------------------------------------------------------

// CreateSession mints the platform id (pi's charset: letters, numbers,
// `.`, `_`, `-` — the platform uses the UUID-safe subset), spawns the
// pinned child via the transport, and sets the display name.
func (a *Adapter) CreateSession(ctx context.Context, userID, workspaceID, title string) (*session.Session, error) {
	id := newSessionID()
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	c, err := a.transport.CreateSession(ctx, userID, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if title != "" {
		if _, err := call(ctx, c, cmdSetSessionName(title)); err != nil {
			// Name-setting is best-effort at create: the session
			// exists, the title can be re-set via RenameSession.
			a.logger.Warn("pi create session: set name failed", zap.String("session", id), zap.Error(err))
		}
	}
	st, err := a.state(ctx, c)
	if err != nil {
		return nil, err
	}
	// The child is pinned to the minted id (--session-id); trust it
	// over the state echo (a stale/disagreeing echo cannot redefine
	// the platform's session identity).
	st.SessionID = id
	return sessionFromState(st, workspaceID, title), nil
}

// GetSession answers from get_state (the live child) — the platform
// view of one session.
func (a *Adapter) GetSession(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	st, err := a.state(ctx, c)
	if err != nil {
		return nil, err
	}
	s := sessionFromState(st, workspaceID, st.SessionName)
	// Context usage is best-effort (stats are a second RPC; omission
	// is normal right after compaction — tokens/percent go null).
	if stats, serr := a.stats(ctx, c); serr == nil && stats != nil && stats.ContextUsage != nil {
		s.ContextUsage = contextUsageFromStats(stats.ContextUsage)
	}
	return s, nil
}

// ListSessions scans the session dir (pi has no list-sessions RPC;
// "sessions are found by scanning the session dir", session-format.md)
// and derives each view from the persisted file — works while pi is
// down. A session with a live streaming child is busy by definition
// (work happens only in a child); the file scan cannot see streaming
// state, so rows report idle unless the transport knows better — the
// design-0055 authority for busy state is the supervisor's statusz,
// not this list.
func (a *Adapter) ListSessions(ctx context.Context, userID, workspaceID string) ([]session.Session, error) {
	metas, err := a.transport.ListSessionFiles(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(metas))
	for _, meta := range metas {
		id := meta.ID
		if id == "" {
			id = sessionIDFromName(meta.Name)
		}
		if id == "" {
			continue
		}
		if err := validateSessionID(id); err != nil {
			continue
		}
		s := session.Session{
			ID:          id,
			WorkspaceID: workspaceID,
			Status:      session.StatusIdle,
		}
		if !meta.ModifiedAt.IsZero() {
			t := meta.ModifiedAt.UTC()
			s.Time = &session.TimeRange{StartedAt: t}
		}
		// Title/created need the file body: stream it. A file that
		// vanishes mid-scan (deleted between list and open) is skipped,
		// not fatal.
		rc, err := a.transport.OpenSessionFile(ctx, userID, workspaceID, id)
		if err != nil {
			if errors.Is(err, agent.ErrSessionNotFound) {
				continue
			}
			a.logger.Warn("pi list sessions: open failed", zap.String("session", id), zap.Error(err))
			out = append(out, s)
			continue
		}
		sum, err := scanSessionFile(rc)
		_ = rc.Close()
		if err != nil && sum != nil && len(sum.Entries) == 0 {
			// Read died before any entry: keep the row shell.
			a.logger.Warn("pi list sessions: scan failed", zap.String("session", id), zap.Error(err))
			out = append(out, s)
			continue
		}
		s.Title = sum.title()
		if !sum.CreatedAt.IsZero() && s.Time == nil {
			s.Time = &session.TimeRange{StartedAt: sum.CreatedAt}
		}
		out = append(out, s)
	}
	// Recency order (newest first) per the interface contract.
	sortSessionsByRecency(out)
	return out, nil
}

// CountMessages answers from the persisted file — the #1481 ground
// truth, cheaper than opencode's page walk and available while pi is
// down (§5.3: "CountMessages can be answered by file scan").
func (a *Adapter) CountMessages(ctx context.Context, userID, workspaceID, sessionID string) (int, error) {
	msgs, err := a.fileTranscript(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// RenameSession is set_session_name (the name also lands in the
// session file as a session_info entry and surfaces in listings).
func (a *Adapter) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return err
	}
	_, err = call(ctx, c, cmdSetSessionName(title))
	return err
}

// DeleteSession is the file unlink ("Sessions can be removed by
// deleting their .jsonl files", session-format.md); the transport also
// detaches any live child. A 404-class verdict is the definitive reap
// signal (agent.ErrSessionNotFound, #1479).
func (a *Adapter) DeleteSession(ctx context.Context, userID, workspaceID, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	if err := a.transport.RemoveSessionFile(ctx, userID, workspaceID, sessionID); err != nil {
		if errors.Is(err, agent.ErrSessionNotFound) {
			return err
		}
		return err
	}
	a.ringsMu.Lock()
	delete(a.rings, ringKey{workspace: workspaceID, session: sessionID})
	a.ringsMu.Unlock()
	return nil
}

// --- Messaging ----------------------------------------------------------

// Send delivers a user message and waits for the session-level run to
// settle. Admission maps onto pi's queue/steer surface 1:1
// (V2Delivery queue/steer ⇔ follow_up/steer): the default send is
// `prompt`; opts.Admission=steer is the `steer` command; =queue is
// `follow_up`. A prompt accepted while streaming needs
// streamingBehavior, so the default send always sets it from the
// admission too (prompt during streaming without it is an error).
func (a *Adapter) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	if opts.Model != nil {
		if err := a.setSessionModel(ctx, c, opts.Model); err != nil {
			return nil, err
		}
	}

	// Subscribe BEFORE prompting (rpc.md: "Subscribe before sending a
	// prompt to avoid missing a fast completion").
	events, err := c.Events(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := a.deliver(ctx, c, text, opts)
	if err != nil {
		return nil, err
	}
	if resp == dispositionHandled {
		// An extension/input handler consumed the prompt; no run
		// starts ("If it is handled, no run started for this prompt,
		// so don't wait for agent_settled", rpc.md). The handler's own
		// side effects (pi.sendMessage) surface in history.
		return handledMessage(), nil
	}

	// Wait for settle: agent_end alone is NOT completion (retries,
	// compaction recovery, steering and queued messages may follow) —
	// agent_settled is the only idle transition (json.md).
	settled, aborted, streamErr := waitForSettle(ctx, events)
	if streamErr != nil && !settled {
		return nil, fmt.Errorf("pi send: stream: %w", streamErr)
	}

	// The authoritative final message comes off the active branch —
	// message_end events carry it too, but the settle gate plus one
	// get_entries delta is the durable read (and refreshes the ring).
	page, err := a.historyPage(ctx, c, workspaceID, sessionID, 1)
	if err != nil {
		return nil, err
	}
	if len(page) == 0 {
		if aborted {
			return nil, fmt.Errorf("pi send: aborted before any assistant response")
		}
		return handledMessage(), nil
	}
	last := page[len(page)-1]
	if last.Type != session.MessageAssistant {
		// The newest transcript entry is not an assistant message
		// (steer consumed mid-run, abort, ...) — surface it verbatim;
		// callers correlate by stream position for detail.
		return &last, nil
	}
	if aborted {
		if last.Error == nil {
			last.Error = &session.Error{Code: "aborted", Message: "run aborted"}
		}
	}
	return &last, nil
}

// SendAsync delivers without waiting: the disposition response is the
// acceptance verdict; the correlation id is the persisted user entry's
// id when the run started (fetched via the durable cursor — the
// #987/#1119 re-check path), else "" (queued messages persist only on
// delivery).
func (a *Adapter) SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return "", err
	}
	if opts.Model != nil {
		if err := a.setSessionModel(ctx, c, opts.Model); err != nil {
			return "", err
		}
	}
	resp, err := a.deliver(ctx, c, text, opts)
	if err != nil {
		return "", err
	}
	if resp != dispositionStarted {
		return "", nil
	}
	// Best-effort correlation id: the newest user entry at/after the
	// run start. Failure returns "" — delivery already succeeded.
	id := newestUserEntryID(ctx, c, text)
	return id, nil
}

// Abort preserves the platform's queued-input-survives contract
// (adapter.go: non-destructive abort; queued input survives and runs
// on the NEXT wake). pi's abort alone diverges: "abort continues
// queued messages when they remain in the session" (rpc-commands.md),
// which would run the queue immediately. The pi-documented dance
// (its own Esc behavior) is clear_queue → abort → restore; the
// adapter restores by re-queueing each drained message as a follow_up
// — delivered only when the agent next runs, exactly the contract.
func (a *Adapter) Abort(ctx context.Context, userID, workspaceID, sessionID string) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return err
	}
	var drained []string
	if data, qerr := call(ctx, c, cmdClearQueue()); qerr == nil {
		q := piQueueData{}
		if decodeInto(data, &q) == nil {
			drained = append(drained, q.Steering...)
			drained = append(drained, q.FollowUp...)
		}
	}
	// clear_queue failure is tolerated: an abort must still stop the
	// in-flight work (the queue re-queue below is then skipped — a
	// queue pi keeps may still run, the documented pi-side divergence).
	if _, aerr := call(ctx, c, cmdAbort()); aerr != nil {
		if len(drained) > 0 {
			// Abort failed after draining: restore the queue rather
			// than dropping the user's input on a failed command.
			a.requeue(ctx, c, drained)
		}
		return aerr
	}
	a.requeue(ctx, c, drained)
	return nil
}

func (a *Adapter) requeue(ctx context.Context, c SessionClient, texts []string) {
	for _, t := range texts {
		if _, err := call(ctx, c, cmdFollowUp(t)); err != nil {
			a.logger.Warn("pi abort: requeue failed", zap.Error(err))
		}
	}
}

// deliver sends the text with the admission-mapped command and returns
// the disposition.
func (a *Adapter) deliver(ctx context.Context, c SessionClient, text string, opts session.SendOpts) (piDisposition, error) {
	var (
		cmd     map[string]any
		wantCmd = true
	)
	switch opts.Admission {
	case session.AdmissionSteer:
		cmd = cmdSteer(text)
	case session.AdmissionQueue:
		cmd = cmdFollowUp(text)
	default:
		cmd = cmdPrompt(text, "followUp", nil)
		wantCmd = false
	}
	data, err := call(ctx, c, cmd)
	if err != nil {
		return "", err
	}
	d := piDispositionData{}
	if err := decodeInto(data, &d); err != nil || d.Disposition == "" {
		if wantCmd {
			return "", fmt.Errorf("pi %s: response carried no disposition", commandName(cmd))
		}
		return dispositionStarted, nil
	}
	return d.Disposition, nil
}

// SetSessionModel is set_model — under pi the session IS the model
// holder (one active session per child), so this and SetModel are the
// same call.
func (a *Adapter) SetSessionModel(ctx context.Context, userID, workspaceID, sessionID string, m *session.ModelRef) error {
	if err := validateSessionID(sessionID); err != nil {
		return err
	}
	if m == nil || (m.ID == "" && m.Provider == "") {
		return nil // no-op per the interface contract
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return err
	}
	return a.setSessionModel(ctx, c, m)
}

func (a *Adapter) setSessionModel(ctx context.Context, c SessionClient, m *session.ModelRef) error {
	provider, modelID, ok := modelRefFromSplit(m)
	if !ok {
		return fmt.Errorf("pi set_model: model ref %q has no provider (need provider/model or provider+id)", m.ID)
	}
	if _, err := call(ctx, c, cmdSetModel(provider, modelID)); err != nil {
		return err
	}
	return nil
}

// VerifyDelivery is the #987 ambiguity oracle and #1119 promotion
// oracle, answered by scanning the persisted .jsonl — definitive both
// ways and available while pi is down (the file IS the store).
func (a *Adapter) VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (bool, bool, error) {
	if err := validateSessionID(sessionID); err != nil {
		return false, false, err
	}
	rc, err := a.transport.OpenSessionFile(ctx, userID, workspaceID, sessionID)
	if err != nil {
		if errors.Is(err, agent.ErrSessionNotFound) {
			// A gone session is a definitive absence for delivery
			// purposes (a re-send cannot duplicate onto a session that
			// no longer exists).
			return false, true, nil
		}
		// Unknown state: inconclusive — recheck, never re-send.
		return false, false, err
	}
	defer func() { _ = rc.Close() }()
	return verifyScan(rc, text, entryTimestampFloor(since))
}

// --- History ------------------------------------------------------------

// GetHistory returns the full active-branch transcript (root→leaf).
func (a *Adapter) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	return a.historyPage(ctx, c, workspaceID, sessionID, 0)
}

// GetHistoryPage returns the newest limit messages using the native
// get_entries {since} durable cursor on the steady-state path
// (history.go's replay ring; cold reads take one full fetch).
func (a *Adapter) GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	return a.historyPage(ctx, c, workspaceID, sessionID, limit)
}

// --- Streaming / Input --------------------------------------------------

// Stream subscribes to the session's event records and translates them
// to contract events. Unknown/malformed records are dropped silently;
// a terminal stream failure surfaces as a final error event before the
// channel closes (the scanner-error posture of the opencode SSE path).
func (a *Adapter) Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error) {
	if err := validateSessionID(sessionID); err != nil {
		return nil, err
	}
	c, err := a.transport.Session(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	raw, err := c.Events(ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan session.Event, 32)
	go func() {
		defer close(ch)
		tr := newStreamTranslator()
		for rec := range raw {
			if ctx.Err() != nil {
				return
			}
			if rec.Err != nil {
				e := session.Event{Type: session.EventError, Timestamp: time.Now().UTC(),
					SessionID: sessionID, Error: &session.Error{Code: "stream_error", Message: rec.Err.Error()}}
				select {
				case ch <- e:
				case <-ctx.Done():
				}
				return
			}
			for _, ev := range clientEventsOffRecord(rec.Raw, sessionID, tr) {
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// ListPending is an AUTHORITATIVE empty: pi has no question/permission
// surface at all (§1.5), which is the "endpoint not implemented"
// contract case — nil error, never an unknown-set signal.
func (a *Adapter) ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error) {
	_ = sessionID
	return nil, nil
}

// RejectInput is not-supported: pi has no pending-input surface.
func (a *Adapter) RejectInput(ctx context.Context, userID, workspaceID, requestID string) error {
	return ErrInputNotSupported
}

// AnswerQuestion is not-supported: pi has no pending-input surface.
func (a *Adapter) AnswerQuestion(ctx context.Context, userID, workspaceID, requestID string, answers [][]string) error {
	return ErrInputNotSupported
}

// ReplyPermission is not-supported: pi has no pending-input surface.
func (a *Adapter) ReplyPermission(ctx context.Context, userID, workspaceID, requestID, reply, message string) error {
	return ErrInputNotSupported
}

// --- Models -------------------------------------------------------------

// ListAvailableModels is get_available_models over the sessionless
// control surface (models are workspace config, not per-session state;
// the catalog the control child resolves IS the workspace catalog).
func (a *Adapter) ListAvailableModels(ctx context.Context, userID, workspaceID string) ([]session.ModelInfo, error) {
	c, err := a.transport.Control(ctx, userID, workspaceID)
	if err != nil {
		return nil, err
	}
	data, err := call(ctx, c, cmdGetAvailableModels())
	if err != nil {
		return nil, err
	}
	res := piModelsData{}
	if err := decodeInto(data, &res); err != nil {
		return nil, fmt.Errorf("pi get_available_models: decode: %w", err)
	}
	out := make([]session.ModelInfo, 0, len(res.Models))
	for _, m := range res.Models {
		out = append(out, modelObjectToInfo(m))
	}
	return out, nil
}

// ContextUsageFromEvent implements agent.Adapter: one raw event record
// in, the live occupancy out. ok=false for records without a usage
// signal or without the injected sessionId (see client_events.go).
func (a *Adapter) ContextUsageFromEvent(eventType string, rawData string) (string, *session.ContextUsage, bool) {
	if eventType != "" && eventType != evMessageUpdate {
		return "", nil, false
	}
	return contextUsageFromRecord([]byte(rawData))
}

// ClientEventsFromEvent implements agent.Adapter: one raw event record
// in, zero or more contract events out. nil for records with no
// client-facing signal (the majority — the type probe runs before any
// payload parse).
func (a *Adapter) ClientEventsFromEvent(eventType string, rawData string) []session.Event {
	if eventType != "" && !knownEventTypes[eventType] {
		return nil
	}
	ev, ok := parseEvent([]byte(rawData))
	if !ok {
		return nil
	}
	return clientEvents(ev, "", time.Now().UTC(), newStreamTranslator())
}

// RetryFromEvent implements agent.Adapter: auto_retry_start/end into
// the platform retry shape (UI-shaped, rides the session.status SSE
// path).
func (a *Adapter) RetryFromEvent(eventType string, rawData string) (string, *agent.ClientRetryStatus, bool) {
	if eventType != "" && eventType != evAutoRetryStart && eventType != evAutoRetryEnd {
		return "", nil, false
	}
	return retryFromRecord([]byte(rawData), time.Now().UTC())
}

// SetModel is set_model on the named session (same surface as
// SetSessionModel under the one-active-session-per-child model).
func (a *Adapter) SetModel(ctx context.Context, userID, workspaceID, sessionID string, model session.ModelRef) error {
	return a.SetSessionModel(ctx, userID, workspaceID, sessionID, &model)
}

// --- Capabilities / Credentials -----------------------------------------

// Capabilities advertises the pi-native optional behaviors: steer (the
// steer command), queue (follow_up + the queue_update surface), and
// reasoning (set_thinking_level / thinking_level_changed). Deliberately
// absent: rewind/fork/stash (Rule 12 pass-through posture — pi has
// fork/clone/get_tree, but no platform surface exists until a typed
// shape is validated) and diff (no differ is wired on the API side).
func (a *Adapter) Capabilities() []session.Capability {
	return []session.Capability{session.CapSteer, session.CapQueue, session.CapReasoning}
}

// FormatProviderConfig implements agent.Adapter (delegates to the
// pi AgentRuntime — the credentials seam's single source of truth).
func (a *Adapter) FormatProviderConfig(providers []agent.LLMProviderData) ([]byte, error) {
	return a.runtime.FormatProviderConfig(providers)
}

// ValidateCredentials implements agent.Adapter (delegates to the pi
// AgentRuntime).
func (a *Adapter) ValidateCredentials(rawConfig []byte) (*agent.CredentialCheckResult, error) {
	return a.runtime.ValidateCredentials(rawConfig)
}

// --- Helpers ------------------------------------------------------------

// state issues get_state.
func (a *Adapter) state(ctx context.Context, c SessionClient) (*piStateData, error) {
	data, err := call(ctx, c, cmdGetState())
	if err != nil {
		return nil, err
	}
	st := piStateData{}
	if err := decodeInto(data, &st); err != nil {
		return nil, fmt.Errorf("pi get_state: decode: %w", err)
	}
	return &st, nil
}

// stats issues get_session_stats.
func (a *Adapter) stats(ctx context.Context, c SessionClient) (*piStatsData, error) {
	data, err := call(ctx, c, cmdGetStats())
	if err != nil {
		return nil, err
	}
	st := piStatsData{}
	if err := decodeInto(data, &st); err != nil {
		return nil, fmt.Errorf("pi get_session_stats: decode: %w", err)
	}
	return &st, nil
}

// fileTranscript reads the whole session file and translates the
// active branch (the while-down history/count source).
func (a *Adapter) fileTranscript(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	rc, err := a.transport.OpenSessionFile(ctx, userID, workspaceID, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	sum, err := scanSessionFile(rc)
	if err != nil {
		return nil, err
	}
	return sum.transcriptMessages(), nil
}

// sessionFromState maps get_state data to the platform session view.
// isCompacting wins over isStreaming (compaction is the rarer, more
// specific state; both true means mid-compaction recovery).
func sessionFromState(st *piStateData, workspaceID, fallbackTitle string) *session.Session {
	s := &session.Session{ID: st.SessionID, WorkspaceID: workspaceID}
	s.Title = st.SessionName
	if s.Title == "" {
		s.Title = fallbackTitle
	}
	switch {
	case st.IsCompacting:
		s.Status = session.StatusCompacting
	case st.IsStreaming:
		s.Status = session.StatusBusy
	default:
		s.Status = session.StatusIdle
	}
	if st.Model != nil && st.Model.ID != "" {
		s.Model = &session.ModelRef{ID: st.Model.ID, Provider: st.Model.Provider}
	}
	return s
}

// contextUsageFromStats maps get_session_stats' contextUsage (tokens
// null right after compaction) to the contract shape.
func contextUsageFromStats(cu *piContextUsage) *session.ContextUsage {
	if cu == nil || cu.Tokens == nil || *cu.Tokens <= 0 {
		return nil
	}
	u := &session.ContextUsage{Used: *cu.Tokens}
	if cu.ContextWindow != nil {
		u.Window = *cu.ContextWindow
	}
	return u
}

// waitForSettle scans the subscription until agent_settled (the only
// idle transition) or the stream/channel dies. Returns whether settle
// arrived and whether it carried aborted=true.
func waitForSettle(ctx context.Context, events <-chan EventRecord) (settled, aborted bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, false, ctx.Err()
		case rec, ok := <-events:
			if !ok {
				return false, false, nil
			}
			if rec.Err != nil {
				return false, false, rec.Err
			}
			ev, ok := parseEvent(rec.Raw)
			if !ok {
				continue
			}
			switch ev.Type {
			case evAgentSettled:
				return true, ev.Aborted, nil
			case evAutoRetryStart:
				// Retry backoff is part of the run: keep waiting
				// (agent_end.willRetry is the same signal class).
			}
		}
	}
}

// newestUserEntryID finds the durable entry id of the just-persisted
// user message via the cursor (best-effort; "" when it cannot be
// found — delivery already succeeded).
func newestUserEntryID(ctx context.Context, c SessionClient, text string) string {
	d, err := fetchEntries(ctx, c, "")
	if err != nil {
		return ""
	}
	for i := len(d.Entries) - 1; i >= 0; i-- {
		e := d.Entries[i]
		if e.Message != nil && e.Message.Role == "user" {
			if text == "" || e.Message.textContent() == text {
				return e.ID
			}
		}
	}
	return ""
}

// handledMessage is the Send return for a disposition=handled prompt
// (an extension consumed it; no assistant run). Honest, non-nil, and
// typed system — never a fabricated assistant response.
func handledMessage() *session.Message {
	m := session.SystemMessage("pi-handled", "prompt handled by a pi extension or input handler", nil)
	return &m
}

// clientEventsOffRecord is Stream's per-record entry: parse, stamp the
// session id, translate.
func clientEventsOffRecord(raw []byte, sessionID string, tr *streamTranslator) []session.Event {
	ev, ok := parseEvent(raw)
	if !ok {
		return nil
	}
	return clientEvents(ev, sessionID, time.Now().UTC(), tr)
}

// sortSessionsByRecency orders newest-first (mtime; zero times last).
func sortSessionsByRecency(s []session.Session) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0; j-- {
			a, b := s[j-1].Time, s[j].Time
			at, bt := time.Time{}, time.Time{}
			if a != nil {
				at = a.StartedAt
			}
			if b != nil {
				bt = b.StartedAt
			}
			if bt.IsZero() {
				break
			}
			if at.IsZero() || bt.After(at) {
				s[j-1], s[j] = s[j], s[j-1]
			} else {
				break
			}
		}
	}
}

// newSessionID mints a pi-charset-safe id (UUID v4, the shape pi
// itself mints by default).
func newSessionID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

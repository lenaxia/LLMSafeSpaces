// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// The API's busy-gated ABI consumer (US-69.11): billing from per-step
// MESSAGE_END tokens (deterministic idempotency keys → the DB's unique
// constraint makes multi-replica billing exactly-once), the Epic 28
// user-stream state bridge (source swapped from the retired tracker),
// context/title persistence, and agent-death detection.

// usageStreamGates (US-69.11, the scale-to-zero AC observable): 1 per
// workspace with an armed busy-gated subscription, series deleted when
// the gate drops (Close / idle window) — an idle fleet scrapes empty,
// not a wall of zero series.
var usageStreamGates = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "llmsafespaces_usage_stream_gates",
	Help: "Open busy-gated usage-stream subscriptions per workspace (set on Open, series deleted on close or idle-drop — scale-to-zero).",
}, []string{"workspace_id"})

// recordUsageStreamGate is the Consumer's metrics hook (the package
// stays import-clean; the gauge lives here).
func recordUsageStreamGate(workspaceID string, open bool) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if open {
		usageStreamGates.WithLabelValues(workspaceID).Set(1)
	} else {
		usageStreamGates.DeleteLabelValues(workspaceID)
	}
}

var (
	usageStreamMu   sync.Mutex
	usageStreamOnce sync.Once
	usageStreamPtr  *usagestream.Consumer
)

// UsageStream lazily builds the process-wide consumer (the resolve seam
// re-resolves the pod per (re)connect — resume-safe, A7). The mutex
// guards the singleton swap (tests inject fakes; the race detector
// holds us to it).
func (h *ProxyHandler) UsageStream() *usagestream.Consumer {
	usageStreamMu.Lock()
	defer usageStreamMu.Unlock()
	usageStreamOnce.Do(func() {
		usageStreamPtr = h.buildUsageStream()
	})
	return usageStreamPtr
}

// buildUsageStream is the production wiring (split for the test seam).
func (h *ProxyHandler) buildUsageStream() *usagestream.Consumer {
	return usagestream.New(usagestream.Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			return h.agentdEndpoint(ctx, workspaceID)
		},
		NewClient: func(baseURL, password string) usagestream.Client {
			return newUsageStreamClient(baseURL, password)
		},
		Billing:      usagestream.BillingFunc(h.recordStepUsage),
		Bridge:       &usageBridge{h: h},
		Logger:       h.logger,
		OnGateChange: recordUsageStreamGate,
	})
}

// SetUsageBilling wires the inference metrics + metering sinks (app.go;
// lazy to avoid a service-registry dependency cycle at construction).
func (h *ProxyHandler) SetUsageBilling(inferenceMetrics func(modelID, providerID string, inputTokens, outputTokens int64, costDollars float64), meteringRecorder func(types.UsageEvent)) {
	h.usageBillingMu.Lock()
	h.usageInference = inferenceMetrics
	h.usageMetering = meteringRecorder
	h.usageBillingMu.Unlock()
}

func (h *ProxyHandler) inferenceMetricsSink() (func(string, string, int64, int64, float64), func(types.UsageEvent)) {
	h.usageBillingMu.Lock()
	defer h.usageBillingMu.Unlock()
	return h.usageInference, h.usageMetering
}

// recordStepUsage bills one MESSAGE_END step. The idempotency keys are
// deterministic in (workspace, message, seq): two replicas consuming the
// same pod stream generate the SAME key, and usage_events' unique
// constraint turns the race into an at-most-once insert — fixing the
// tracker's latent double-billing (its keys embedded UnixNano).
func (h *ProxyHandler) recordStepUsage(workspaceID string, u usagestream.Usage) {
	inference, metering := h.inferenceMetricsSink()
	if inference != nil {
		inference(u.ModelID, u.ProviderID, u.InputTokens, u.OutputTokens, u.CostUSD)
	}
	if metering == nil || (u.OutputTokens == 0 && u.InputTokens == 0) {
		return
	}
	ownerID := h.GetWorkspaceOwner(workspaceID)
	if ownerID == "" {
		return
	}
	owner := types.BillingOwner{ID: ownerID, Type: types.OwnerTypeUser}
	meta := map[string]any{"model_id": u.ModelID, "provider_id": u.ProviderID, "session_id": u.SessionID}
	base := fmt.Sprintf("tokens:%s:%s:%d", workspaceID, u.MessageID, u.Seq)
	metering(types.UsageEvent{
		IdempotencyKey: base + ":in",
		Owner:          owner,
		ActorID:        ownerID,
		WorkspaceID:    workspaceID,
		EventType:      "llm_tokens",
		EventSubtype:   "input",
		Quantity:       u.InputTokens,
		Source:         "api",
		EventTime:      time.Now(),
		Metadata:       meta,
	})
	if u.OutputTokens > 0 {
		metering(types.UsageEvent{
			IdempotencyKey: base + ":out",
			Owner:          owner,
			ActorID:        ownerID,
			WorkspaceID:    workspaceID,
			EventType:      "llm_tokens",
			EventSubtype:   "output",
			Quantity:       u.OutputTokens,
			Source:         "api",
			EventTime:      time.Now(),
			Metadata:       meta,
		})
	}
}

// usageBridge adapts the consumer's derived state onto the platform's
// surviving surfaces: the Epic 28 user-stream events (same wire shapes
// the tracker's translations emitted — the frontend provider consumes
// these unchanged), session-index persistence, and agent death.
type usageBridge struct {
	h *ProxyHandler

	// kindsMu remembers the kind of every input request seen on this
	// replica so resolved events carry the right wire type
	// (agent.question.resolved vs agent.permission.resolved). Requests
	// first seen by another replica degrade to question.resolved — the
	// frontend removes by request id either way.
	kindsMu sync.Mutex
	kinds   map[string]abiv1.InputKind
}

func (b *usageBridge) rememberKind(id string, kind abiv1.InputKind) {
	b.kindsMu.Lock()
	defer b.kindsMu.Unlock()
	if b.kinds == nil {
		b.kinds = map[string]abiv1.InputKind{}
	}
	b.kinds[id] = kind
}

func (b *usageBridge) kindOf(id string) abiv1.InputKind {
	b.kindsMu.Lock()
	defer b.kindsMu.Unlock()
	return b.kinds[id]
}

func (b *usageBridge) SessionStatus(workspaceID, sessionID string, busy bool) {
	b.SessionStatusWithInternal(workspaceID, sessionID, "", busy)
}

// SessionStatusWithInternal is the #1817 piece-3 seam: the consumer passes
// the ADDITIVE harness id (agentd's internal_session_id) so the bridge's
// translation can detect an already-phrase-keyed frame and avoid the
// double-translation phantom row. A harness-keyed frame (heal window /
// legacy) has an empty internal and still translates normally.
func (b *usageBridge) SessionStatusWithInternal(workspaceID, sessionID, internalID string, busy bool) {
	if b.h.userBroker == nil {
		return
	}
	status := "idle"
	if busy {
		status = "busy"
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		// #1817/#1821 piece 1: the wire id is the PHRASE (the id space
		// the frontend joins on — session list, history, chat); the
		// harness id rides along ADDITIVELY as internal_session_id.
		// Degraded index (no phrase) passes the harness id through —
		// never worse than the pre-fix behavior.
		// #1817 piece 3: if the frame already carries an internal id (a
		// refreshed agentd put the phrase on the wire), seed it onto the
		// event so translateSessionIDToPhrase's guard sees it and does not
		// re-register the phrase (the phantom-row defect).
		var data map[string]any
		if internalID != "" {
			data = map[string]any{"internal_session_id": internalID}
		}
		evt := apitypes.WorkspaceSSEEvent{
			Type:        "session.status",
			WorkspaceID: workspaceID,
			SessionID:   sessionID,
			Status:      status,
			Data:        data,
		}
		b.h.translateSessionIDToPhrase(context.Background(), workspaceID, &evt)
		b.h.userBroker.PublishToUser(userID, evt)
	}
}

func (b *usageBridge) InputRequested(workspaceID string, req *abiv1.InputRequest) {
	if b.h.userBroker == nil || req == nil {
		return
	}
	b.rememberKind(req.GetId(), req.GetKind())
	// Headless mode: auto-approve answers permissions server-side — the
	// user stream must not prompt a human for an input the platform is
	// about to resolve (the retired dialect path suppressed these too).
	if req.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION && b.h.shouldAutoApprovePermissions(context.Background(), workspaceID) {
		go b.h.autoApprovePermission(workspaceID, req.GetId())
		return
	}
	// #1313: every surfaced ask is recorded in the inbox — the live ask
	// stays just the doorbell; the record survives its death.
	b.h.recordInboxAsk(context.Background(), workspaceID, req)
	root := req.GetRootSessionId()
	if root == "" {
		root = b.h.resolveRootSessionID(workspaceID, req.GetSessionId())
	}
	// 4a-2 (r1 f1): the ONE contract shape — the ABI InputRequest is
	// served verbatim (camelCase tags, kind-discriminated); the legacy
	// envelope expansion is retired with its converters.
	evt := apitypes.WorkspaceSSEEvent{
		Type:      "agent.question",
		SessionID: req.GetSessionId(),
		RequestID: req.GetId(),
		Data:      inputRequestFromABI(req, root),
	}
	if req.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION {
		evt.Type = "agent.permission"
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
		b.h.translateSessionIDToPhrase(context.Background(), workspaceID, &evt)
		b.h.userBroker.PublishToUser(userID, evt)
	}
}

func (b *usageBridge) InputResolved(workspaceID, sessionID, inputID string) {
	b.InputResolvedWithInternal(workspaceID, sessionID, "", inputID)
}

// InputResolvedWithInternal is the #1817 piece-3 additive seam (finding 2):
// the consumer passes the harness id (agentd's internal_session_id) so the
// translation guard detects an already-translated frame (SessionID=phrase)
// and does NOT re-register the phrase (the phantom-row defect that left the
// resolved event targeting a third id, so the frontend never removed the
// prompt). A harness-keyed frame (legacy) has an empty internal and still
// translates normally.
func (b *usageBridge) InputResolvedWithInternal(workspaceID, sessionID, internalID, inputID string) {
	if b.h.userBroker == nil {
		return
	}
	resolvedType := "agent.question.resolved"
	if b.kindOf(inputID) == abiv1.InputKind_INPUT_KIND_PERMISSION {
		resolvedType = "agent.permission.resolved"
	}
	data := map[string]string{
		"request_id": inputID,
		"session_id": sessionID,
	}
	// #1817 piece 3: seed the additive harness id so the translation guard
	// sees an already-translated frame and passes the phrase through.
	if internalID != "" {
		data["internal_session_id"] = internalID
	}
	evt := apitypes.WorkspaceSSEEvent{
		Type:      resolvedType,
		SessionID: sessionID,
		RequestID: inputID,
		Data:      data,
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
		b.h.translateSessionIDToPhrase(context.Background(), workspaceID, &evt)
		b.h.userBroker.PublishToUser(userID, evt)
	}
}

func (b *usageBridge) SessionTitle(workspaceID, sessionID, title string) {
	if b.h.sessionIndex == nil || sessionID == "" || b.h.isSessionDeleted(workspaceID, sessionID) {
		return
	}
	b.h.persistSessionMeta(context.Background(), workspaceID, sessionID, title, "")
}

func (b *usageBridge) ContextUsed(workspaceID, sessionID string, used int64) {
	b.ContextUsedWithInternal(workspaceID, sessionID, "", used)
}

// ContextUsedWithInternal is the #1817 piece-3 additive seam (finding 1):
// the context-usage write must key the row against the REAL session
// (the harness id on refreshed pods), not the phrase. Register(phrase)
// mints a phantom short-id row (EnsureSessionRegistered INSERTs on
// session_id, a phrase lives in short_id) and the context occupancy would
// land on the ghost row. When the frame is already translated (internalID
// present), key the Register/UpsertContextUsed by the harness id (the
// internal) so the existing row is updated. A harness-keyed frame (legacy)
// has an empty internal and keys by sessionID as before.
func (b *usageBridge) ContextUsedWithInternal(workspaceID, sessionID, internalID string, used int64) {
	if b.h.sessionIndex == nil || sessionID == "" || used <= 0 {
		return
	}
	// #1817 piece 3: the row is keyed by the session id the index knows
	// (the harness id). On refreshed pods the wire sessionID is the phrase
	// and the harness id rides additively — key the row by it so the
	// existing row is updated, not a minted phantom.
	rowID := internalID
	if rowID == "" {
		rowID = sessionID
	}
	// #1817 piece 3 (r3 finding 2): the delete tombstone is recorded under
	// the HARNESS id (proxy_handlers.go delete path → MarkSessionDeleted
	// with the harness sessionID), not the phrase. A late context-usage
	// event on a refreshed pod carries the phrase on the wire and the
	// harness id additively; guarding only on the phrase (sessionID) would
	// MISS the tombstone, and the Register(rowID=harness) below would
	// RE-CREATE the just-deleted index row (the delete-tail ghost-row class
	// r2 finding 4 closed, re-opening here). Guard against BOTH id spaces:
	// the harness id (rowID, where the tombstone lives) and the wire id
	// (a legacy frame where wire = harness).
	if b.h.isSessionDeleted(workspaceID, rowID) || b.h.isSessionDeleted(workspaceID, sessionID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Design 0066 registration seam (context-usage row-creating path).
	b.h.sessionIndex.Register(ctx, workspaceID, rowID, "")
	if err := b.h.sessionIndex.UpsertContextUsed(ctx, workspaceID, rowID, used); err != nil {
		b.h.logger.Warn("usagestream: context-usage upsert failed", "error", err, "workspaceID", workspaceID, "sessionID", sessionID)
	}
}

func (b *usageBridge) AgentDied(workspaceID string) {
	if b.h.userBroker == nil {
		return
	}
	b.h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
		Type:        "agent_died",
		WorkspaceID: workspaceID,
		Data:        map[string]string{"reason": "unknown"},
	})
}

// inputRequestFromABI maps the ABI InputRequest onto the platform
// contract shape (root resolved) — the single SSE payload form for both
// kinds (#1302 item 4).
func inputRequestFromABI(req *abiv1.InputRequest, root string) session.InputRequest {
	ir := session.InputRequest{
		ID:            req.GetId(),
		SessionID:     req.GetSessionId(),
		RootSessionID: root,
	}
	if req.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION {
		ir.Kind = session.InputPermission
		ir.Permission = req.GetPermission()
		ir.Patterns = req.GetPatterns()
		ir.Always = req.GetAlways()
	} else {
		ir.Kind = session.InputQuestion
		ir.Question = req.GetQuestion()
		ir.Header = req.GetHeader()
		ir.Multiple = req.GetMultiple()
		ir.Custom = req.GetCustom()
		for _, o := range req.GetOptions() {
			ir.Options = append(ir.Options, session.InputOption{Label: o.GetLabel(), Description: o.GetDescription()})
		}
	}
	if t := req.GetTool(); t != nil {
		ir.Tool = &session.ToolRef{MessageID: t.GetMessageId(), CallID: t.GetCallId()}
	}
	return ir
}

// newUsageStreamClient builds the reference abiclient over a §D1
// Basic-auth transport (same discipline as the contract-stream proxy —
// that transport lives in the contractstream package unexported, so this
// is a local twin).
func newUsageStreamClient(baseURL, password string) usagestream.Client {
	httpClient := &http.Client{
		Transport: &usageAuthTransport{password: password, inner: http.DefaultTransport},
		Timeout:   0, // streams: reads are ctx-bounded by the client
	}
	return abiclient.New(httpClient, baseURL)
}

type usageAuthTransport struct {
	password string
	inner    http.RoundTripper
}

func (t *usageAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(agentd.AuthUsername, t.password)
	return t.inner.RoundTrip(r)
}

// translateSessionIDToPhrase is the #1821 piece-1 ID-edge translation:
// the user-stream wire id becomes the PHRASE (the id space the frontend
// joins on since the #1756/#1775/#1806 re-keys — session list, history
// REST, chat identity), with the harness id preserved ADDITIVELY as
// internal_session_id. One id space at the edge; the harness id is
// never dropped (consumers that need it read the additive field).
// Degraded seam (index nil, session unknown, or no phrase minted):
// the event passes through UNCHANGED — the pre-fix behavior, never
// worse. The Register seam is idempotent and LRU-cached after first
// sight, so the hot emission path is cheap once a session is known.
// ctx is the caller's context — it is threaded to the Register seam so
// cancellations/deadlines propagate.
func (h *ProxyHandler) translateSessionIDToPhrase(ctx context.Context, workspaceID string, evt *apitypes.WorkspaceSSEEvent) {
	harnessID := evt.SessionID
	if h.sessionIndex == nil || harnessID == "" {
		return
	}
	// #1817 piece 3 — double-translation guard (the refreshed-pod defect):
	// on refreshed pods the agentd emission already translated the frame to
	// the phrase and left the harness id ADDITIVELY as internal_session_id
	// (authority.go wireEvent / projection.go podSnapshotsLocked). If the
	// consumer passes that already-phrase wire id back through this
	// translation, the Register below would treat the PHRASE as a session_id
	// and MINT a new short-id row (sessionindex.Service.Register →
	// EnsureSessionRegistered — the INSERT keys on session_id, but a phrase
	// lives in short_id, so the phrase is never found and a ghost row is
	// created). That re-broke the busy join this PR exists to fix and
	// produced a phantom sessions-list row. The additive internal id is the
	// marker that the frame was already translated: resolve from it (the
	// harness id) and leave the phrase untouched — Register on an already-
	// resolved session is a no-op (the row exists), and the internal id
	// stays the harness id. A harness-keyed frame (heal window / legacy)
	// has no internal id and still translates: phrase = Register(harness).
	// #1817 piece 3 — double-translation guard (the refreshed-pod defect):
	// the additive internal_session_id is the marker that agentd ALREADY
	// translated the frame — the wire SessionID is the PHRASE and the
	// harness id rides additively. In that case the wire id is already
	// correct: re-registering the phrase (Register(workspace, phrase)) would
	// MINT a phantom short-id row (EnsureSessionRegistered INSERTs on
	// session_id, but a phrase lives in short_id), re-breaking the busy join
	// and writing context_used to a ghost row. So pass the phrase through
	// UNCHANGED (re-affirm the additive id) — never downgrade a correct
	// phrase to the harness id, even under a degraded index (Register
	// returning "" must not flip a healthy refreshed-pod frame back to
	// harness space — "never worse" holds for the refreshed-pod shape too).
	if internalID := internalSessionIDFromData(evt.Data); internalID != "" {
		setInternalSessionID(evt, internalID)
		return
	}
	regCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	phrase := h.sessionIndex.Register(regCtx, workspaceID, harnessID, "")
	if phrase == "" || phrase == harnessID {
		return
	}
	evt.SessionID = phrase
	switch data := evt.Data.(type) {
	case map[string]any:
		if data == nil {
			data = map[string]any{}
		}
		data["internal_session_id"] = harnessID
		evt.Data = data
	case map[string]string:
		data["internal_session_id"] = harnessID
	case session.InputRequest:
		// #1817 piece 2: agent.question / agent.permission carry a
		// STRUCT payload (inputRequestFromABI) whose fields already
		// carry the session id — replacing it with a synthetic map
		// clobbers the contract payload the frontend renders. The wire
		// session_id above is the phrase, the payload keeps its own.
	default:
		evt.Data = map[string]any{"internal_session_id": harnessID}
	}
}

// translateSessionIDToPhraseNoMint is the READ-ONLY twin of
// translateSessionIDToPhrase for the LIFECYCLE CLEAR path (the delete tail).
// #1817 piece 3 (r2 finding 4): the delete tail runs AFTER DeleteSession has
// removed the row, so calling the minting translateSessionIDToPhrase here
// would Register(workspace, harness) → EnsureSessionRegistered → INSERT a
// ghost row for a session that was just deleted, and the `deleted` clear
// would carry a freshly-minted phrase the frontend does not recognize.
// ResolveShortID is LRU-only (no mint): it returns the phrase the session
// was ALREADY known under (the LRU survives DeleteSession), and the clear
// matches the busy flip's phrase key. When unknown (cold/evicted LRU) it
// returns "" and the clear passes the harness id through — never worse:
// the frontend removes the session by identity either way.
func (h *ProxyHandler) translateSessionIDToPhraseNoMint(workspaceID string, evt *apitypes.WorkspaceSSEEvent) {
	harnessID := evt.SessionID
	if h.sessionIndex == nil || harnessID == "" {
		return
	}
	// Same double-translation guard: a refreshed-pod frame already carries
	// the phrase + additive internal id — leave it untouched.
	if internalID := internalSessionIDFromData(evt.Data); internalID != "" {
		setInternalSessionID(evt, internalID)
		return
	}
	phrase := h.sessionIndex.ResolveShortID(workspaceID, harnessID)
	if phrase == "" || phrase == harnessID {
		return
	}
	evt.SessionID = phrase
	switch data := evt.Data.(type) {
	case map[string]any:
		if data == nil {
			data = map[string]any{}
		}
		data["internal_session_id"] = harnessID
		evt.Data = data
	case map[string]string:
		data["internal_session_id"] = harnessID
	}
}

// internalSessionIDFromData reads the additive internal_session_id the
// #1817 piece-1 emission leaves on the wire (the harness id) across both
// map payload shapes. The session.InputRequest struct has no such field
// — the nested payload keeps its own session id (agentd translates only
// the top-level frame field), so struct payloads return "".
func internalSessionIDFromData(data any) string {
	switch m := data.(type) {
	case map[string]any:
		if v, ok := m["internal_session_id"].(string); ok {
			return v
		}
	case map[string]string:
		return m["internal_session_id"]
	}
	return ""
}

// setInternalSessionID writes the additive internal_session_id on the two
// map payload shapes (the struct payload keeps its own id field).
func setInternalSessionID(evt *apitypes.WorkspaceSSEEvent, harnessID string) {
	switch d := evt.Data.(type) {
	case map[string]any:
		if d == nil {
			d = map[string]any{}
		}
		d["internal_session_id"] = harnessID
		evt.Data = d
	case map[string]string:
		d["internal_session_id"] = harnessID
	}
}

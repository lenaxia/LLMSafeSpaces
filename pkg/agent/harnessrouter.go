// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

// harnessrouter.go — the per-workspace Adapter dispatch (S5
// harness-selection plumbing, scoping §6 item 4). The platform holds
// ONE Adapter (the proxy handler's ctor-required seam, unchanged);
// THIS adapter resolves each call's workspace to its harness via the
// CRD (spec.harness) and delegates to that harness's adapter,
// constructed once per harness through the registry's factory.
//
// Routing rules:
//   - The per-workspace methods (sessions, messaging, models) route on
//     the caller's workspaceID — the one argument every one of them
//     already carries.
//   - The stateless event translators (ContextUsageFromEvent,
//     ClientEventsFromEvent, RetryFromEvent) and Capabilities carry no
//     workspace identity and have no production caller today (the SSE
//     translation paths live inside the adapters); they delegate to
//     the DEFAULT harness's adapter so the seam stays total. When a
//     caller with workspace context materializes, the method shapes
//     grow the argument — never a guess here.
//   - A harness-lookup failure falls back to the default harness with
//     a logged warn: an apiserver hiccup must not take down the
//     (previously only) opencode fleet's hot path. The harness is
//     immutable after create, so the cache never serves a stale
//     SWITCHED value — only a same-name delete+recreate could change
//     it, and the TTL bounds that window.
//
// systemnotices.Wrap wraps this router exactly as it wrapped the
// opencode adapter (it embeds the interface — harness-neutral by
// construction, #944's one-point injection preserved).

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// HarnessLookup resolves a workspaceID to its CRD-declared harness.
// The API wiring backs this with the workspace fetch (the same client
// the pod-IP resolver uses); tests back it with a map.
type HarnessLookup func(ctx context.Context, workspaceID string) (AgentType, error)

// HarnessRouterConfig is the router's construction input.
type HarnessRouterConfig struct {
	// Default is the harness assumed when the lookup fails or the
	// workspace's spec.harness is empty (the CRD default). Empty
	// defaults to AgentTypeOpenCode — the pre-S5 behavior.
	Default AgentType
	// Lookup backs the per-workspace routing (nil routes everything to
	// Default — the pre-S5 behavior, one adapter).
	Lookup HarnessLookup
	// Factory constructs per-harness adapters. Nil uses
	// NewAdapterForType (the registry); tests inject recorded fakes.
	Factory func(agentType AgentType, deps AdapterFactoryDeps) (Adapter, error)
	// Deps are the shared wiring inputs passed to every factory.
	Deps   AdapterFactoryDeps
	Logger *zap.Logger
}

// harnessCacheTTL bounds the stale same-name-recreate window (the
// harness itself is immutable after create; only a delete + recreate
// under the same name can change it).
const harnessCacheTTL = 60 * time.Second

type harnessCacheEntry struct {
	harness AgentType
	expiry  time.Time
}

// HarnessRouter is the Adapter that dispatches per workspace.
type HarnessRouter struct {
	cfg      HarnessRouterConfig
	defaultA Adapter

	mu       sync.RWMutex
	adapters map[AgentType]Adapter
	cache    map[string]harnessCacheEntry
}

var _ Adapter = (*HarnessRouter)(nil)

// NewHarnessRouter builds the router: the default harness's adapter is
// constructed EAGERLY (its failure is fatal — the default harness must
// exist for the platform to boot, exactly as the pre-S5 hard-wired
// construction was); every other harness constructs lazily on first
// use (a pi-adapter construction failure then surfaces on pi calls,
// never on the opencode fleet).
func NewHarnessRouter(cfg HarnessRouterConfig) (*HarnessRouter, error) {
	if cfg.Default == "" {
		cfg.Default = AgentTypeOpenCode
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	factory := cfg.Factory
	if factory == nil {
		factory = func(agentType AgentType, deps AdapterFactoryDeps) (Adapter, error) {
			return NewAdapterForType(agentType, deps)
		}
	}
	def, err := factory(cfg.Default, cfg.Deps)
	if err != nil {
		return nil, err
	}
	return &HarnessRouter{
		cfg:      cfg,
		defaultA: def,
		adapters: map[AgentType]Adapter{cfg.Default: def},
		cache:    map[string]harnessCacheEntry{},
	}, nil
}

// adapterFor resolves the workspace's adapter (cached, fail-open to
// the default harness on lookup errors — see the file header).
func (h *HarnessRouter) adapterFor(ctx context.Context, workspaceID string) Adapter {
	t, ok := h.harnessFor(ctx, workspaceID)
	if !ok {
		t = h.cfg.Default
	}
	return h.adapterForType(t)
}

// harnessFor serves the cached lookup; ok=false on any miss/error.
func (h *HarnessRouter) harnessFor(ctx context.Context, workspaceID string) (AgentType, bool) {
	if h.cfg.Lookup == nil || workspaceID == "" {
		return h.cfg.Default, false
	}
	h.mu.RLock()
	if e, hit := h.cache[workspaceID]; hit && time.Now().Before(e.expiry) {
		h.mu.RUnlock()
		return e.harness, true
	}
	h.mu.RUnlock()

	t, err := h.cfg.Lookup(ctx, workspaceID)
	if err != nil {
		h.cfg.Logger.Warn("harness router: workspace harness lookup failed; routing to default",
			zap.String("workspace", workspaceID), zap.Error(err))
		return h.cfg.Default, false
	}
	h.mu.Lock()
	h.cache[workspaceID] = harnessCacheEntry{t, time.Now().Add(harnessCacheTTL)}
	h.mu.Unlock()
	return t, true
}

// adapterForType returns the harness's adapter, constructing lazily.
// A lazy construction failure logs and falls back to the default —
// the call then fails in the DEFAULT adapter (wrong-harness error
// surface) rather than a router panic; both are loud.
func (h *HarnessRouter) adapterForType(t AgentType) Adapter {
	h.mu.RLock()
	a, ok := h.adapters[t]
	h.mu.RUnlock()
	if ok {
		return a
	}
	a, err := h.cfg.Factory(t, h.cfg.Deps)
	if err != nil {
		h.cfg.Logger.Error("harness router: adapter construction failed; using default harness adapter",
			zap.String("harness", string(t)), zap.Error(err))
		return h.defaultA
	}
	h.mu.Lock()
	h.adapters[t] = a
	h.mu.Unlock()
	return a
}

// --- Sessions ------------------------------------------------------------

func (h *HarnessRouter) CreateSession(ctx context.Context, userID, workspaceID, title string) (*session.Session, error) {
	return h.adapterFor(ctx, workspaceID).CreateSession(ctx, userID, workspaceID, title)
}

func (h *HarnessRouter) GetSession(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error) {
	return h.adapterFor(ctx, workspaceID).GetSession(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) ListSessions(ctx context.Context, userID, workspaceID string) ([]session.Session, error) {
	return h.adapterFor(ctx, workspaceID).ListSessions(ctx, userID, workspaceID)
}

func (h *HarnessRouter) CountMessages(ctx context.Context, userID, workspaceID, sessionID string) (int, error) {
	return h.adapterFor(ctx, workspaceID).CountMessages(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	return h.adapterFor(ctx, workspaceID).RenameSession(ctx, userID, workspaceID, sessionID, title)
}

func (h *HarnessRouter) DeleteSession(ctx context.Context, userID, workspaceID, sessionID string) error {
	return h.adapterFor(ctx, workspaceID).DeleteSession(ctx, userID, workspaceID, sessionID)
}

// --- Messaging -----------------------------------------------------------

func (h *HarnessRouter) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	return h.adapterFor(ctx, workspaceID).Send(ctx, userID, workspaceID, sessionID, text, opts)
}

func (h *HarnessRouter) SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error) {
	return h.adapterFor(ctx, workspaceID).SendAsync(ctx, userID, workspaceID, sessionID, text, opts)
}

func (h *HarnessRouter) Abort(ctx context.Context, userID, workspaceID, sessionID string) error {
	return h.adapterFor(ctx, workspaceID).Abort(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) SetSessionModel(ctx context.Context, userID, workspaceID, sessionID string, m *session.ModelRef) error {
	return h.adapterFor(ctx, workspaceID).SetSessionModel(ctx, userID, workspaceID, sessionID, m)
}

func (h *HarnessRouter) VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (bool, bool, error) {
	return h.adapterFor(ctx, workspaceID).VerifyDelivery(ctx, userID, workspaceID, sessionID, text, since)
}

func (h *HarnessRouter) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	return h.adapterFor(ctx, workspaceID).GetHistory(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	return h.adapterFor(ctx, workspaceID).GetHistoryPage(ctx, userID, workspaceID, sessionID, limit)
}

// --- Streaming / Input ---------------------------------------------------

func (h *HarnessRouter) Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error) {
	return h.adapterFor(ctx, workspaceID).Stream(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error) {
	return h.adapterFor(ctx, workspaceID).ListPending(ctx, userID, workspaceID, sessionID)
}

func (h *HarnessRouter) RejectInput(ctx context.Context, userID, workspaceID, requestID string) error {
	return h.adapterFor(ctx, workspaceID).RejectInput(ctx, userID, workspaceID, requestID)
}

func (h *HarnessRouter) AnswerQuestion(ctx context.Context, userID, workspaceID, requestID string, answers [][]string) error {
	return h.adapterFor(ctx, workspaceID).AnswerQuestion(ctx, userID, workspaceID, requestID, answers)
}

func (h *HarnessRouter) ReplyPermission(ctx context.Context, userID, workspaceID, requestID, reply, message string) error {
	return h.adapterFor(ctx, workspaceID).ReplyPermission(ctx, userID, workspaceID, requestID, reply, message)
}

// --- Models --------------------------------------------------------------

func (h *HarnessRouter) ListAvailableModels(ctx context.Context, userID, workspaceID string) ([]session.ModelInfo, error) {
	return h.adapterFor(ctx, workspaceID).ListAvailableModels(ctx, userID, workspaceID)
}

func (h *HarnessRouter) SetModel(ctx context.Context, userID, workspaceID, sessionID string, model session.ModelRef) error {
	return h.adapterFor(ctx, workspaceID).SetModel(ctx, userID, workspaceID, sessionID, model)
}

// --- Stateless translators (no workspace identity — default harness) ------

func (h *HarnessRouter) ContextUsageFromEvent(eventType string, rawData string) (string, *session.ContextUsage, bool) {
	return h.defaultA.ContextUsageFromEvent(eventType, rawData)
}

func (h *HarnessRouter) ClientEventsFromEvent(eventType string, rawData string) []session.Event {
	return h.defaultA.ClientEventsFromEvent(eventType, rawData)
}

func (h *HarnessRouter) RetryFromEvent(eventType string, rawData string) (string, *ClientRetryStatus, bool) {
	return h.defaultA.RetryFromEvent(eventType, rawData)
}

func (h *HarnessRouter) Capabilities() []session.Capability {
	return h.defaultA.Capabilities()
}

// --- Credentials ---------------------------------------------------------

func (h *HarnessRouter) FormatProviderConfig(providers []LLMProviderData) ([]byte, error) {
	return h.defaultA.FormatProviderConfig(providers)
}

func (h *HarnessRouter) ValidateCredentials(rawConfig []byte) (*CredentialCheckResult, error) {
	return h.defaultA.ValidateCredentials(rawConfig)
}

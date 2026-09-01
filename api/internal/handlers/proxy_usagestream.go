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
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
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
	usageStreamOnce sync.Once
	usageStreamPtr  *usagestream.Consumer
)

// UsageStream lazily builds the process-wide consumer (the resolve seam
// re-resolves the pod per (re)connect — resume-safe, A7).
func (h *ProxyHandler) UsageStream() *usagestream.Consumer {
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
	if b.h.userBroker == nil {
		return
	}
	status := "idle"
	if busy {
		status = "busy"
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		b.h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
			Type:        "session.status",
			WorkspaceID: workspaceID,
			SessionID:   sessionID,
			Status:      status,
		})
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
	root := req.GetRootSessionId()
	if root == "" {
		root = b.h.resolveRootSessionID(workspaceID, req.GetSessionId())
	}
	evt := apitypes.WorkspaceSSEEvent{
		Type:      "agent.question",
		SessionID: req.GetSessionId(),
		RequestID: req.GetId(),
	}
	if req.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION {
		evt.Type = "agent.permission"
		evt.Data = permissionRequestFromABI(req, root)
	} else {
		evt.Data = questionRequestFromABI(req, root)
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
		b.h.userBroker.PublishToUser(userID, evt)
	}
}

func (b *usageBridge) InputResolved(workspaceID, sessionID, inputID string) {
	if b.h.userBroker == nil {
		return
	}
	resolvedType := "agent.question.resolved"
	if b.kindOf(inputID) == abiv1.InputKind_INPUT_KIND_PERMISSION {
		resolvedType = "agent.permission.resolved"
	}
	evt := apitypes.WorkspaceSSEEvent{
		Type:      resolvedType,
		SessionID: sessionID,
		RequestID: inputID,
		Data: map[string]string{
			"request_id": inputID,
			"session_id": sessionID,
		},
	}
	if userID := b.h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
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
	if b.h.sessionIndex == nil || sessionID == "" || b.h.isSessionDeleted(workspaceID, sessionID) || used <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.h.sessionIndex.UpsertContextUsed(ctx, workspaceID, sessionID, used); err != nil {
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

// questionRequestFromABI re-expands the unified InputRequest into the
// QuestionRequest wire shape the frontend provider stores (the ABI's
// flattened single-question form reversed).
func questionRequestFromABI(req *abiv1.InputRequest, root string) agent.QuestionRequest {
	q := agent.QuestionRequest{
		ID:            req.GetId(),
		SessionID:     req.GetSessionId(),
		RootSessionID: root,
		Questions: []agent.QuestionInfo{{
			Question: req.GetQuestion(),
			Header:   req.GetHeader(),
			Multiple: req.GetMultiple(),
		}},
	}
	for _, o := range req.GetOptions() {
		q.Questions[0].Options = append(q.Questions[0].Options, agent.QuestionOption{Label: o.GetLabel(), Description: o.GetDescription()})
	}
	if t := req.GetTool(); t != nil {
		q.Tool = &agent.ToolRef{MessageID: t.GetMessageId(), CallID: t.GetCallId()}
	}
	return q
}

// permissionRequestFromABI maps the ABI permission input onto the
// PermissionRequest wire shape.
func permissionRequestFromABI(req *abiv1.InputRequest, root string) agent.PermissionRequest {
	p := agent.PermissionRequest{
		ID:            req.GetId(),
		SessionID:     req.GetSessionId(),
		RootSessionID: root,
		Permission:    req.GetPermission(),
		Patterns:      req.GetPatterns(),
		Always:        req.GetAlways(),
	}
	if t := req.GetTool(); t != nil {
		p.Tool = &agent.ToolRef{MessageID: t.GetMessageId(), CallID: t.GetCallId()}
	}
	return p
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

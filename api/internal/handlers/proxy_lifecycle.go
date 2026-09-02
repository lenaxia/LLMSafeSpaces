// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func (h *ProxyHandler) EnableSessionParentResolution() {
	if h.sessionParents != nil {
		return
	}
	h.sessionParents = newSessionParentCache(h.fetchSessionParent)
}

func (h *ProxyHandler) Start() error {
	var startErr error
	h.startOnce.Do(func() {
		h.started = true
		h.stopCh = make(chan struct{})
		h.userBroker = eventbroker.NewUserEventBroker()

		h.activityTracker = activity.NewActivityTracker(h.k8sClient, h.logger, h.namespace)
		if err := h.activityTracker.Start(); err != nil {
			startErr = fmt.Errorf("starting activity tracker: %w", err)
			return
		}

		watcher, err := workspace.NewWatcher(h.k8sClient, h.logger, h.namespace, h.onPhaseChange)
		if err != nil {
			_ = h.activityTracker.Stop()
			startErr = fmt.Errorf("creating CRD watcher: %w", err)
			return
		}
		watcher.SetUserBroker(h.userBroker)
		if h.versionSyncCb != nil {
			watcher.SetVersionSyncCallback(h.versionSyncCb)
		}
		if err := watcher.Start(); err != nil {
			_ = h.activityTracker.Stop()
			startErr = fmt.Errorf("starting CRD watcher: %w", err)
			return
		}
		h.watcher = watcher
		h.phaseSource = watcher
		// #902 semantics live on in the state reconciler: gates that die
		// (pod churn) heal on its tick; usage-gate Open is idempotent.
		go h.stateReconciler(stateReconcileInterval)

		// D3 (#907): the outbox delivery worker — detached from any
		// request context; survives client disconnects (the incident's
		// message-loss class). The bridge re-resolves the workspace and
		// re-checks quota per delivery.
		if h.outbox != nil {
			if h.adapter != nil {
				h.outbox.SetOnDelivered(h.outboxOnDelivered)
				h.outbox.SetOnStaged(h.outboxOnStaged)
			}
			wctx, wcancel := context.WithCancel(context.Background())
			h.outboxCancel = wcancel
			go func() {
				<-h.stopCh
				wcancel()
			}()
			go h.outbox.Run(wctx, h.outboxDeliver, outboxTick)
		}
	})
	return startErr
}

// stateReconcileInterval is the state reconciler's tick (gate arming,
// D6 sweep, statusz self-heal). Var for tests.
var stateReconcileInterval = 60 * time.Second

func (h *ProxyHandler) Stop() error {
	h.stopOnce.Do(func() {
		if h.stopCh != nil {
			close(h.stopCh)
		}
		if h.watcher != nil {
			h.watcher.Stop()
		}
		if h.activityTracker != nil {
			_ = h.activityTracker.Stop()
		}
	})
	return nil
}

func (h *ProxyHandler) GetPasswordGetter() interfaces.WorkspacePasswordProvider {
	return h
}

func (h *ProxyHandler) SetAgentStateChecker(c AgentStateChecker) {
	h.agentStateChecker = c
}

func (h *ProxyHandler) SetVersionSyncCallback(cb workspace.VersionSyncCallback) {
	h.versionSyncCb = cb
}

// SetRequestBufferConfig rebuilds the per-workspace request buffer with the
// configured size and timeout. Must be called before Start: request goroutines
// read h.requestBuffer without synchronization, so a late swap would race.
// Values <=0 fall back to the enabled defaults (size 10, timeout 30s) so the
// feature is on unless explicitly constructed disabled — the zero-value config
// must not silently turn buffering off in production.
func (h *ProxyHandler) SetRequestBufferConfig(maxSize int, timeout time.Duration) {
	if h.started {
		panic("SetRequestBufferConfig called after Start — request goroutines may already be reading requestBuffer")
	}
	if maxSize <= 0 {
		maxSize = defaultBufferMaxSize
	}
	h.requestBuffer = newRequestBuffer(maxSize, timeout, defaultBufferPollInterval, h.logger)
}

func (h *ProxyHandler) SetMeteringService(svc interfaces.MeteringService) {
	h.meteringSvc = svc
}

func (h *ProxyHandler) GetBroker() BrokerPublisher {
	if h.userBroker == nil {
		return nil
	}
	return h.userBroker
}

func (h *ProxyHandler) GetWorkspaceOwner(workspaceID string) string {
	if h.userBroker == nil {
		return ""
	}
	return h.userBroker.WorkspaceOwner(workspaceID)
}

// publishWorkspaceEvent fans out a workspace-scoped SSE event to subscribers.
func (h *ProxyHandler) publishWorkspaceEvent(workspaceID string, evt apitypes.WorkspaceSSEEvent) {
	if h.userBroker != nil {
		h.userBroker.PublishToWorkspace(workspaceID, evt)
	}
}

// publishWorkspaceAndUserEvent delivers an event to BOTH the workspace stream
// (active-view consumers) and the user stream (cross-workspace, replay-buffered).
// Use for low-frequency events that affect global UI state (agent.question,
// agent.permission). The user-stream copy carries WorkspaceID so the frontend
// can route it; the workspace-stream copy does not (implicit for subscribers).
// If the workspace owner is unrecorded, the user-stream publish is skipped
// silently — the workspace-stream publish still fires.
func (h *ProxyHandler) publishWorkspaceAndUserEvent(workspaceID string, evt apitypes.WorkspaceSSEEvent) {
	if h.userBroker == nil {
		return
	}
	h.userBroker.PublishToWorkspace(workspaceID, evt)
	if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
		evt.WorkspaceID = workspaceID
		h.userBroker.PublishToUser(userID, evt)
	}
}

func (h *ProxyHandler) GetAllKnownPhases() map[string]string {
	if h.watcher == nil {
		return nil
	}
	return h.watcher.GetAllKnownPhases()
}

// outboxTick is the outbox delivery scan interval. Var for tests.
var outboxTick = 1 * time.Second

// SetAgentdTerminus switches the outbox deliverer to the agentd ledger
// (US-69.8, design 0055 M2/M4). It must be called before Start().
//
// Parameters:
//   - enabled: route outboxDeliver through the ABI delivery ledger.
func (h *ProxyHandler) SetAgentdTerminus(enabled bool) {
	h.agentdTerminus = enabled
}

// outboxDeliver bridges the outbox worker to the adapter: detached
// context and D3 model-selector forwarding (the accepted entry carries
// the raw selector JSON). Confirmed delivery completes via the outbox's
// OnDelivered hook (wired in Start) — the SINGLE seam for the
// cross-cutting events (SSE queue.update/sent, activity, session-index,
// llm_request usage) on every delivery path.
//
// Failure classification (#987 rule 2, the surviving half): an HTTP
// status response (agent.ErrHTTPStatus) means the agent PROCESSED and
// rejected the request — definitive, safe to retry. Anything else
// (timeout mid-turn, connection cut mid-flight) is outcome-UNKNOWN and
// wraps outbox.Ambiguous. The transcript-verify oracle that once
// resolved ambiguous outcomes (and gated re-sends) was deleted per the
// admission-ID matrix disposition (#1219): with no verifier wired the
// outbox degrades ambiguous entries to its bounded retry ladder — the
// documented trade of a bounded duplicate risk against the stranded-
// in-verifying alternative (outbox.Service.verifyOne).
func (h *ProxyHandler) outboxDeliver(ctx context.Context, workspaceID, sessionID string, e outbox.Entry) error {
	if h.agentdTerminus {
		return h.agentdTerminusDeliver(ctx, workspaceID, sessionID, e)
	}
	var model *session.ModelRef
	if len(e.Model) > 0 {
		var m session.ModelRef
		if json.Unmarshal(e.Model, &m) == nil && m.ID != "" {
			model = &m
		}
	}
	opts := session.SendOpts{Model: model}
	if h.v2Delivery {
		// V2 mode (design 0052, OPENCODE_V2_DELIVERY): admission
		// returns in milliseconds. The admission→promotion await that
		// once ran here (its only observer was the deleted text-scan
		// oracle, #1119) is gone with the oracle; the stranded-admitted
		// risk it covered is owned by the agentd ledger's state machine
		// on the authority path (design 0055 M2/I6), which is the only
		// regime where this branch's flag combination occurs in
		// production. This legacy branch completes at admission.
		//
		// V2 has no per-prompt model: the endpoint strips the field
		// (verified live, 2026-08-29). Apply the entry's model to the
		// SESSION before admission — the same mechanism the SPA uses.
		if model != nil {
			if err := h.adapter.SetSessionModel(ctx, "", workspaceID, sessionID, model); err != nil {
				if errors.Is(err, agent.ErrHTTPStatus) {
					return err
				}
				return outbox.Ambiguous(err)
			}
		}
		_, err := h.adapter.SendAsync(ctx, "", workspaceID, sessionID, e.Text, opts)
		if err != nil {
			if errors.Is(err, agent.ErrHTTPStatus) {
				return err
			}
			return outbox.Ambiguous(err)
		}
		return nil
	}
	_, err := h.adapter.Send(ctx, "", workspaceID, sessionID, e.Text, opts)
	if err != nil {
		if errors.Is(err, agent.ErrHTTPStatus) {
			return err
		}
		return outbox.Ambiguous(err)
	}
	return nil
}

// agentdTerminusDeliver is the US-69.8 terminus: resolve the pod's ABI
// surface with the proxy's resume-safe semantics, then POST/poll the
// ledger (the deliverer implements the I10 mapping).
func (h *ProxyHandler) agentdTerminusDeliver(ctx context.Context, workspaceID, sessionID string, e outbox.Entry) error {
	d := &agentdDeliverer{
		resolve: func(ctx context.Context, ws, ses string) (string, string, error) {
			return h.agentdEndpoint(ctx, ws)
		},
	}
	return d.deliver(ctx, workspaceID, sessionID, e)
}

// SetV2Delivery enables V2 admit-and-return outbox delivery (design
// 0052, OPENCODE_V2_DELIVERY). Must be called before Start(), and the
// adapter must be constructed with WithV2Store(true) in the same
// wiring (delivery and history reads must agree on the store).
func (h *ProxyHandler) SetV2Delivery(enabled bool) {
	h.v2Delivery = enabled
}

// outboxOnDelivered is the single confirmed-delivery seam: cross-cutting
// events (activity, session-index, metering) plus the queue.update/sent
// SSE that clears the frontend pill deterministically (#987 — the
// outbox path previously never emitted it).
func (h *ProxyHandler) outboxOnDelivered(workspaceID, sessionID string, e outbox.Entry) {
	// US-69.11: a confirmed delivery means a turn may start — arm the
	// busy-gated usage stream so billing + the state bridge see it. The
	// gate drops itself after the idle settle window.
	if h.agentdTerminus {
		h.UsageStream().Open(workspaceID)
	}
	h.postOutboxDeliverSuccess(workspaceID, sessionID, e)
	h.publishQueueEvent(workspaceID, sessionID, "sent", e.ID, "")
}

// outboxOnStaged publishes the picked-up signal: the entry left the
// visible queue and its delivery POST is in flight. The delivery send
// is synchronous turn-to-completion (the V2 admit-and-schedule path is
// dormant on the pinned opencode, #755), so without this event the pill
// renders "Sending…" for the whole multi-minute turn. Frontends clear
// the pill here — TUI parity: once the agent owns the message it is in
// the conversation, not the queue. The authoritative cleanup remains
// queue.update/sent at confirmed delivery (outboxOnDelivered).
func (h *ProxyHandler) outboxOnStaged(workspaceID, sessionID string, e outbox.Entry) {
	h.publishQueueEvent(workspaceID, sessionID, "delivering", e.ID, "")
}

// postOutboxDeliverSuccess mirrors postAdapterSuccess for the detached
// delivery path (no gin context: the accept-time userID and a synthetic
// request id ride the entry).
func (h *ProxyHandler) postOutboxDeliverSuccess(workspaceID, sessionID string, e outbox.Entry) {
	if h.activityTracker != nil {
		h.activityTracker.Record(workspaceID)
	}
	if h.sessionIndex != nil && sessionID != "" {
		h.sessionIndex.RecordMessage(workspaceID, sessionID, "", time.Now())
	}
	if h.meteringSvc != nil && workspaceID != "" {
		if e.UserID != "" {
			h.meteringSvc.Record(types.UsageEvent{
				IdempotencyKey: fmt.Sprintf("llmreq:%s:%d", workspaceID, time.Now().UnixNano()),
				Owner:          types.BillingOwner{ID: e.UserID, Type: types.OwnerTypeUser},
				ActorID:        e.UserID,
				WorkspaceID:    workspaceID,
				EventType:      "llm_request",
				EventSubtype:   "message",
				Quantity:       1,
				Source:         "api",
				EventTime:      time.Now(),
				RequestContext: map[string]any{
					"request_id": "outbox:" + e.ID,
					"session_id": sessionID,
				},
			})
		}
	}
}

// D6 (#998) tunables. BusyOlderThan: a session busy this long is
// escalated (design sketch said 900s; 15 min of continuous busy with no
// idle is the hung-and-alive signature post-watchdog-demotion).
// AlertCooldown: minimum spacing between alerts for the same workspace —
// unattended sessions stay busy until a human acts, so an alert per tick
// would be noise. Vars for tests.
var (
	busyAlertOlderThan = 15 * time.Minute
	busyAlertCooldown  = 30 * time.Minute
)

// escalateHungs publishes workspace.alert for every watched Active
// workspace whose statusz reports oldest_busy_seconds past the threshold
// (D6/#998: hung-and-alive sessions — suppressed-forever by design D1 —
// are surfaced to the owner instead of executed). Statusz fetch failure
// is silent-to-log: the alert path must not add load when the pod is
// merely slow (the tracker/alerting stack covers hard failures).
func (h *ProxyHandler) escalateHungs(workspaceIDs []string) {
	for _, wid := range workspaceIDs {
		if h.busyAlertCooling(wid) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		podIP := h.statuszPodIP(ctx, wid)
		if podIP == "" {
			cancel()
			continue
		}
		sz, err := h.fetchStatusz(ctx, wid, podIP)
		cancel()
		if err != nil {
			continue
		}
		if sz.OldestBusySeconds < int(busyAlertOlderThan.Seconds()) {
			continue
		}
		h.publishWorkspaceAndUserEvent(wid, apitypes.WorkspaceSSEEvent{
			Type:      "workspace.alert",
			SessionID: oldestSession(sz.BusyAges),
			Status:    "session_hung",
			Data: map[string]any{
				"alert":               "session_hung",
				"oldest_busy_seconds": sz.OldestBusySeconds,
				"busy_ages":           sz.BusyAges,
				"policy":              "notify_only",
				"guidance":            "session busy beyond threshold with no progress — likely hung-and-alive; stop or resume manually",
			},
		})
		h.markBusyAlerted(wid)
		// #998 finding 4: persist the alert so it lands in session
		// history for workflow surfaces and survives SSE disconnects.
		// Non-blocking (bounded queue + drainer); nil = SSE-only.
		if h.sessionAlerts != nil {
			h.sessionAlerts.RecordAlert(wid, oldestSession(sz.BusyAges), "session_hung", sz.OldestBusySeconds)
		}
		h.logger.Warn("D6 escalation: session hung (notify-only)",
			"workspaceID", wid, "oldestBusySeconds", sz.OldestBusySeconds)
	}
}

// fetchStatusz GETs /v1/statusz from the agentd admin port using the
// shared bearer-candidate machinery (workspace secret admin-token, the
// same path reconcileSessionState uses).
func (h *ProxyHandler) fetchStatusz(ctx context.Context, workspaceID, podIP string) (*agentd.StatuszResponse, error) {
	// Production PodIPs carry no port; test fakes listen on ephemeral
	// ports and pass host:port. Append the admin port only for bare hosts.
	host := podIP
	if !strings.Contains(host, ":") {
		host = fmt.Sprintf("%s:%d", host, agentd.AgentdAdminPort)
	}
	url := fmt.Sprintf("http://%s/v1/statusz", host) //nolint:gosec // G107: internal pod
	resp, err := GetWithBearers(ctx, h.httpClient, url, h.adminBearerCandidates(ctx, workspaceID, ""))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("statusz %d", resp.StatusCode)
	}
	var sz agentd.StatuszResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sz); err != nil {
		return nil, err
	}
	return &sz, nil
}

// busyAlerts records last-alert time per workspace for the D6 cooldown.
// Guarded by busyAlertsMu; empty map = no cooldowns (fresh boot re-alerts).
func (h *ProxyHandler) busyAlertCooling(workspaceID string) bool {
	h.busyAlertsMu.Lock()
	defer h.busyAlertsMu.Unlock()
	last, ok := h.busyAlerts[workspaceID]
	return ok && time.Since(last) < busyAlertCooldown
}

func (h *ProxyHandler) markBusyAlerted(workspaceID string) {
	h.busyAlertsMu.Lock()
	h.busyAlerts[workspaceID] = time.Now()
	h.busyAlertsMu.Unlock()
}

func oldestSession(ages map[string]int) string {
	best, bestAge := "", -1
	for id, age := range ages {
		if age > bestAge {
			best, bestAge = id, age
		}
	}
	return best
}

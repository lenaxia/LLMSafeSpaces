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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
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

		h.sseTracker = h.newSSETracker()

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
		if h.workspaceUpdateCb != nil {
			watcher.SetWorkspaceUpdateCallback(h.workspaceUpdateCb)
		}
		if err := watcher.Start(); err != nil {
			_ = h.activityTracker.Stop()
			startErr = fmt.Errorf("starting CRD watcher: %w", err)
			return
		}
		h.watcher = watcher
		h.phaseSource = watcher
		// #902: the seed path alone cannot keep watches alive — prior-phase
		// is Redis-persisted, so post-restart seeds skip arming, and dead
		// watches have no transition event to re-arm them. The reconciler
		// heals missing watches; see its doc comment for scope limits.
		go h.sseWatchReconciler(sseWatchReconcileInterval)

		// D3 (#907): the outbox delivery worker — detached from any
		// request context; survives client disconnects (the incident's
		// message-loss class). The bridge re-resolves the workspace and
		// re-checks quota per delivery.
		if h.outbox != nil {
			if h.adapter != nil {
				h.outbox.SetVerifier(h.outboxVerify)
				h.outbox.SetOnDelivered(h.outboxOnDelivered)
				h.outbox.SetOnStaged(h.outboxOnStaged)
			}
			wctx, wcancel := context.WithCancel(context.Background())
			h.outboxCancel = wcancel
			go func() {
				<-h.stopCh
				wcancel()
			}()
			// G2 (design 0054): decay bridge-derived busy states whose
			// terminal event never arrived. Rides the outbox worker's
			// lifecycle (same lifetime, same stop signal).
			go h.v2BusyReap(wctx)
			go h.outbox.Run(wctx, h.outboxDeliver, outboxTick)
		}
	})
	return startErr
}

// sseWatchReconcileInterval is how often the SSE watch reconciler re-arms
// watches for Active workspaces. Var for tests.
var sseWatchReconcileInterval = 60 * time.Second

// sseWatchReconciler periodically re-arms SSE tracker watches for every
// Active workspace (#902). It heals MISSING watches — a seed skipped via
// Redis-persisted prior-phase, an armed watch whose subscribe goroutine
// exited, a future bug — converting permanent event-blindness into at
// most one interval. Scope limits, stated plainly (#903 review):
//   - It only adds watches (EnsureWatching is an idempotent map check);
//     it never tears down or resets connections.
//   - It cannot see ARMED-BUT-FAILING watches (goroutine alive,
//     connectAndRead failing forever at Warn with backoff — e.g. a stale
//     podIP). That class needs the tracker-state signal from #901
//     (G1/G11), not this reconciler.
func (h *ProxyHandler) sseWatchReconciler(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			// Shutdown race (#903 review): the select above may pick
			// ticker.C even when stopCh is already closed; arming after
			// Stop() would leak a retry goroutine nobody will cancel.
			select {
			case <-h.stopCh:
				return
			default:
			}
			if h.sseTracker == nil || h.phaseSource == nil {
				continue
			}
			phases := h.phaseSource.GetAllKnownPhases()
			watched := make([]string, 0, len(phases))
			for id, phase := range phases {
				if phase == string(phaseActive) {
					h.sseTracker.EnsureWatching(id)
					watched = append(watched, id)
				}
			}
			// D6 (#998): unattended-escalation sweep on the same tick —
			// notify, never execute. Failure-isolated: an escalation
			// panic must never take down the reconciler (which also
			// arms SSE watches — losing it re-creates the #902 incident
			// class).
			func() {
				defer func() {
					if r := recover(); r != nil {
						h.logger.Error("D6 escalation sweep panicked (isolated; reconciler continues)",
							fmt.Errorf("%v", r), "component", "d6")
					}
				}()
				h.escalateHungs(watched)
			}()
			// #901 G3: refresh upstream-liveness gauges while we're here
			// (no second loop/ticker for the same data).
			sse.RefreshLastEventGauges(watched)
		}
	}
}

func (h *ProxyHandler) Stop() error {
	h.stopOnce.Do(func() {
		if h.stopCh != nil {
			close(h.stopCh)
		}
		if h.sseTracker != nil {
			h.sseTracker.Stop()
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

func (h *ProxyHandler) GetSSETracker() *sse.Tracker {
	return h.sseTracker
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

// SetWorkspaceUpdateCallback installs the per-CRD-event callback that
// powers the watcher-driven auto-push of user-DEK secrets after pod
// recreation (worklog 0591). Must be called before Start().
func (h *ProxyHandler) SetWorkspaceUpdateCallback(cb workspace.WorkspaceUpdateCallback) {
	h.workspaceUpdateCb = cb
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

// SetV2QueueShadow injects the Redis-backed shadow marker for V2 queue
// visibility (US-63.10). Must be called before Start(). Pass nil to
// disable the shadow (ListQueue returns empty under V2).
func (h *ProxyHandler) SetV2QueueShadow(s *V2QueueShadow) {
	h.v2Shadow = s
}

// SetV2PendingTracker overrides the V2 pending-session tracker. Used by
// app.go to swap the in-memory tracker for a Redis-backed one when a Redis
// client is available (multi-replica support). Must be called before Start().
func (h *ProxyHandler) SetV2PendingTracker(t v2PendingTracker) {
	if t != nil {
		h.v2Pending = t
	}
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

// newSSETracker constructs the workspace SSE tracker with every callback
// wired, including the metering decoder — which rides the Adapter seam
// (design 0049 boundary: no agent wire knowledge in the tracker). The
// decoder is wired HERE, at construction, so no call site can forget it:
// a nil decoder silently zeroes all billing inference.
func (h *ProxyHandler) newSSETracker() *sse.Tracker {
	t := sse.NewTracker(h.httpClient, h.logger, h.onSessionIdle)
	if h.adapter != nil {
		t.SetMeteringDecoder(h.adapter.MeteringFromEvent)
		t.SetEventClassifier(h.adapter.IsKnownEventType)
	}
	t.SetPasswordGetter(h)
	t.SetPodIPResolver(h.getPodIPForSSE)
	t.SetOnSessionActive(h.onSessionActive)
	t.SetOnRawEvent(h.onRawEvent)
	t.SetOnAgentDied(h.onAgentDied)
	t.SetOnReconnect(h.reconcileSessionState)
	if h.tokenSeenStore != nil {
		t.SetTokenSeenStore(h.tokenSeenStore)
	}
	return t
}

// outboxTick is the outbox delivery scan interval. Var for tests.
var outboxTick = 1 * time.Second

// V2 promotion-await tunables (#1119). Production observations on
// opencode 1.18.15: `session.next.prompted` (and the user-text persist)
// land within ~1s of admission — including for defect-class deaths, which
// ALSO promote-then-die; a promotion unobserved for V2PromotionWait is a
// genuinely stranded row (park race), and the ambiguous→verifying→re-admit
// path is the bounded nudge. Vars for tests.
var (
	// V2PromotionWait bounds the admission→promotion await (#1119).
	// Exported read via V2PromotionAwaitBudget so app wiring can size
	// outbox.DeliveryTimeout around it (the wait runs INSIDE the
	// deliverer's detached context).
	V2PromotionWait = 30 * time.Second
	v2PromotionPoll = 2 * time.Second
)

// V2PromotionAwaitBudget reports the current promotion-await window
// (app wiring sizes the V2 delivery budget = window + margin).
func V2PromotionAwaitBudget() time.Duration { return V2PromotionWait }

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
// llm_request usage) on the synchronous, verified, and re-send paths.
//
// Two #987 rules:
//
//  1. Never re-send unverified: any re-send (Attempts > 0 — a prior
//     attempt existed) verifies against the transcript first. opencode
//     persists the user message BEFORE the turn runs, so presence of
//     the exact text at/after the send window proves the prior attempt
//     landed and a re-send would duplicate the whole turn (the
//     sent-once/delivered-3x incident). Subsumes the D3 r2
//     pre-redelivery tail-25 check with exact cursor-paged coverage.
//
//  2. Classify failures: an HTTP status response
//     (agent.ErrHTTPStatus) means the agent PROCESSED and rejected
//     the request — definitive, safe to retry. Anything else (timeout
//     mid-turn, connection cut mid-flight) is outcome-UNKNOWN and wraps
//     outbox.Ambiguous: the outbox verifies instead of blind-retrying.
func (h *ProxyHandler) outboxDeliver(ctx context.Context, workspaceID, sessionID string, e outbox.Entry) error {
	if h.agentdTerminus {
		return h.agentdTerminusDeliver(ctx, workspaceID, sessionID, e)
	}
	if e.Attempts > 0 || e.VerifyAttempts > 0 {
		if h.outboxVerify(ctx, workspaceID, sessionID, e) == outbox.VerdictDelivered {
			return nil // prior attempt confirmed in the transcript — complete without re-sending
		}
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
		// returns in milliseconds. But admission ≠ delivery (#1119):
		// the user text persists at PROMOTION, and a defect-class
		// death (model-resolve failure, park race) consumes or strands
		// the admitted row with no signal — completing at pickup was
		// the silent-drop bug (production 10:24Z). The deliverer now
		// waits for promotion, proven by the persisted text via the
		// same oracle the #987 verifier uses, before completing. If
		// the window expires unobserved, the outcome is unknown →
		// ambiguous → the verifying machinery resolves it: text
		// present (late promotion) completes; absent-after-window
		// re-admits (the bounded nudge) — never a blind complete.
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
		admittedAt := time.Now().UTC()
		msgID, err := h.adapter.SendAsync(ctx, "", workspaceID, sessionID, e.Text, opts)
		if err != nil {
			if errors.Is(err, agent.ErrHTTPStatus) {
				return err
			}
			return outbox.Ambiguous(err)
		}
		_ = msgID // correlation key for the events-based fast path (follow-up)
		verifier, hasVerifier := h.adapter.(deliveryVerifier)
		deadline := time.Now().UTC().Add(V2PromotionWait)
		for {
			if hasVerifier {
				if delivered, _, verr := verifier.VerifyDelivery(ctx, "", workspaceID, sessionID, e.Text, admittedAt); verr == nil && delivered {
					return nil // promotion observed — real delivery
				}
			}
			if !time.Now().UTC().Before(deadline) {
				return outbox.Ambiguous(fmt.Errorf("v2 promotion not observed within %s (admission msg %q)", V2PromotionWait, msgID))
			}
			select {
			case <-ctx.Done():
				return outbox.Ambiguous(ctx.Err())
			case <-time.After(v2PromotionPoll):
			}
		}
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
			pw, err := h.getPassword(ctx, ws)
			if err != nil {
				return "", "", err
			}
			v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
			if v1Err != nil {
				return "", "", v1Err
			}
			wsObj, err := v1Client.Workspaces(h.namespace).Get(ctx, ws, metav1.GetOptions{})
			if err != nil {
				return "", "", err
			}
			if wsObj.Status.PodIP == "" {
				return "", "", fmt.Errorf("agentd terminus: no pod IP for %s (phase %s)", ws, wsObj.Status.Phase)
			}
			return fmt.Sprintf("http://%s:%d", wsObj.Status.PodIP, agentd.AgentdPort), pw, nil
		},
	}
	return d.deliver(ctx, workspaceID, sessionID, e)
}

// SetV2Delivery enables V2 admit-and-return outbox delivery (design
// 0052, OPENCODE_V2_DELIVERY). Must be called before Start(), and the
// adapter must be constructed with WithV2Store(true) in the same
// wiring — delivery and verification must agree on the store, or the
// verifier reports false absence and re-sends (the #987 duplicate
// class).
func (h *ProxyHandler) SetV2Delivery(enabled bool) {
	h.v2Delivery = enabled
}

// deliveryVerifier is the #987/#1119 ambiguity oracle. Now ALSO on
// agent.Adapter (promoted 2026-08-29: production adapters are wrapped by
// systemnotices.Wrap, and this local assertion silently failed against
// the wrapper — every verification returned inconclusive without
// touching the network, breaking all V2 delivery for hours). The local
// interface stays as the compile-time shape contract at the call sites.
type deliveryVerifier interface {
	VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (delivered, definitive bool, err error)
}

// outboxVerify resolves delivery ambiguity against the agent
// transcript: cursor-paged coverage of the send window (adapter.
// VerifyDelivery), delivered-proof even while the turn still runs.
// Inconclusive (agent unreachable, coverage incomplete, or a non-
// opencode adapter without the verifier) never blocks a re-send
// attempt by itself — the send's own failure classification re-enters
// verification.
func (h *ProxyHandler) outboxVerify(ctx context.Context, workspaceID, sessionID string, e outbox.Entry) outbox.Verdict {
	v, ok := h.adapter.(deliveryVerifier)
	if !ok {
		// Unreachable since VerifyDelivery moved onto agent.Adapter
		// (wrappers inherit it via embedding) — but the 2026-08-29
		// incident showed this branch failing SILENTLY for hours, so
		// it screams if it ever happens again.
		h.logger.Error("outbox verify: adapter does not implement deliveryVerifier — all verification is inconclusive",
			fmt.Errorf("adapter %T lacks VerifyDelivery", h.adapter))
		return outbox.VerdictInconclusive
	}
	since := e.LastAttemptAt
	if since.IsZero() {
		// Crash-recovered entry: the send started sometime after
		// accept — AcceptedAt is the safe floor.
		since = e.AcceptedAt
	}
	delivered, definitive, err := v.VerifyDelivery(ctx, "", workspaceID, sessionID, e.Text, since)
	switch {
	case err != nil:
		// #1119 follow-up 2: verify errors were silent in the first
		// live-traffic incident — seven inconclusive passes against a
		// healthy agent left no log line. Surface them: an unreachable
		// or wedged verify path is ops signal, not noise (cadence is
		// bounded by the verify backoff ladder).
		h.logger.Warn("outbox verify inconclusive: transport error",
			"workspaceID", workspaceID, "sessionID", sessionID,
			"entryID", e.ID, "error", err)
		return outbox.VerdictInconclusive
	case delivered:
		return outbox.VerdictDelivered
	case definitive:
		return outbox.VerdictAbsent
	default:
		return outbox.VerdictInconclusive
	}
}

// outboxOnDelivered is the single confirmed-delivery seam: cross-cutting
// events (activity, session-index, metering) plus the queue.update/sent
// SSE that clears the frontend pill deterministically (#987 — the
// outbox path previously never emitted it).
func (h *ProxyHandler) outboxOnDelivered(workspaceID, sessionID string, e outbox.Entry) {
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
		podIP := h.getPodIPForSSE(wid)
		if podIP == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

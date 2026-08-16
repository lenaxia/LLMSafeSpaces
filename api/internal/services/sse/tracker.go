// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

const sseIdleTimeout = 5 * time.Minute

type SessionIdleCallback func(workspaceID, sessionID string)

type RawEventCallback func(workspaceID, eventType, rawData string)

type InferenceCallback func(workspaceID, modelID, providerID string, inputTokens, outputTokens int64, costDollars float64)

// AgentDiedCallback is invoked when an upstream SSE stream ends after at least
// one byte of data has been received, signaling that the agent process died
// mid-stream (OOM, crash, or restart). The tracker cannot distinguish a real
// death from a normal opencode restart — see US-44.1a/c for the accepted
// false-positive tradeoff.
type AgentDiedCallback func(workspaceID string)

// ReconnectCallback is called at the start of each connection attempt, after
// the pod IP and password are resolved but before the SSE stream is opened.
// podIP is the raw IP (no port). password is the workspace password (used as
// Bearer token on the agentd admin port).
// Intended use: query /v1/statusz to reconcile any sessions that went idle
// while the SSE connection was down, and drain their queues.
type ReconnectCallback func(workspaceID, podIP, password string)

type SessionMetricsRecorder interface {
	RecordSessionCompleted(workspaceID string, durationSeconds float64)
}

type sseEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type opencodeEvent struct {
	Payload struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	} `json:"payload"`
}

type Tracker struct {
	HttpClient       *http.Client
	Logger           pkginterfaces.LoggerInterface
	onSessionIdle    SessionIdleCallback
	onSessionActive  SessionIdleCallback
	onRawEvent       RawEventCallback
	onInference      InferenceCallback
	onReconnect      ReconnectCallback
	onAgentDied      AgentDiedCallback
	idleTimeout      time.Duration
	tokensMu         sync.Mutex
	sessionTokenSeen map[string]int64
	sessionCostSeen  map[string]float64
	startTimeMu      sync.Mutex
	sessionStartTime map[string]time.Time
	sessionMetrics   SessionMetricsRecorder
	subscriptions    map[string]context.CancelFunc
	subMu            sync.Mutex
	goroutineWg      map[string]*sync.WaitGroup
	passwordGetter   interfaces.WorkspacePasswordProvider
	podIPResolver    func(workspaceID string) string
	drainMu          sync.Mutex
	drainSubs        map[string]map[uint64]*drainSub
	drainSubCounter  uint64
}

type drainSub struct {
	onIdle   func(workspaceID, sessionID string)
	onActive func(workspaceID, sessionID string)
}

func NewTracker(
	httpClient *http.Client,
	logger pkginterfaces.LoggerInterface,
	onSessionIdle SessionIdleCallback,
) *Tracker {
	registerWatchedGauge()
	return &Tracker{
		HttpClient:       httpClient,
		Logger:           logger,
		onSessionIdle:    onSessionIdle,
		idleTimeout:      sseIdleTimeout,
		subscriptions:    make(map[string]context.CancelFunc),
		goroutineWg:      make(map[string]*sync.WaitGroup),
		sessionTokenSeen: make(map[string]int64),
		sessionCostSeen:  make(map[string]float64),
		sessionStartTime: make(map[string]time.Time),
	}
}

func (t *Tracker) SetPasswordGetter(provider interfaces.WorkspacePasswordProvider) {
	t.passwordGetter = provider
}

func (t *Tracker) SetPodIPResolver(resolver func(workspaceID string) string) {
	t.podIPResolver = resolver
}

func (t *Tracker) SetOnSessionActive(callback SessionIdleCallback) {
	t.onSessionActive = callback
}

func (t *Tracker) SetOnReconnect(callback ReconnectCallback) {
	t.onReconnect = callback
}

func (t *Tracker) SetOnInference(cb InferenceCallback) {
	t.onInference = cb
}

func (t *Tracker) SetSessionMetrics(r SessionMetricsRecorder) {
	t.sessionMetrics = r
}

func (t *Tracker) SetOnRawEvent(callback RawEventCallback) {
	t.onRawEvent = callback
}

func (t *Tracker) SetOnAgentDied(cb AgentDiedCallback) {
	t.onAgentDied = cb
}

// SetIdleTimeout overrides the SSE idle timeout. Primarily for tests; production
// uses the package default (sseIdleTimeout).
func (t *Tracker) SetIdleTimeout(d time.Duration) {
	t.idleTimeout = d
}

// lastEventMu guards lastEvent (workspace -> last upstream event time).
// Backs llmsafespaces_sse_tracker_last_event_age_seconds (#901 G3):
// receiving client heartbeats proves NOTHING about the upstream tracker
// (they are generated per-subscriber in proxy_stream.go) — this gauge is
// the upstream-liveness signal.
var (
	lastEventMu sync.Mutex
	lastEvent   = map[string]time.Time{}

	lastEventAgeGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmsafespaces_sse_tracker_last_event_age_seconds",
		Help: "Seconds since the tracker last received an upstream agent event per workspace (stale/large = connected-but-silent or dead upstream)",
	}, []string{"workspace_id"})

	lastEventGaugeOnce sync.Once
)

func recordLastEvent(workspaceID string) {
	lastEventMu.Lock()
	lastEvent[workspaceID] = time.Now()
	lastEventMu.Unlock()
}

// RefreshLastEventGauges recomputes the age gauges for the given
// workspaces (stale entries included — a workspace with no recent events
// is exactly the signal). Called from the watch reconciler each tick.
func RefreshLastEventGauges(workspaceIDs []string) {
	lastEventGaugeOnce.Do(func() { prometheus.MustRegister(lastEventAgeGauge) })
	lastEventMu.Lock()
	defer lastEventMu.Unlock()
	now := time.Now()
	for _, id := range workspaceIDs {
		t, ok := lastEvent[id]
		if !ok {
			// Watched but never received: report since process start so
			// the silence is visible rather than absent.
			lastEventAgeGauge.WithLabelValues(id).Set(math.Max(300, now.Sub(processStart).Seconds()))
			continue
		}
		lastEventAgeGauge.WithLabelValues(id).Set(now.Sub(t).Seconds())
	}
}

var processStart = time.Now()

// Per-workspace connection state (#901 G1 — the issue's actual ask; the
// aggregate watched-count gauge from #903 cannot see armed-but-failing
// watches): connected=1 while an /event stream is open (HTTP 200 read
// loop active), 0 otherwise; reconnects counts successful (re)connects.
var (
	connStateMu sync.Mutex
	connState   = map[string]bool{}

	trackerConnectedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "llmsafespaces_sse_tracker_connected",
		Help: "1 while this replica holds an open /event stream for the workspace (armed-but-failing watches read 0)",
	}, []string{"workspace_id"})

	trackerReconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_sse_tracker_reconnects_total",
		Help: "Successful (re)connections to a workspace /event stream",
	}, []string{"workspace_id"})

	connGaugesOnce sync.Once
)

func setTrackerConnected(workspaceID string, up bool) {
	connGaugesOnce.Do(func() {
		prometheus.MustRegister(trackerConnectedGauge)
		prometheus.MustRegister(trackerReconnects)
	})
	connStateMu.Lock()
	connState[workspaceID] = up
	connStateMu.Unlock()
	trackerConnectedGauge.WithLabelValues(workspaceID).Set(boolToFloat(up))
	if up {
		trackerReconnects.WithLabelValues(workspaceID).Inc()
	}
}

// deleteTrackerSeries removes a workspace's gauge series (#906 review:
// stale series fired UpstreamSilent forever on suspended workspaces).
func deleteTrackerSeries(workspaceID string) {
	connStateMu.Lock()
	delete(connState, workspaceID)
	connStateMu.Unlock()
	lastEventMu.Lock()
	delete(lastEvent, workspaceID)
	lastEventMu.Unlock()
	trackerConnectedGauge.DeleteLabelValues(workspaceID)
	lastEventAgeGauge.DeleteLabelValues(workspaceID)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// watchedCount tracks live tracker subscriptions across all Tracker
// instances in this process; exported as the
// llmsafespaces_sse_tracker_watched_workspaces gauge (#902 fix item 3,
// #901 G1 minimal slice — "how many workspaces does THIS API replica
// actually watch" was tonight's blind spot).
var watchedCount atomic.Int64

var registerWatchedGaugeOnce sync.Once

func registerWatchedGauge() {
	registerWatchedGaugeOnce.Do(func() {
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "llmsafespaces_sse_tracker_watched_workspaces",
				Help: "Number of workspace SSE watches this API replica currently holds (0 for a replica means user streams on it are event-blind)",
			},
			func() float64 { return float64(watchedCount.Load()) },
		))
	})
}

func (t *Tracker) EnsureWatching(workspaceID string) {
	t.subMu.Lock()
	defer t.subMu.Unlock()

	if _, exists := t.subscriptions[workspaceID]; exists {
		return
	}

	//nolint:gosec // G118 false positive; cancel stored in subscriptions map
	ctx, cancel := context.WithCancel(context.Background())
	t.subscriptions[workspaceID] = cancel
	watchedCount.Add(1)
	t.Logger.Info("SSE watch armed", "workspaceID", workspaceID)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	t.goroutineWg[workspaceID] = wg

	go func() {
		defer wg.Done()
		t.subscribe(ctx, workspaceID)
	}()
}

// ForceWatchingForTest arms a watch without connecting — tests use it to
// simulate a pre-existing (possibly stale) subscription. The cancel
// function is a no-op: StopWatching deletes the map entry regardless.
func (t *Tracker) ForceWatchingForTest(workspaceID string) {
	t.ForceWatchingWithCancelForTest(workspaceID, func() {})
}

// ForceWatchingWithCancelForTest is ForceWatchingForTest with an
// caller-supplied cancel, so tests can observe StopWatching actually
// canceling a live subscription (the transition fresh-connection
// semantics, #903 review).
func (t *Tracker) ForceWatchingWithCancelForTest(workspaceID string, cancel context.CancelFunc) {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	if _, exists := t.subscriptions[workspaceID]; !exists {
		t.subscriptions[workspaceID] = cancel
		watchedCount.Add(1)
	}
}

// IsWatching returns true if the tracker has an active SSE subscription
// for the given workspace. Used by tests to verify that read-path
// handlers trigger SSE watch (#755 stuck-busy regression).
func (t *Tracker) IsWatching(workspaceID string) bool {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	_, exists := t.subscriptions[workspaceID]
	return exists
}

func (t *Tracker) StopWatching(workspaceID string) {
	t.subMu.Lock()
	if cancel, exists := t.subscriptions[workspaceID]; exists {
		cancel()
		delete(t.subscriptions, workspaceID)
		watchedCount.Add(-1)
		t.Logger.Info("SSE watch stopped", "workspaceID", workspaceID)
	}
	wg := t.goroutineWg[workspaceID]
	delete(t.goroutineWg, workspaceID)

	// Wait for the subscribe goroutine to exit while still holding subMu.
	// This prevents a concurrent EnsureWatching from starting a new goroutine
	// that writes to billing maps before cleanup completes. subscribe() never
	// acquires subMu, so no deadlock risk.
	if wg != nil {
		wg.Wait()
	}

	// #906 review: drop per-workspace gauge series so suspended workspaces
	// stop emitting (permanently-firing UpstreamSilent).
	deleteTrackerSeries(workspaceID)

	prefix := workspaceID + ":"
	t.tokensMu.Lock()
	for k := range t.sessionTokenSeen {
		if strings.HasPrefix(k, prefix) {
			delete(t.sessionTokenSeen, k)
		}
	}
	for k := range t.sessionCostSeen {
		if strings.HasPrefix(k, prefix) {
			delete(t.sessionCostSeen, k)
		}
	}
	t.tokensMu.Unlock()

	t.startTimeMu.Lock()
	for k := range t.sessionStartTime {
		if strings.HasPrefix(k, prefix) {
			delete(t.sessionStartTime, k)
		}
	}
	t.startTimeMu.Unlock()

	t.subMu.Unlock()
}

func (t *Tracker) Stop() {
	t.subMu.Lock()
	for id, cancel := range t.subscriptions {
		cancel()
		delete(t.subscriptions, id)
	}
	wgs := make([]*sync.WaitGroup, 0, len(t.goroutineWg))
	for id, wg := range t.goroutineWg {
		wgs = append(wgs, wg)
		delete(t.goroutineWg, id)
	}
	t.subMu.Unlock()

	for _, wg := range wgs {
		wg.Wait()
	}
}

func (t *Tracker) SubscriptionCount() int {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	return len(t.subscriptions)
}

// GetBillingState returns entries from the three billing maps that match the
// given workspace prefix. Used by integration tests to verify cleanup.
func (t *Tracker) GetBillingState(workspaceID string) (tokens, costs, startTimes map[string]bool) {
	prefix := workspaceID + ":"
	tokens = make(map[string]bool)
	costs = make(map[string]bool)
	startTimes = make(map[string]bool)

	t.tokensMu.Lock()
	for k := range t.sessionTokenSeen {
		if strings.HasPrefix(k, prefix) {
			tokens[k] = true
		}
	}
	for k := range t.sessionCostSeen {
		if strings.HasPrefix(k, prefix) {
			costs[k] = true
		}
	}
	t.tokensMu.Unlock()

	t.startTimeMu.Lock()
	for k := range t.sessionStartTime {
		if strings.HasPrefix(k, prefix) {
			startTimes[k] = true
		}
	}
	t.startTimeMu.Unlock()

	return
}

func (t *Tracker) SubscribeDrain(
	workspaceID string,
	onIdle func(workspaceID, sessionID string),
	onActive func(workspaceID, sessionID string),
) (cancel func()) {
	t.drainMu.Lock()
	defer t.drainMu.Unlock()

	if t.drainSubs == nil {
		t.drainSubs = make(map[string]map[uint64]*drainSub)
	}
	if t.drainSubs[workspaceID] == nil {
		t.drainSubs[workspaceID] = make(map[uint64]*drainSub)
	}
	t.drainSubCounter++
	id := t.drainSubCounter
	t.drainSubs[workspaceID][id] = &drainSub{onIdle: onIdle, onActive: onActive}

	return func() {
		t.drainMu.Lock()
		defer t.drainMu.Unlock()
		delete(t.drainSubs[workspaceID], id)
		if len(t.drainSubs[workspaceID]) == 0 {
			delete(t.drainSubs, workspaceID)
		}
	}
}

// healthyConnBackoffReset is how long a connection must live before its
// ending earns a backoff reset. Var for tests.
var healthyConnBackoffReset = 30 * time.Second

// backoffAfterConnect computes the next retry backoff after a
// connectAndRead returned. connectAndRead ALWAYS returns non-nil (even a
// clean stream end is an error return), so a reset-on-nil branch was dead
// code and a long-lived healthy connection that ended (pod restart) kept
// the maxed 30s backoff forever (#903 review; pinned by
// TestBackoffAfterConnect).
func backoffAfterConnect(cur time.Duration, connDuration time.Duration) time.Duration {
	if connDuration > healthyConnBackoffReset {
		return 2 * time.Second
	}
	return cur
}

func (t *Tracker) subscribe(ctx context.Context, workspaceID string) {
	backoff := 2 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		started := time.Now()
		err := t.connectAndRead(ctx, workspaceID)
		backoff = backoffAfterConnect(backoff, time.Since(started))
		if err != nil {
			// Warn, not Debug (#901 G2 / #902 fix item 3): a workspace whose
			// tracker cannot connect is EVENT-BLIND — users halt while sends
			// keep succeeding. Rate-limited by the backoff below (max one
			// line per 30s per workspace).
			t.Logger.Warn("SSE subscription ended; retrying", "error", err, "workspaceID", workspaceID, "backoff", backoff.String())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (t *Tracker) connectAndRead(ctx context.Context, workspaceID string) error {
	if t.passwordGetter == nil {
		return fmt.Errorf("password getter not configured")
	}

	if t.podIPResolver == nil {
		return fmt.Errorf("pod IP resolver not configured")
	}

	podIP := t.podIPResolver(workspaceID)
	if podIP == "" {
		return fmt.Errorf("no pod IP for workspace %s", workspaceID)
	}

	password, err := t.passwordGetter.WorkspacePassword(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("getting password for SSE: %w", err)
	}

	// Reconcile any sessions that went idle while this connection was down.
	// Must happen before opening the SSE stream so that if the stream
	// immediately delivers a busy event, we don't double-drain.
	if t.onReconnect != nil {
		t.onReconnect(workspaceID, podIP, password)
	}

	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	idleTimer := time.AfterFunc(t.idleTimeout, cancelIdle)
	defer idleTimer.Stop()

	targetURL := fmt.Sprintf("http://%s:%d/event", podIP, agentd.AgentPort)
	req, err := http.NewRequestWithContext(idleCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return fmt.Errorf("creating SSE request: %w", err)
	}
	req.SetBasicAuth("opencode", password)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := t.HttpClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connection: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE endpoint returned status %d", resp.StatusCode)
	}
	setTrackerConnected(workspaceID, true)
	defer setTrackerConnected(workspaceID, false)

	scanner := bufio.NewScanner(resp.Body)
	// 16 MB buffer per line (was 64 KB). Same root cause as the agentd
	// SSE scanner bug (#805): opencode emits message.part.updated events
	// that exceed 300KB (patch parts listing thousands of files), and a
	// large monorepo could produce 10MB+. The old 64 KB cap caused the
	// scanner to fail silently, dropping the SSE connection and causing
	// the API-side tracker to miss session.status:idle events — the
	// stuck-busy bug. 16 MB matches the agentd-side fix.
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	var eventData strings.Builder
	var bytesReceived int64
	// bytesReceived counts len(scanner.Text()) — line content with the trailing
	// newline stripped by ScanLines. This diverges from US-44.1a's raw
	// resp.Body.Read byte count, but only the > 0 threshold matters and opencode
	// always emits real event data before any termination.
	for scanner.Scan() {
		idleTimer.Reset(t.idleTimeout)

		line := scanner.Text()
		bytesReceived += int64(len(line))

		if strings.HasPrefix(line, "data: ") {
			eventData.WriteString(strings.TrimPrefix(line, "data: "))
			eventData.WriteString("\n")
		} else if line == "" && eventData.Len() > 0 {
			t.processEvent(workspaceID, eventData.String())
			eventData.Reset()
		}
	}

	// Non-EOF read error (TCP RST, bufio.ErrTooLong) after data was received.
	// context.Canceled means idleCtx or parent ctx was canceled — handled by
	// the idleCtx.Err() check below. A network blip must not be reported as an
	// agent death; aligns with US-44.1a's network-vs-death distinction.
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("SSE scanner error for workspace %s: %w", workspaceID, err)
	}

	if idleCtx.Err() != nil {
		return fmt.Errorf("SSE idle timeout for workspace %s", workspaceID)
	}
	if bytesReceived > 0 && t.onAgentDied != nil {
		t.onAgentDied(workspaceID)
	}
	return fmt.Errorf("SSE stream ended for workspace %s", workspaceID)
}

func (t *Tracker) ProcessEvent(workspaceID, data string) {
	t.processEvent(workspaceID, data)
}

func (t *Tracker) processEvent(workspaceID, data string) {
	data = strings.TrimSpace(data)
	if data == "" {
		return
	}
	recordLastEvent(workspaceID)

	var evt sseEvent
	if err := json.Unmarshal([]byte(data), &evt); err != nil || evt.Type == "" {
		var nested opencodeEvent
		if json.Unmarshal([]byte(data), &nested) == nil && nested.Payload.Type != "" {
			if t.onRawEvent != nil {
				t.onRawEvent(workspaceID, nested.Payload.Type, data)
			}
			t.dispatchProperties(workspaceID, nested.Payload.Type, nested.Payload.Properties)
		}
		return
	}

	if t.onRawEvent != nil {
		t.onRawEvent(workspaceID, evt.Type, data)
	}
	t.dispatchProperties(workspaceID, evt.Type, evt.Properties)
}

func (t *Tracker) DispatchProperties(workspaceID, eventType string, props json.RawMessage) {
	t.dispatchProperties(workspaceID, eventType, props)
}

func (t *Tracker) dispatchProperties(workspaceID, eventType string, props json.RawMessage) {
	if eventType == "session.updated" && len(props) > 0 && t.onInference != nil {
		t.handleSessionUpdated(workspaceID, props)
	}
	if eventType != "session.status" || len(props) == 0 {
		return
	}

	var p struct {
		SessionID string `json:"sessionID"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	if json.Unmarshal(props, &p) != nil || p.SessionID == "" {
		return
	}

	switch p.Status.Type {
	case "idle":
		if t.sessionMetrics != nil {
			timeKey := workspaceID + ":" + p.SessionID
			t.startTimeMu.Lock()
			if start, ok := t.sessionStartTime[timeKey]; ok {
				delete(t.sessionStartTime, timeKey)
				t.startTimeMu.Unlock()
				t.sessionMetrics.RecordSessionCompleted(workspaceID, time.Since(start).Seconds())
			} else {
				t.startTimeMu.Unlock()
			}
		}
		if t.onSessionIdle != nil {
			t.onSessionIdle(workspaceID, p.SessionID)
		}
		t.drainMu.Lock()
		subs := make([]*drainSub, 0, len(t.drainSubs[workspaceID]))
		for _, s := range t.drainSubs[workspaceID] {
			subs = append(subs, s)
		}
		t.drainMu.Unlock()
		for _, s := range subs {
			s.onIdle(workspaceID, p.SessionID)
		}
	case "busy", "retry":
		timeKey := workspaceID + ":" + p.SessionID
		t.startTimeMu.Lock()
		if _, exists := t.sessionStartTime[timeKey]; !exists {
			t.sessionStartTime[timeKey] = time.Now()
		}
		t.startTimeMu.Unlock()
		if t.onSessionActive != nil {
			t.onSessionActive(workspaceID, p.SessionID)
		}
		t.drainMu.Lock()
		subs := make([]*drainSub, 0, len(t.drainSubs[workspaceID]))
		for _, s := range t.drainSubs[workspaceID] {
			subs = append(subs, s)
		}
		t.drainMu.Unlock()
		for _, s := range subs {
			s.onActive(workspaceID, p.SessionID)
		}
	}
}

func (t *Tracker) handleSessionUpdated(workspaceID string, props []byte) {
	var p struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			ID    string `json:"id"`
			Model struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
				Provider   string `json:"provider"`
			} `json:"model"`
			Tokens struct {
				Input  int64 `json:"input"`
				Output int64 `json:"output"`
			} `json:"tokens"`
			Cost json.RawMessage `json:"cost"`
		} `json:"info"`
	}
	if err := json.Unmarshal(props, &p); err != nil {
		t.Logger.Warn("handleSessionUpdated: failed to parse event", "workspaceID", workspaceID, "error", err)
		return
	}
	if p.Info.ID == "" || p.Info.Tokens.Output == 0 || p.Info.Model.ID == "" {
		t.Logger.Warn("handleSessionUpdated: dropping session.updated with incomplete billing fields",
			"workspaceID", workspaceID, "sessionID", p.Info.ID,
			"hasModel", p.Info.Model.ID != "", "outputTokens", p.Info.Tokens.Output)
		return
	}

	providerID := p.Info.Model.ProviderID
	if providerID == "" {
		providerID = p.Info.Model.Provider
	}

	costVal := 0.0
	if len(p.Info.Cost) > 0 {
		trimmed := bytes.TrimSpace(p.Info.Cost)
		// Try as a plain number first (1.15.x wire shape): "cost": 0.042
		var costFloat float64
		if json.Unmarshal(trimmed, &costFloat) == nil {
			costVal = costFloat
		} else {
			// Try as an object (potential 1.18.10 wire shape).
			// In ocCost, "cost" is CostUSD (dollar amount), while
			// "total" is TotalTokens (int64 count). Extract the
			// dollar field, not the token count.
			var costObj struct {
				Cost float64 `json:"cost"`
			}
			if json.Unmarshal(trimmed, &costObj) == nil {
				costVal = costObj.Cost
			} else {
				t.Logger.Warn("handleSessionUpdated: could not parse cost field",
					"workspaceID", workspaceID, "raw", string(trimmed))
			}
		}
	}

	key := workspaceID + ":" + p.Info.ID
	t.tokensMu.Lock()
	prevOutput := t.sessionTokenSeen[key]
	if p.Info.Tokens.Output <= prevOutput {
		t.tokensMu.Unlock()
		return
	}
	prevCost := t.sessionCostSeen[key]
	t.sessionTokenSeen[key] = p.Info.Tokens.Output
	t.sessionCostSeen[key] = costVal
	t.tokensMu.Unlock()

	outputDelta := p.Info.Tokens.Output - prevOutput
	inputTokens := p.Info.Tokens.Input
	if prevOutput > 0 {
		inputTokens = 0
	}
	costDelta := costVal - prevCost
	if costDelta < 0 {
		costDelta = 0
	}
	t.onInference(workspaceID, p.Info.Model.ID, providerID, inputTokens, outputDelta, costDelta)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

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
	return &Tracker{
		HttpClient:       httpClient,
		Logger:           logger,
		onSessionIdle:    onSessionIdle,
		idleTimeout:      sseIdleTimeout,
		subscriptions:    make(map[string]context.CancelFunc),
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

func (t *Tracker) EnsureWatching(workspaceID string) {
	t.subMu.Lock()
	defer t.subMu.Unlock()

	if _, exists := t.subscriptions[workspaceID]; exists {
		return
	}

	//nolint:gosec // G118 false positive; cancel stored in subscriptions map
	ctx, cancel := context.WithCancel(context.Background())
	t.subscriptions[workspaceID] = cancel

	go t.subscribe(ctx, workspaceID)
}

func (t *Tracker) StopWatching(workspaceID string) {
	t.subMu.Lock()
	defer t.subMu.Unlock()

	if cancel, exists := t.subscriptions[workspaceID]; exists {
		cancel()
		delete(t.subscriptions, workspaceID)
	}

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
}

func (t *Tracker) Stop() {
	t.subMu.Lock()
	defer t.subMu.Unlock()

	for id, cancel := range t.subscriptions {
		cancel()
		delete(t.subscriptions, id)
	}
}

func (t *Tracker) SubscriptionCount() int {
	t.subMu.Lock()
	defer t.subMu.Unlock()
	return len(t.subscriptions)
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

func (t *Tracker) subscribe(ctx context.Context, workspaceID string) {
	backoff := 2 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := t.connectAndRead(ctx, workspaceID); err != nil {
			t.Logger.Debug("SSE subscription ended", "error", err, "workspaceID", workspaceID)
		} else {
			backoff = 2 * time.Second
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

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
			t.startTimeMu.Lock()
			if start, ok := t.sessionStartTime[p.SessionID]; ok {
				delete(t.sessionStartTime, p.SessionID)
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
		t.startTimeMu.Lock()
		if _, exists := t.sessionStartTime[p.SessionID]; !exists {
			t.sessionStartTime[p.SessionID] = time.Now()
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
		t.Logger.Debug("handleSessionUpdated: failed to parse event", "error", err)
		return
	}
	if p.Info.ID == "" || p.Info.Tokens.Output == 0 || p.Info.Model.ID == "" {
		return
	}

	providerID := p.Info.Model.ProviderID
	if providerID == "" {
		providerID = p.Info.Model.Provider
	}

	costVal := 0.0
	if len(p.Info.Cost) > 0 {
		var costFloat float64
		if json.Unmarshal(p.Info.Cost, &costFloat) == nil {
			costVal = costFloat
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

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

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

type providerCache struct {
	mu            sync.Mutex
	connected     []string
	configured    int
	sessions      []agentd.SessionInfo
	lastFetchedAt time.Time
}

// sessionStatusTracker subscribes to opencode's SSE stream and tracks busy/idle per session
// and per-session prompt tokens from step-finish events.
type sessionStatusTracker struct {
	mu           sync.RWMutex
	statuses     map[string]string // session ID → "busy" | "idle"
	promptTokens map[string]int64  // session ID → current context size (input + cache.read + cache.write)
}

func newSessionStatusTracker() *sessionStatusTracker {
	return &sessionStatusTracker{
		statuses:     make(map[string]string),
		promptTokens: make(map[string]int64),
	}
}

func (t *sessionStatusTracker) set(sessionID, status string) {
	t.mu.Lock()
	t.statuses[sessionID] = status
	t.mu.Unlock()
}

func (t *sessionStatusTracker) get(sessionID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if s, ok := t.statuses[sessionID]; ok {
		return s
	}
	return "idle"
}

// hasAnyBusy returns true if any tracked session is currently "busy".
// Used by the session-aware restart mechanism (US-44.2) to decide
// whether to defer an opencode restart until sessions are idle.
func (t *sessionStatusTracker) hasAnyBusy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.statuses {
		if s == "busy" {
			return true
		}
	}
	return false
}

// listBusy returns the IDs of all sessions currently marked "busy".
// Used for logging which sessions are blocking a deferred restart.
func (t *sessionStatusTracker) listBusy() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var busy []string
	for id, s := range t.statuses {
		if s == "busy" {
			busy = append(busy, id)
		}
	}
	return busy
}

// hasAnyData returns true if the tracker has tracked at least one
// session. Used by the session-aware restart logic to detect the
// SSE-disconnect case: an empty tracker means no session.status events
// have been received, so we cannot safely defer a restart.
func (t *sessionStatusTracker) hasAnyData() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.statuses) > 0
}

// snapshot returns the current busy session count and total prompt
// tokens across all sessions. Used by the ops metrics loop to update
// Prometheus gauges without holding the lock for multiple calls.
func (t *sessionStatusTracker) snapshot() (busyCount int, totalTokens int64) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.statuses {
		if s == "busy" {
			busyCount++
		}
	}
	for _, tok := range t.promptTokens {
		totalTokens += tok
	}
	return busyCount, totalTokens
}

// prune removes entries for sessions that no longer exist.
func (t *sessionStatusTracker) prune(activeIDs []string) {
	active := make(map[string]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		active[id] = struct{}{}
	}
	t.mu.Lock()
	for id := range t.statuses {
		if _, exists := active[id]; !exists {
			delete(t.statuses, id)
			delete(t.promptTokens, id)
		}
	}
	t.mu.Unlock()
}

func (t *sessionStatusTracker) setPromptTokens(sessionID string, tokens int64) {
	t.mu.Lock()
	t.promptTokens[sessionID] = tokens
	t.mu.Unlock()
}

func (t *sessionStatusTracker) getPromptTokens(sessionID string) int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.promptTokens[sessionID]
}

func (t *sessionStatusTracker) hasPromptTokens(sessionID string) bool {
	t.mu.RLock()
	_, ok := t.promptTokens[sessionID]
	t.mu.RUnlock()
	return ok
}

func (t *sessionStatusTracker) subscribe(ctx context.Context, client *OpenCodeClient) {
	backoff := 2 * time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := t.connectAndRead(ctx, client)
		if err != nil && ctx.Err() == nil {
			log.Debug("SSE stream ended", zap.Error(err))
		}
		// Reconcile authoritative session statuses after every reconnect
		// to heal stale-idle entries (#753 F1). If the SSE stream dropped
		// after a session went busy, opencode does not re-emit the busy
		// event — the tracker would retain stale "idle", causing a
		// credential push to restart mid-turn.
		if ctx.Err() == nil {
			t.reconcileStatuses(ctx, client)
		}
		// If the parent context is done, exit
		if ctx.Err() != nil {
			return
		}
		// Reset backoff on successful read (timeout is expected, not an error)
		if err == nil || isTimeoutError(err) {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff*2 > maxBackoff {
			backoff = maxBackoff
		} else {
			backoff = backoff * 2
		}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// reconcileStatuses queries opencode's /v1/statusz for authoritative
// session statuses after an SSE reconnect (#753 F1). If the SSE stream
// dropped after a session transitioned to busy, the tracker retains
// stale "idle" — this method corrects it by reading ground truth.
func (t *sessionStatusTracker) reconcileStatuses(ctx context.Context, client *OpenCodeClient) {
	sessions, err := client.fetchSessionStatuses(ctx)
	if err != nil {
		log.Debug("reconcileStatuses: failed to fetch authoritative statuses", zap.Error(err))
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range sessions {
		if s.Status == "idle" {
			t.statuses[s.ID] = "idle"
		} else {
			t.statuses[s.ID] = "busy"
		}
	}
	log.Debug("reconcileStatuses: reconciled session statuses",
		zap.Int("count", len(sessions)))
}

// sseConnectionTimeout is the maximum lifetime of a single SSE connection.
// After this duration, the connection is closed and reconnected to prevent
// goroutine leaks from half-open sockets.
var sseConnectionTimeout = 5 * time.Minute

func (t *sessionStatusTracker) connectAndRead(ctx context.Context, client *OpenCodeClient) error {
	connCtx, cancel := context.WithTimeout(ctx, sseConnectionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(connCtx, "GET", getAgentAddr()+"/event", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(agentd.AuthUsername, client.password)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	httpClient := &http.Client{Timeout: 0} // no timeout for SSE
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE returned status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	var eventData strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			eventData.WriteString(strings.TrimPrefix(line, "data: "))
			eventData.WriteString("\n")
		} else if line == "" && eventData.Len() > 0 {
			t.processEvent(eventData.String())
			eventData.Reset()
		}
	}
	return scanner.Err()
}

func (t *sessionStatusTracker) processEvent(data string) {
	// Parse flat envelope first (cheap). Only try nested if flat fails.
	var evt struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if json.Unmarshal([]byte(data), &evt) != nil {
		return
	}
	switch evt.Type {
	case "session.status":
		t.handleSessionStatus(evt.Properties)
	case "session.next.step.ended":
		t.handleStepEnded(evt.Properties)
	default:
		// Try nested format for session.status (backward compat with global SSE endpoint).
		var nested struct {
			Payload struct {
				Type       string          `json:"type"`
				Properties json.RawMessage `json:"properties"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(data), &nested) != nil {
			return
		}
		switch nested.Payload.Type {
		case "session.status":
			t.handleSessionStatus(nested.Payload.Properties)
		case "session.next.step.ended":
			t.handleStepEnded(nested.Payload.Properties)
		}
	}
}

func (t *sessionStatusTracker) handleSessionStatus(props json.RawMessage) {
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
		t.set(p.SessionID, "idle")
	case "busy", "retry", "error", "compacting":
		t.set(p.SessionID, "busy")
	}
}

func (t *sessionStatusTracker) handleStepEnded(props json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionID"`
		Tokens    *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Cache     struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	}
	if json.Unmarshal(props, &p) != nil || p.SessionID == "" || p.Tokens == nil {
		return
	}
	promptTokens := p.Tokens.Input + p.Tokens.Cache.Read + p.Tokens.Cache.Write
	t.setPromptTokens(p.SessionID, promptTokens)
}

// fillGapsState prevents concurrent fillGaps iterations.
type fillGapsState struct {
	mu      sync.Mutex
	running bool
}

func runFill(ctx context.Context, client *OpenCodeClient, tracker *sessionStatusTracker, sessions func() []agentd.SessionInfo, state *fillGapsState) {
	state.mu.Lock()
	if state.running {
		state.mu.Unlock()
		return
	}
	state.running = true
	state.mu.Unlock()
	defer func() { state.running = false }()

	iterCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for _, s := range sessions() {
		if tracker.hasPromptTokens(s.ID) {
			continue
		}
		select {
		case <-iterCtx.Done():
			return
		default:
		}
		if tokens := client.fetchSessionPromptTokens(iterCtx, s.ID); tokens > 0 {
			tracker.setPromptTokens(s.ID, tokens)
		}
	}
}

func fillGaps(ctx context.Context, client *OpenCodeClient, tracker *sessionStatusTracker, sessions func() []agentd.SessionInfo, state *fillGapsState) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runFill(ctx, client, tracker, sessions, state)
		}
	}
}

const connectedCacheTTL = 15 * time.Second

func cachedState(ctx context.Context, client *OpenCodeClient, cache *providerCache, tracker *sessionStatusTracker) ([]string, int, []agentd.SessionInfo) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if time.Since(cache.lastFetchedAt) < connectedCacheTTL && cache.connected != nil {
		// Even on cache hit, refresh session statuses from SSE tracker
		for i := range cache.sessions {
			cache.sessions[i].Status = tracker.get(cache.sessions[i].ID)
		}
		return cache.connected, cache.configured, cache.sessions
	}
	connected, connErr := client.ConnectedProviders(ctx)
	configured, cfgErr := client.ConfiguredProviderCount(ctx)
	sessions, sessErr := client.ListSessions(ctx)
	if connErr != nil {
		log.Warn("failed to fetch connected providers", zap.Error(connErr))
	}
	if cfgErr != nil {
		log.Warn("failed to fetch configured provider count", zap.Error(cfgErr))
	}
	if sessErr != nil {
		log.Debug("failed to fetch sessions", zap.Error(sessErr))
	}
	// Merge SSE-tracked statuses into session list
	for i := range sessions {
		sessions[i].Status = tracker.get(sessions[i].ID)
	}
	// Prune tracker entries for sessions that no longer exist
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	tracker.prune(ids)
	cache.connected = connected
	cache.configured = configured
	cache.sessions = sessions
	cache.lastFetchedAt = time.Now()
	return connected, configured, sessions
}

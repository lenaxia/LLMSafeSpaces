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
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

type providerCache struct {
	mu            sync.Mutex
	connected     []string
	configured    int
	sessions      []agentd.SessionInfo
	lastFetchedAt time.Time
	// readySnapshot mirrors connected/configured behind an atomic
	// pointer so /v1/readyz never touches mu (design 0050 D4, review
	// round 4 on #895): cachedState holds mu across three synchronous
	// opencode HTTP calls on TTL expiry — up to ~15s under the exact
	// starvation this PR targets — and readyz blocking on that mutex
	// would defeat "probes detect death, never slowness". Written under
	// mu on every cache update; read atomically by lastKnown.
	readySnapshot atomic.Pointer[providerReadySnapshot]
}

// providerReadySnapshot is the lock-free readyz view of the cache.
type providerReadySnapshot struct {
	connected  []string
	configured int
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

// resetBusyFlags transitions every "busy" entry to "idle" and returns
// the cleared session IDs. Entries and promptTokens are preserved —
// prune() owns deletion and context meters are display state, not
// liveness state.
func (t *sessionStatusTracker) resetBusyFlags() []string {
	t.mu.Lock()
	var cleared []string
	for id, s := range t.statuses {
		if s == "busy" {
			t.statuses[id] = "idle"
			cleared = append(cleared, id)
		}
	}
	t.mu.Unlock()
	return cleared
}

// onOpencodeGenerationStart is the supervisor's generation-change hook
// (design 0050 D2). Busy state produced by a dead opencode generation is
// orphaned by definition — no idle event will ever arrive for it — so
// every new generation starts from honest idle. Validated by the
// 2026-08-15/16 incident: 8 tools stuck status:"running" across two
// workspaces, each starting seconds before a generation change, keeping
// sessions phantom-busy for 20-30+ minutes because /session returns DB
// records (which survive death), not busyness.
//
// Theoretical micro-race, accepted: an SSE event already buffered from
// the dying generation could be processed after this reset and re-mark a
// session busy. It is mutex-protected (no corruption), strictly no worse
// than pre-fix behavior, and self-heals on the next generation change.
func (t *sessionStatusTracker) onOpencodeGenerationStart() {
	cleared := t.resetBusyFlags()
	if len(cleared) == 0 {
		return
	}
	pkgOpsMetrics.RecordTrackerBusyReset(workspaceIDFromEnv(), len(cleared))
	log.Info("cleared orphaned busy flags for new opencode generation",
		zap.Strings("session_ids", cleared),
		zap.Int("count", len(cleared)))
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
	// 16 MB buffer per line (was 64 KB). SSE events like
	// message.part.updated carry full message metadata and can be very
	// large:
	//   - patch parts list every changed file path (observed: 2717 files
	//     = 370KB in a single event; a large monorepo could be 10MB+)
	//   - tool output is truncated to 50KB by opencode but the full
	//     Part envelope adds metadata
	//   - reasoning/text parts carry the model's full output
	// The old 64 KB cap caused the scanner to fail silently on large
	// events, dropping the SSE connection. The agentd tracker then
	// missed the session.status:idle transition and the session stayed
	// "busy" forever — the stuck-busy bug.
	// 16 MB is generous: it handles a 100K-file patch event (~10MB)
	// while still bounding a malicious or runaway upstream. The Go
	// scanner allocates lazily so the 16MB is a max, not a baseline.
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)

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
	case "":
		// Nested format (legacy global SSE endpoint only): its events
		// have no top-level type. Gated on evt.Type == "" so flat
		// non-usage events (deltas — the hottest, largest class) never
		// pay the extra whole-payload re-parse.
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
		default:
			if u, ok, err := wire.ParseStepUsageProps(nested.Payload.Type, nested.Payload.Properties); err != nil {
				log.Warn("usage event claims tokens but fails to decode — wire drift?",
					zap.Error(err), zap.String("eventType", nested.Payload.Type))
			} else if ok {
				t.setPromptTokens(u.SessionID, u.Tokens.PromptTokens())
			}
		}
	default:
		// Per-step usage (statusz ContextUsed until the usage-authority
		// cutover): every usage-bearing shape is decoded by the wire
		// seam — the legacy standalone step-ended event (mixed fleet)
		// and 1.18.x step-finish parts, suffixed or not. The Props
		// variant reuses the envelope already parsed above — part
		// updates are the dominant event type on an active stream.
		if u, ok, err := wire.ParseStepUsageProps(evt.Type, evt.Properties); err != nil {
			log.Warn("usage event claims tokens but fails to decode — wire drift?",
				zap.Error(err), zap.String("eventType", evt.Type))
		} else if ok {
			t.setPromptTokens(u.SessionID, u.Tokens.PromptTokens())
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

	activeSessions := sessions()

	// Pattern 2 Fix S8: prune stale session entries every fill cycle.
	// Without this, sessions that were deleted from opencode (but not
	// cleared from the tracker) stay in statuses/promptTokens forever,
	// causing phantom busy counts and incorrect restart gating.
	activeIDs := make([]string, 0, len(activeSessions))
	for _, s := range activeSessions {
		activeIDs = append(activeIDs, s.ID)
	}
	tracker.prune(activeIDs)

	for _, s := range activeSessions {
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

// lastKnown returns the provider cache's most recent connected/configured
// values without ever fetching and without taking providerCache.mu —
// it reads an atomic snapshot, so a concurrent cachedState fetch (which
// holds mu across synchronous opencode HTTP for up to its TTL-refresh
// window under starvation) cannot block it. Used by /v1/readyz (design
// 0050 D4): readiness must answer in microseconds under any load.
func (c *providerCache) lastKnown() (connected []string, configured int) {
	if snap := c.readySnapshot.Load(); snap != nil {
		return snap.connected, snap.configured
	}
	return nil, 0
}

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
	cache.readySnapshot.Store(&providerReadySnapshot{connected: connected, configured: configured})
	return connected, configured, sessions
}

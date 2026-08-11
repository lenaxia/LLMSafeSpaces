// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// V2SessionClient is re-exported from pkg/agent (the canonical location).
// Defined as an interface so tests inject a client targeting a dynamic-port
// httptest.Server instead of the hardcoded port 4096.
type V2SessionClient = agent.V2SessionClient

// V2ClientFactory builds a V2SessionClient for the given workspace.
type V2ClientFactory = agent.V2ClientFactory

// SetV2ClientFactory overrides V2 client construction. Used by tests to
// inject a client targeting a dynamic-port httptest.Server.
func (h *ProxyHandler) SetV2ClientFactory(f V2ClientFactory) {
	h.v2ClientFactory = f
}

// SetV2ClientConcreteFactory sets the factory that builds a V2SessionClient
// from a baseURL + password. app.go wires this with opencode.NewClient so
// this file does not import the opencode package.
func (h *ProxyHandler) SetV2ClientConcreteFactory(f func(baseURL, password string) (agent.V2SessionClient, error)) {
	h.v2ClientConcreteFactory = f
}

func (h *ProxyHandler) v2Client(ctx context.Context, workspaceID string) (V2SessionClient, error) {
	if h.v2ClientFactory != nil {
		return h.v2ClientFactory(ctx, workspaceID)
	}
	podIP, password, err := h.getPodIPAndPassword(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	baseURL := fmt.Sprintf("http://%s:%d", podIP, agentd.AgentPort)
	// Use a factory set during wiring (app.go) — this file must not
	// import pkg/agent/opencode.
	if h.v2ClientConcreteFactory != nil {
		return h.v2ClientConcreteFactory(baseURL, password)
	}
	return nil, fmt.Errorf("V2 client factory not configured")
}

// ---------------------------------------------------------------------------
// US-63.5: SSE Event Bridge — V2 events → queue.update
// ---------------------------------------------------------------------------

// Spike-verified V2 event wire types (worklog NNNN_us-63.1-v2-spike, F14):
//
//	session.next.prompt.admitted → queue.update/enqueued
//	session.next.prompted        → queue.update/sent
//
// Both carry properties.{messageID, sessionID, delivery}. Only
// delivery:"queue" inputs are bridged — delivery:"steer" inputs are
// mid-turn injections, not queue entries the frontend tracks as pills.
const (
	v2EventPromptAdmitted = "session.next.prompt.admitted"
	v2EventPrompted       = "session.next.prompted"
)

// v2PendingSessions tracks sessions that received a delivery:"queue" prompt
// whose input has NOT yet been promoted (drained) by opencode. Used by
// US-63.9 (stranded-input recovery) to identify sessions needing a wake
// after pod restart. Entries are reference-counted: enqueueV2 and
// bridgeV2Admitted increment; bridgeV2Prompted decrements. A session is
// removed from tracking only when its count reaches zero (all pending
// inputs drained). Per-replica; sufficient for the SSE-reconnect case.
// v2PendingTracker tracks sessions with undrained V2 queue-delivered input.
// Used by US-63.9 (stranded-input recovery) to identify sessions needing a
// wake after pod restart. Redis-backed in production (cross-replica shared);
// in-memory in tests without Redis.
type v2PendingTracker interface {
	add(workspaceID, sessionID string)
	remove(workspaceID, sessionID string)
	has(workspaceID, sessionID string) bool
	sessionsForWorkspace(workspaceID string) []string
}

// --- in-memory implementation (tests, single-replica fallback) ---

type v2PendingSessions struct {
	mu   sync.Mutex
	data map[string]map[string]v2PendingEntry // workspaceID → sessionID → entry
}

type v2PendingEntry struct {
	count     int
	lastAdded time.Time
}

func newV2PendingSessions() *v2PendingSessions {
	return &v2PendingSessions{data: make(map[string]map[string]v2PendingEntry)}
}

func (v *v2PendingSessions) add(workspaceID, sessionID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data[workspaceID] == nil {
		v.data[workspaceID] = make(map[string]v2PendingEntry)
	}
	e := v.data[workspaceID][sessionID]
	e.count++
	e.lastAdded = time.Now()
	v.data[workspaceID][sessionID] = e
}

func (v *v2PendingSessions) remove(workspaceID, sessionID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked()
	if ws, ok := v.data[workspaceID]; ok {
		e := ws[sessionID]
		e.count--
		if e.count <= 0 {
			delete(ws, sessionID)
		} else {
			ws[sessionID] = e
		}
		if len(ws) == 0 {
			delete(v.data, workspaceID)
		}
	}
}

func (v *v2PendingSessions) has(workspaceID, sessionID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked()
	ws, ok := v.data[workspaceID]
	return ok && ws[sessionID].count > 0
}

func (v *v2PendingSessions) sessionsForWorkspace(workspaceID string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked()
	ws, ok := v.data[workspaceID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(ws))
	for sid, e := range ws {
		if e.count > 0 {
			out = append(out, sid)
		}
	}
	return out
}

// pruneLocked removes entries older than v2PendingTTL. Must be called
// with mu held. Matches the Redis implementation's TTL behavior (#744 F2).
func (v *v2PendingSessions) pruneLocked() {
	cutoff := time.Now().Add(-v2PendingTTL)
	for wsID, ws := range v.data {
		for sid, e := range ws {
			if e.lastAdded.Before(cutoff) {
				delete(ws, sid)
			}
		}
		if len(ws) == 0 {
			delete(v.data, wsID)
		}
	}
}

// --- Redis-backed implementation (production, multi-replica) ---

// v2PendingRedis tracks pending V2 sessions in Redis so all replicas share
// the same view. A Redis hash per workspace stores sessionID → pending count.
// HINCRBY increments/decrements atomically; sessions reaching zero are
// removed. A TTL on the hash key prevents unbounded growth from lost events.
type v2PendingRedis struct {
	client *redis.Client
}

const v2PendingTTL = 10 * time.Minute

func newV2PendingRedis(client *redis.Client) *v2PendingRedis {
	if client == nil {
		return nil
	}
	return &v2PendingRedis{client: client}
}

// NewV2PendingTracker creates a Redis-backed pending-session tracker for
// multi-replica V2 stranded-input recovery. Returns nil if client is nil
// (caller keeps the in-memory default).
func NewV2PendingTracker(client *redis.Client) v2PendingTracker {
	t := newV2PendingRedis(client)
	if t == nil {
		return nil
	}
	return t
}

func v2PendingKey(workspaceID string) string {
	return fmt.Sprintf("v2pending:%s", workspaceID)
}

func (v *v2PendingRedis) add(workspaceID, sessionID string) {
	if v == nil {
		return
	}
	ctx := context.Background()
	key := v2PendingKey(workspaceID)
	pipe := v.client.TxPipeline()
	pipe.HIncrBy(ctx, key, sessionID, 1)
	pipe.Expire(ctx, key, v2PendingTTL)
	_, _ = pipe.Exec(ctx)
}

func (v *v2PendingRedis) remove(workspaceID, sessionID string) {
	if v == nil {
		return
	}
	ctx := context.Background()
	// Decrement the reference count. Do NOT HDel even when count reaches
	// zero — a concurrent add between HINCRBY -1 and HDel would be
	// clobbered (TOCTOU race), reproducing the stranded-input bug.
	// Readers (has, sessionsForWorkspace) filter count > 0; negative counts
	// are invisible. The TTL sweeps the hash key eventually.
	_, err := v.client.HIncrBy(ctx, v2PendingKey(workspaceID), sessionID, -1).Result()
	if err != nil {
		// Best-effort: Redis errors don't block the request path. The
		// TTL sweeps stale entries; the next add re-increments.
		return
	}
}

func (v *v2PendingRedis) has(workspaceID, sessionID string) bool {
	if v == nil {
		return false
	}
	ctx := context.Background()
	val, err := v.client.HGet(ctx, v2PendingKey(workspaceID), sessionID).Int()
	if err != nil {
		return false
	}
	return val > 0
}

func (v *v2PendingRedis) sessionsForWorkspace(workspaceID string) []string {
	if v == nil {
		return nil
	}
	ctx := context.Background()
	raw, err := v.client.HGetAll(ctx, v2PendingKey(workspaceID)).Result()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for sid, val := range raw {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			out = append(out, sid)
		}
	}
	return out
}

// onV2RawEvent is called from onRawEvent to detect V2 PromptAdmitted/Prompted
// events and synthesize the queue.update SSE events the frontend expects, and
// manage the pending-sessions tracking for US-63.9.
//
// rawData is the raw JSON envelope: {"id":"...","type":"...","properties":{...}}.
func (h *ProxyHandler) onV2RawEvent(workspaceID, eventType, rawData string) {
	switch eventType {
	case v2EventPromptAdmitted:
		h.bridgeV2Admitted(workspaceID, rawData)
	case v2EventPrompted:
		h.bridgeV2Prompted(workspaceID, rawData)
	}
}

func (h *ProxyHandler) bridgeV2Admitted(workspaceID, rawData string) {
	var props struct {
		Properties struct {
			MessageID string `json:"messageID"`
			SessionID string `json:"sessionID"`
			Delivery  string `json:"delivery"`
			Prompt    struct {
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rawData), &props); err != nil {
		h.logger.Debug("V2 SSE bridge: failed to parse admitted event", "error", err, "workspaceID", workspaceID)
		return
	}
	if props.Properties.Delivery != "queue" {
		return
	}
	// US-63.5: synthesize queue.update/enqueued from the V2 admission event.
	h.publishQueueEvent(workspaceID, props.Properties.SessionID, "enqueued", props.Properties.MessageID, "")
	// US-63.10: write to Redis shadow for fresh-load pill visibility.
	h.v2Shadow.Add(context.Background(), workspaceID, props.Properties.SessionID,
		props.Properties.MessageID, props.Properties.Prompt.Text)
}

func (h *ProxyHandler) bridgeV2Prompted(workspaceID, rawData string) {
	var props struct {
		Properties struct {
			MessageID string `json:"messageID"`
			SessionID string `json:"sessionID"`
			Delivery  string `json:"delivery"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rawData), &props); err != nil {
		h.logger.Debug("V2 SSE bridge: failed to parse prompted event", "error", err, "workspaceID", workspaceID)
		return
	}
	if props.Properties.Delivery != "queue" {
		return
	}
	h.publishQueueEvent(workspaceID, props.Properties.SessionID, "sent", props.Properties.MessageID, "")
	h.v2Pending.remove(workspaceID, props.Properties.SessionID)
	// US-63.10: clear from Redis shadow (message was promoted/drained).
	h.v2Shadow.Remove(context.Background(), workspaceID, props.Properties.SessionID, props.Properties.MessageID)
}

// ---------------------------------------------------------------------------
// US-63.9: Stranded-Input Recovery — wake on reconnect
// ---------------------------------------------------------------------------

// wakeStrandedV2Sessions sends a minimal delivery:"queue" prompt to each
// idle session that has pending V2 input. This triggers opencode's
// execution.wake → runner.run, which drains the durable SessionInput rows
// that survived the restart in SQLite.
//
// The wake prompt creates one extra turn per session. This is the accepted
// trade-off of Option B (proxy-side wake): it pollutes the session with one
// no-op turn, but unblocks all stranded queued input. The clean solution
// (Option A: upstream resume endpoint) would not create a turn; it is a
// follow-up if/when opencode exposes POST /api/session/:sid/resume.
//
// Called from reconcileSessionState after the idle-session sweep.
func (h *ProxyHandler) wakeStrandedV2Sessions(ctx context.Context, workspaceID string, busySessions map[string]bool) {
	sessions := h.v2Pending.sessionsForWorkspace(workspaceID)
	for _, sid := range sessions {
		if busySessions[sid] {
			h.logger.Debug("V2 stranded-input recovery: skipping busy session",
				"workspaceID", workspaceID, "sessionID", sid)
			continue
		}
		h.logger.Info("V2 stranded-input recovery: waking idle session with pending queue",
			"workspaceID", workspaceID, "sessionID", sid)
		client, err := h.v2Client(ctx, workspaceID)
		if err != nil {
			h.logger.Warn("V2 stranded-input recovery: failed to construct client",
				"error", err, "workspaceID", workspaceID, "sessionID", sid)
			continue
		}
		// A single newline triggers execution.wake → runner.run → drains
		// ALL pending rows. Non-empty (passes F18 validation). Minimal
		// history pollution — one turn the LLM processes as a blank.
		if _, err := client.PromptV2(ctx, sid, "\n", agent.V2DeliveryQueue); err != nil {
			h.logger.Warn("V2 stranded-input recovery: wake prompt failed",
				"error", err, "workspaceID", workspaceID, "sessionID", sid)
			continue
		}
	}
}

// ---------------------------------------------------------------------------
// US-63.3: Enqueue path (delivery:queue)
// ---------------------------------------------------------------------------

// enqueueV2 sends a prompt to opencode's V2 session API with
// delivery:"queue".
//
// Under US-63.5: the queue.update/enqueued SSE event is NO LONGER emitted
// here — it is derived from the V2 PromptAdmitted event in onV2RawEvent.
// This eliminates the race where enqueued fires before opencode has
// actually admitted the input. The response still returns the messageID
// synchronously for callers that need it.
func (h *ProxyHandler) enqueueV2(c *gin.Context, wid, sid, text string) {
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 enqueue: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return
	}
	resp, err := client.PromptV2(c.Request.Context(), sid, text, agent.V2DeliveryQueue)
	if err != nil {
		if agent.IsSessionNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}
		h.logger.Error("V2 enqueue: PromptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue message"})
		return
	}
	// US-63.5: do NOT emit enqueued here — onV2RawEvent derives it from the
	// V2 PromptAdmitted event. Track for US-63.9 stranded-input recovery.
	if h.v2Pending != nil {
		h.v2Pending.add(wid, sid)
	}
	c.JSON(http.StatusAccepted, gin.H{"messageID": resp.ID})
}

// ---------------------------------------------------------------------------
// US-63.4: Abort path (non-destructive interrupt)
// ---------------------------------------------------------------------------

// abortV2 sends a non-destructive interrupt to opencode's V2 session API.
// The queued messages survive and drain on the next execution.wake (F8).
func (h *ProxyHandler) abortV2(c *gin.Context, wid, sid string) {
	client, err := h.v2Client(c.Request.Context(), wid)
	if err != nil {
		h.logger.Error("V2 abort: failed to construct client", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reach workspace"})
		return
	}
	if err := client.InterruptV2(c.Request.Context(), sid); err != nil {
		h.logger.Error("V2 abort: InterruptV2 failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abort session"})
		return
	}
	c.Status(http.StatusNoContent)
}

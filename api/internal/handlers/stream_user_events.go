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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

const (
	heartbeatInterval   = 25 * time.Second
	writeDeadlineWindow = 30 * time.Second
	snapshotTimeout     = 5 * time.Second

	labelUserID = "user-id"

	sseConnRateLimit  = 10
	sseConnRateWindow = time.Minute
)

type sseConnAttempt struct {
	count   int
	resetAt time.Time
}

var (
	sseConnMu     sync.Mutex
	sseConnCounts = make(map[string]*sseConnAttempt)
)

// StreamUserEvents is the user-scoped SSE endpoint (GET /api/v1/events).
// It delivers workspace.phase events for ALL of the user's workspaces.
func (h *ProxyHandler) StreamUserEvents(c *gin.Context) {
	if !sseConnAllowed(c.ClientIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many SSE connection attempts"})
		return
	}

	userID, _ := c.Get("userID")
	uid, ok := userID.(string)
	if !ok || uid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if h.userBroker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "event broker not initialized"})
		return
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	s, err := h.userBroker.SubscribeUser(uid)
	if err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many connections"})
		return
	}
	defer h.userBroker.UnsubscribeUser(uid, s)

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	streamCtx, streamCancel := context.WithCancel(c.Request.Context())
	defer streamCancel()

	// Acquire ResponseController for write deadlines
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))

	// Replay phase: handler goroutine writes directly (live loop not started yet — F1)
	lastEventIDStr := c.GetHeader("Last-Event-ID")
	if lastEventIDStr != "" {
		lastID, parseErr := strconv.ParseUint(lastEventIDStr, 10, 64)
		if parseErr == nil && lastID > 0 {
			entries, gapDetected := h.userBroker.Replay(uid, lastID)
			if gapDetected {
				if writeErr := writeSSEResync(c.Writer, flusher, rc); writeErr != nil {
					streamCancel()
					return
				}
			}
			for _, entry := range entries {
				if writeErr := writeSSEEvent(c.Writer, flusher, rc, entry.ID, entry.Event); writeErr != nil {
					streamCancel()
					return
				}
			}
		}
	}

	// Start snapshot goroutine (concurrent with live loop; sends into s.Ch)
	go h.snapshotUserWorkspaces(streamCtx, s, uid)

	// Start heartbeat goroutine (sends _heartbeat sentinels into s.Ch)
	go heartbeatLoop(streamCtx, s)

	// Live loop — sole writer to c.Writer from this point (F1)
	for {
		select {
		case <-streamCtx.Done():
			return
		case evt, open := <-s.Ch:
			if !open {
				return // defensive: ch is never closed (markClosed+context cancellation used instead)
			}
			if evt.Type == eventbroker.HeartbeatSentinelType {
				if _, writeErr := fmt.Fprint(c.Writer, ":\n\n"); writeErr != nil {
					streamCancel()
					return
				}
				flusher.Flush()
				_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
				continue
			}
			if evt.EventID == 0 {
				// Snapshot event — no id: line
				data, marshalErr := json.Marshal(evt)
				if marshalErr != nil {
					h.logger.Warn("SSE snapshot event marshal failed, dropping",
						"error", marshalErr,
						"userID", uid,
						"eventType", evt.Type,
					)
					continue
				}
				if _, writeErr := fmt.Fprintf(c.Writer, "data: %s\n\n", data); writeErr != nil {
					streamCancel()
					return
				}
			} else {
				// Live event with id: line
				data, marshalErr := json.Marshal(evt)
				if marshalErr != nil {
					h.logger.Warn("SSE live event marshal failed, dropping",
						"error", marshalErr,
						"userID", uid,
						"eventType", evt.Type,
					)
					continue
				}
				if _, writeErr := fmt.Fprintf(c.Writer, "id: %d\ndata: %s\n\n", evt.EventID, data); writeErr != nil {
					streamCancel()
					return
				}
			}
		}
		flusher.Flush()
		_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
	}
}

func sseConnAllowed(ip string) bool {
	sseConnMu.Lock()
	defer sseConnMu.Unlock()

	now := time.Now()
	// G42: opportunistic pruning of stale entries. Without this, the
	// sseConnCounts map grows unbounded over the process lifetime —
	// every distinct client IP that ever attempts a connection leaves
	// a permanent entry, even after its resetAt expires. The prune is
	// O(N) where N is the number of distinct IPs seen in the last
	// sseConnRateWindow. N is bounded in practice by the per-IP rate
	// limit (sseConnRateLimit per sseConnRateWindow) — long-lived
	// deployments with rotating clients (NAT pools, mobile networks)
	// are the realistic worst case at ~thousands of entries, not
	// millions. Sweeping on every call avoids a separate goroutine
	// and the lifecycle complexity it would add.
	for k, v := range sseConnCounts {
		if now.After(v.resetAt) {
			delete(sseConnCounts, k)
		}
	}

	entry, ok := sseConnCounts[ip]
	if !ok || now.After(entry.resetAt) {
		sseConnCounts[ip] = &sseConnAttempt{count: 1, resetAt: now.Add(sseConnRateWindow)}
		return true
	}
	entry.count++
	return entry.count <= sseConnRateLimit
}

// snapshotUserWorkspaces lists the user's workspaces and emits their current phases
// into s.Ch. On k8s list failure, emits resync. Runs concurrently with live loop.
func (h *ProxyHandler) snapshotUserWorkspaces(ctx context.Context, s *eventbroker.Subscriber, userID string) {
	if h.k8sClient == nil {
		return // no k8s client — skip snapshot (tests or degraded mode)
	}

	// Use a timeout-limited goroutine for the k8s list (G5)
	type listResult struct {
		items []string
		err   error
	}
	resultCh := make(chan listResult, 1)
	go func() {
		v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
		if v1Err != nil {
			resultCh <- listResult{err: v1Err}
			return
		}
		list, err := v1Client.Workspaces(h.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelUserID + "=" + userID,
		})
		if err != nil {
			resultCh <- listResult{err: err}
			return
		}
		ids := make([]string, len(list.Items))
		for i := range list.Items {
			ids[i] = list.Items[i].Name
		}
		resultCh <- listResult{items: ids}
	}()

	// Wait with timeout
	timer := time.NewTimer(snapshotTimeout)
	defer timer.Stop()

	var wsIDs []string
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		s.Send(apitypes.WorkspaceSSEEvent{Type: "resync"})
		return
	case res := <-resultCh:
		if res.err != nil {
			s.Send(apitypes.WorkspaceSSEEvent{Type: "resync"})
			return
		}
		wsIDs = res.items
	}

	// Get all known phases in one RLock (G8)
	var phases map[string]string
	if h.watcher != nil {
		phases = h.watcher.GetAllKnownPhases()
	}

	for _, wsID := range wsIDs {
		phase := ""
		if phases != nil {
			phase = phases[wsID]
		}
		if phase == "" {
			continue // F4: skip workspaces with empty phase
		}
		s.Send(apitypes.WorkspaceSSEEvent{
			Type:        "workspace.phase",
			WorkspaceID: wsID,
			Phase:       phase,
			EventID:     0, // snapshot events have no id: line
		})
	}

	// US-55.3: Pending-input anti-entropy. Re-emit each Active workspace's
	// authoritative pending set from the pod so a reconnecting client rebuilds
	// pendingActions correctly (mirrors busy's seedBusy). Bounded by the ≤10
	// active-workspace scale constraint; each fetch is timeout-guarded.
	if h.dialect != nil {
		for _, wsID := range wsIDs {
			phase := ""
			if phases != nil {
				phase = phases[wsID]
			}
			if phase != string(phaseActive) {
				continue
			}
			go func(id string) {
				defer func() { _ = recover() }() // never let a fetch panic the snapshot
				h.emitPendingInputRequests(ctx, id)
			}(wsID)
		}
	}
}

// heartbeatLoop sends heartbeat sentinels into s.Ch every heartbeatInterval.
// Exits when ctx is canceled.
func heartbeatLoop(ctx context.Context, s *eventbroker.Subscriber) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Send(apitypes.WorkspaceSSEEvent{Type: eventbroker.HeartbeatSentinelType})
		}
	}
}

// writeSSEEvent writes a single SSE event with id: line to the writer.
func writeSSEEvent(w gin.ResponseWriter, flusher http.Flusher, rc *http.ResponseController, id uint64, evt apitypes.WorkspaceSSEEvent) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if _, writeErr := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, data); writeErr != nil {
		return writeErr
	}
	flusher.Flush()
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
	return nil
}

// writeSSEResync writes a resync event (no id: line).
func writeSSEResync(w gin.ResponseWriter, flusher http.Flusher, rc *http.ResponseController) error {
	resyncEvt := apitypes.WorkspaceSSEEvent{Type: "resync"}
	data, err := json.Marshal(resyncEvt)
	if err != nil {
		return err
	}
	if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", data); writeErr != nil {
		return writeErr
	}
	flusher.Flush()
	_ = rc.SetWriteDeadline(time.Now().Add(writeDeadlineWindow))
	return nil
}

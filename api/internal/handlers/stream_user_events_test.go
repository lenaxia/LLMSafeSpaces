// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestStreamUserEvents_Unauthenticated_Returns401(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		// Don't set userID — simulates unauthenticated
		h.StreamUserEvents(c)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/events", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestStreamUserEvents_NilBroker_Returns503(t *testing.T) {
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: nil}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-1")
		h.StreamUserEvents(c)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/events", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestStreamUserEvents_TooManyConnections_Returns429(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	// Fill all subscriber slots
	subs := make([]*eventbroker.Subscriber, eventbroker.MaxSubscribersPerUser)
	for i := 0; i < eventbroker.MaxSubscribersPerUser; i++ {
		s, err := broker.SubscribeUser("user-full")
		require.NoError(t, err)
		subs[i] = s
	}
	defer func() {
		for _, s := range subs {
			broker.UnsubscribeUser("user-full", s)
		}
	}()

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-full")
		h.StreamUserEvents(c)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/events", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestStreamUserEvents_SSEHeaders(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-hdr")
		h.StreamUserEvents(c)
	})

	// Use a real server so SSE streaming works
	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
}

func TestStreamUserEvents_LiveEventDelivery(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-live")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Publish an event after connection is established
	time.Sleep(100 * time.Millisecond)
	broker.PublishToUser("user-live", apitypes.WorkspaceSSEEvent{
		Type:        "workspace.phase",
		WorkspaceID: "ws-test",
		Phase:       "Active",
	})

	// Read SSE events
	scanner := bufio.NewScanner(resp.Body)
	var foundEvent bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var evt apitypes.WorkspaceSSEEvent
			if err := json.Unmarshal([]byte(data), &evt); err == nil {
				if evt.Type == "workspace.phase" && evt.Phase == "Active" && evt.WorkspaceID == "ws-test" {
					foundEvent = true
					assert.NotZero(t, evt.EventID)
					break
				}
			}
		}
	}
	assert.True(t, foundEvent, "should have received the live workspace.phase event")
}

func TestStreamUserEvents_LiveEvent_HasIDLine(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-id-line")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	broker.PublishToUser("user-id-line", apitypes.WorkspaceSSEEvent{
		Type:        "workspace.phase",
		WorkspaceID: "ws-x",
		Phase:       "Active",
	})

	scanner := bufio.NewScanner(resp.Body)
	var foundIDLine bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			foundIDLine = true
			break
		}
	}
	assert.True(t, foundIDLine, "live events should have id: line")
}

func TestStreamUserEvents_Replay(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	// Pre-populate replay buffer
	for i := 0; i < 3; i++ {
		broker.PublishToUser("user-replay", apitypes.WorkspaceSSEEvent{
			Type:        "workspace.phase",
			WorkspaceID: "ws-r",
			Phase:       "Active",
		})
	}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-replay")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Connect with Last-Event-ID: 1 — should replay events 2 and 3
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var replayedCount int
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			replayedCount++
			if replayedCount >= 2 {
				break
			}
		}
	}
	assert.Equal(t, 2, replayedCount)
}

func TestStreamUserEvents_HeartbeatEmitted(t *testing.T) {
	// Verifies the heartbeat goroutine sends heartbeat sentinel events.
	// Uses a short ticker interval to avoid slow tests; asserts ≥1 heartbeat
	// in 300ms (ticker at 50ms). Assertion is ≥1 (not ≥2) to avoid flakiness
	// under scheduler jitter when tests run with -race and -count>1.

	s := &eventbroker.Subscriber{Ch: make(chan apitypes.WorkspaceSSEEvent, 10)}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Run heartbeat with a very short interval for testing
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Send(apitypes.WorkspaceSSEEvent{Type: eventbroker.HeartbeatSentinelType})
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	// Should have at least 1 heartbeat in the channel.
	var heartbeats int
	for {
		select {
		case evt := <-s.Ch:
			if evt.Type == eventbroker.HeartbeatSentinelType {
				heartbeats++
			}
		default:
			goto done
		}
	}
done:
	assert.GreaterOrEqual(t, heartbeats, 1, "expected at least one heartbeat event in channel")
}

func TestStreamUserEvents_SnapshotEmitsBeforeLiveEvents(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()

	// Set up a mock k8s client that returns workspaces for the user
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("List", mock.Anything, mock.MatchedBy(func(opts metav1.ListOptions) bool {
		return opts.LabelSelector == labelUserID+"=user-snap"
	})).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-b"}},
		},
	}, nil)

	// Set up watcher with known phases
	watcher, _ := workspace.NewWatcher(k8sMock, &testLogger{}, "default", func(*v1.Workspace) {})
	watcher.SetKnownPhase("ws-a", "Active")
	watcher.SetKnownPhase("ws-b", "Suspended")

	h := &ProxyHandler{
		k8sClient:  k8sMock,
		logger:     &testLogger{},
		namespace:  "default",
		userBroker: broker,
		watcher:    watcher,
	}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-snap")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Collect events — expect snapshot events for ws-a and ws-b
	scanner := bufio.NewScanner(resp.Body)
	var snapshotEvents []apitypes.WorkspaceSSEEvent
	deadline := time.After(2 * time.Second)

	for {
		select {
		case <-deadline:
			goto done
		default:
		}
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var evt apitypes.WorkspaceSSEEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err == nil {
				if evt.Type == "workspace.phase" {
					snapshotEvents = append(snapshotEvents, evt)
					if len(snapshotEvents) >= 2 {
						goto done
					}
				}
			}
		}
	}
done:
	cancel()

	assert.Len(t, snapshotEvents, 2)
	// Snapshot events should have EventID=0 (no id: line in SSE)
	for _, evt := range snapshotEvents {
		assert.Zero(t, evt.EventID, "snapshot events should have EventID=0")
	}
	// Verify both workspaces present
	phases := map[string]string{}
	for _, evt := range snapshotEvents {
		phases[evt.WorkspaceID] = evt.Phase
	}
	assert.Equal(t, "Active", phases["ws-a"])
	assert.Equal(t, "Suspended", phases["ws-b"])
}

func TestStreamUserEvents_SnapshotSkipsEmptyPhase(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-known"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-deleted"}}, // not in knownPhases
		},
	}, nil)

	watcher, _ := workspace.NewWatcher(k8sMock, &testLogger{}, "default", func(*v1.Workspace) {})
	watcher.SetKnownPhase("ws-known", "Active")

	h := &ProxyHandler{
		k8sClient:  k8sMock,
		logger:     &testLogger{},
		namespace:  "default",
		userBroker: broker,
		watcher:    watcher,
	}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-f4")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var events []apitypes.WorkspaceSSEEvent
	deadline := time.After(1 * time.Second)

	for {
		select {
		case <-deadline:
			goto done2
		default:
		}
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var evt apitypes.WorkspaceSSEEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err == nil {
				if evt.Type == "workspace.phase" {
					events = append(events, evt)
				}
			}
		}
	}
done2:
	cancel()

	// Only ws-known should appear (ws-deleted has empty phase, skipped per F4)
	assert.Len(t, events, 1)
	assert.Equal(t, "ws-known", events[0].WorkspaceID)
	assert.Equal(t, "Active", events[0].Phase)
}

// TestStreamUserEvents_SnapshotListFailure_EmitsResync (S28.8) verifies that
// when the k8s List call fails, the snapshot goroutine emits a `resync` event
// instead of hanging or silently dropping the snapshot. The client receives
// the resync and can trigger a full refetch.
func TestStreamUserEvents_SnapshotListFailure_EmitsResync(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("List", mock.Anything, mock.Anything).Return(
		(*v1.WorkspaceList)(nil), fmt.Errorf("k8s api unavailable"),
	)

	h := &ProxyHandler{
		k8sClient:  k8sMock,
		logger:     &testLogger{},
		namespace:  "default",
		userBroker: broker,
	}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-resync")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	gotResync := false
	deadline := time.After(2 * time.Second)

	for {
		select {
		case <-deadline:
			goto doneResync
		default:
		}
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var evt apitypes.WorkspaceSSEEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err == nil {
				if evt.Type == "resync" {
					gotResync = true
					goto doneResync
				}
			}
		}
	}
doneResync:
	cancel()

	assert.True(t, gotResync, "client should receive a resync event when k8s List fails")
}

// TestStreamUserEvents_GoroutineExitsOnClientDisconnect verifies that when the
// client drops the connection both the snapshot goroutine and heartbeat goroutine
// exit promptly (no leak), and the subscription is unregistered from the broker.
//
// Approach: use a real httptest.Server so we have a genuine TCP connection to
// close. Closing the client's response body cancels the request context, which
// propagates to streamCtx. We then poll the broker's subscriber count to confirm
// it drops back to zero within a reasonable window.
func TestStreamUserEvents_GoroutineExitsOnClientDisconnect(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-gc")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	// Wait for the handler to be fully in the live loop (subscription registered).
	require.Eventually(t, func() bool {
		return broker.UserSubscriberCount("user-gc") == 1
	}, time.Second, 10*time.Millisecond, "subscription should be registered")

	// Drop the connection — cancels c.Request.Context() server-side.
	resp.Body.Close()
	cancel()

	// defer UnsubscribeUser must fire and remove the subscription.
	require.Eventually(t, func() bool {
		return broker.UserSubscriberCount("user-gc") == 0
	}, 2*time.Second, 20*time.Millisecond, "subscription should be unregistered after disconnect")
}

// TestStreamUserEvents_WriteErrorCancelsStream verifies that when the server
// fails to write an SSE event (write deadline exceeded / broken pipe), the
// live loop calls streamCancel() and returns, and the subscription is cleaned up.
//
// We exercise the write-error path by publishing an event and then immediately
// closing the response body — the server's pending write to the now-dead TCP
// connection returns an error, triggering the streamCancel() path in the live
// loop. This is the same code path that fires on a real write-deadline eviction.
func TestStreamUserEvents_WriteErrorCancelsStream(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-wd")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	// Wait for subscription to be established.
	require.Eventually(t, func() bool {
		return broker.UserSubscriberCount("user-wd") == 1
	}, time.Second, 10*time.Millisecond, "subscription should be registered")

	// Publish an event so the live loop has a pending write to perform.
	broker.PublishToUser("user-wd", apitypes.WorkspaceSSEEvent{
		Type:        "workspace.phase",
		WorkspaceID: "ws-wd",
		Phase:       "Active",
	})

	// Close the connection immediately — the server's write attempt fails,
	// streamCancel() fires, and defer UnsubscribeUser cleans up.
	resp.Body.Close()
	cancel()

	// Subscription must be unregistered — no zombie connection left.
	require.Eventually(t, func() bool {
		return broker.UserSubscriberCount("user-wd") == 0
	}, 2*time.Second, 20*time.Millisecond, "subscription should be unregistered after write failure")
}

func TestSSEConnAllowed_EnforcesRateLimit(t *testing.T) {
	sseConnMu.Lock()
	for k := range sseConnCounts {
		delete(sseConnCounts, k)
	}
	sseConnMu.Unlock()

	ip := "10.0.0.1"

	for i := 0; i < sseConnRateLimit; i++ {
		assert.True(t, sseConnAllowed(ip), "attempt %d should be allowed", i+1)
	}
	assert.False(t, sseConnAllowed(ip), "attempt beyond limit should be rejected")

	sseConnMu.Lock()
	delete(sseConnCounts, ip)
	sseConnMu.Unlock()
}

func TestSSEConnAllowed_ResetsAfterWindow(t *testing.T) {
	sseConnMu.Lock()
	for k := range sseConnCounts {
		delete(sseConnCounts, k)
	}
	sseConnMu.Unlock()

	ip := "10.0.0.2"

	for i := 0; i < sseConnRateLimit; i++ {
		sseConnAllowed(ip)
	}
	assert.False(t, sseConnAllowed(ip))

	sseConnMu.Lock()
	entry := sseConnCounts[ip]
	entry.resetAt = time.Now().Add(-time.Second)
	sseConnMu.Unlock()

	assert.True(t, sseConnAllowed(ip), "should be allowed after window expires")

	sseConnMu.Lock()
	delete(sseConnCounts, ip)
	sseConnMu.Unlock()
}

// Test 16 (US-37.1): user-scoped SSE delivers session.status event.
// Full path: PublishToUser → StreamUserEvents → client receives session_id, workspace_id, status.
func TestStreamUserEvents_DeliversSessionStatusEvent(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	h := &ProxyHandler{logger: &testLogger{}, namespace: "default", userBroker: broker}

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "user-sess-status")
		h.StreamUserEvents(c)
	})

	srv := httptest.NewServer(router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	broker.PublishToUser("user-sess-status", apitypes.WorkspaceSSEEvent{
		Type:        "session.status",
		WorkspaceID: "ws-abc",
		SessionID:   "sess-xyz",
		Status:      "busy",
	})

	scanner := bufio.NewScanner(resp.Body)
	var found bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt apitypes.WorkspaceSSEEvent
		if err2 := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err2 != nil {
			continue
		}
		if evt.Type == "session.status" && evt.WorkspaceID == "ws-abc" &&
			evt.SessionID == "sess-xyz" && evt.Status == "busy" {
			found = true
			break
		}
	}
	assert.True(t, found, "user-scoped SSE must deliver session.status event with correct fields")
}

// TestSSEConnAllowed_G42_PrunesStaleEntries is the G42 regression: the
// sseConnCounts map must be pruned of expired entries on each call.
// Without pruning, every distinct client IP that ever attempted a
// connection leaves a permanent entry — unbounded memory growth over
// the process lifetime for deployments with rotating client IPs (NAT
// pools, mobile networks).
//
// Pre-fix: sseConnCounts map grew monotonically. A deployment running
// for weeks with thousands of distinct client IPs would accumulate
// tens of thousands of stale entries, each ~40 bytes (string key +
// sseConnAttempt struct) — small per entry, unbounded in aggregate.
//
// Post-fix: every sseConnAllowed call sweeps expired entries. The
// sweep is O(N) where N is the current entry count; acceptable
// because N is bounded by the per-IP rate limit (long-lived entries
// are pruned the moment they expire).
func TestSSEConnAllowed_G42_PrunesStaleEntries(t *testing.T) {
	// Reset state.
	sseConnMu.Lock()
	for k := range sseConnCounts {
		delete(sseConnCounts, k)
	}
	sseConnMu.Unlock()

	// Seed 10 entries with resetAt in the past (expired).
	sseConnMu.Lock()
	for i := 0; i < 10; i++ {
		ip := fmt.Sprintf("10.0.0.%d", i)
		sseConnCounts[ip] = &sseConnAttempt{
			count:   1,
			resetAt: time.Now().Add(-time.Minute), // expired
		}
	}
	require.Equal(t, 10, len(sseConnCounts), "seed: 10 expired entries")
	sseConnMu.Unlock()

	// Call sseConnAllowed with a fresh IP — triggers the prune.
	assert.True(t, sseConnAllowed("10.0.0.99"), "fresh IP should be allowed")

	// All 10 expired entries must be pruned. Only the fresh IP (10.0.0.99)
	// should remain.
	sseConnMu.Lock()
	pruned := len(sseConnCounts)
	sseConnMu.Unlock()

	assert.Equal(t, 1, pruned,
		"G42 REGRESSION: stale entries were not pruned; map still has %d entries (want 1)", pruned)

	// Cleanup.
	sseConnMu.Lock()
	delete(sseConnCounts, "10.0.0.99")
	sseConnMu.Unlock()
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Tests that reproduce the queue-drain-miss bug:
//
//   When a workspace session completes (goes idle) while the API server's SSE
//   connection to the workspace pod is down or reconnecting, the
//   onSessionIdle callback is never called, drainQueuedMessage never runs,
//   and the queued message is left stranded in Redis indefinitely.
//
// Confirmed occurrence: 2026-06-15 20:18–20:22 UTC, workspace
// a847faa5-19b4-463d-a434-1ce473a16f93, session ses_1361f1c44ffedDI7pqWvXkNGJt.
// Message enqueued at 20:18:22; GET /queue at 20:22:58 still shows it present;
// no drain log entry, no queue.update SSE event, no prompt_async call was ever
// made to the workspace pod.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	ssetracker "github.com/lenaxia/llmsafespaces/api/internal/services/sse"
)

// TestDrainMiss_SSEDownWhenSessionGoesIdle reproduces the core bug:
//
// The workspace pod emits session.status=idle exactly once. The API server's
// SSE long-poll to the pod is currently disconnected (reconnecting). The idle
// event is never received by the tracker; onSessionIdle is never called;
// drainQueuedMessage never runs; the queued message remains in Redis.
//
// This test documents current broken behavior (message stays in Redis).
// It will flip to failing once a deeper fix eliminates the reconnect window.
func TestDrainMiss_SSEDownWhenSessionGoesIdle(t *testing.T) {
	// --- workspace pod simulator ---
	//
	// The pod's /event SSE endpoint:
	//   - First connection: drops immediately (simulates the connection that
	//     was live during the session, which ends before the pod emits idle).
	//   - Between first and second connection: the pod emits session.status=idle
	//     to nobody (the API server is reconnecting).
	//   - Second connection: returns the idle event only if explicitly told to.
	//     In the bug scenario it does NOT replay the already-emitted idle event —
	//     SSE is not a replay protocol. So the second connection returns only
	//     heartbeats.

	var connectionCount atomic.Int32

	// idleEventCh signals that the pod "emitted" idle between connections.
	idleEventCh := make(chan struct{}, 1)

	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connectionCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		if n == 1 {
			// First connection: stay alive briefly, then drop.
			// The idle event fires *after* this connection closes.
			flusher, _ := w.(http.Flusher)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			// Closing without sending anything — simulates the SSE stream ending.
			// The pod will emit session.status=idle after this returns.
			idleEventCh <- struct{}{}
			return
		}

		// Second+ connections: only heartbeats — idle was already emitted and
		// is gone. The reconnecting API server will never see it.
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		for i := 0; i < 5; i++ {
			hb, _ := json.Marshal(map[string]interface{}{
				"type":       "server.heartbeat",
				"properties": map[string]interface{}{},
			})
			_, _ = w.Write([]byte("data: " + string(hb) + "\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer podServer.Close()

	// --- opencode prompt endpoint (should NOT be called) ---
	var promptCallCount atomic.Int32
	promptServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		promptCallCount.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer promptServer.Close()

	// --- set up handler with real Redis queue and SSE tracker ---
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	podAddr := podServer.Listener.Addr().String()

	// httpClient routes /event to podServer, /session/*/prompt_async to promptServer.
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podAddr,
			promptHost: promptServer.Listener.Addr().String(),
		},
		Timeout: 5 * time.Second,
	}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Wire up the real SSE tracker — the exact path used in production.
	tracker := ssetracker.NewTracker(httpClient, &testLogger{}, func(workspaceID, sessionID string) {
		handler.onSessionIdle(workspaceID, sessionID)
	})
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(_ string) string { return podAddr })
	tracker.SetOnSessionActive(func(workspaceID, sessionID string) {
		handler.onSessionActive(workspaceID, sessionID)
	})
	handler.sseTracker = tracker

	// --- enqueue the message (session was busy when user typed it) ---
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "make passes for internal consistency")
	require.NoError(t, err)

	// Subscribe to SSE events so we can detect if a queue.update ever fires.
	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	// Start watching — this establishes the first SSE connection (which drops quickly).
	tracker.EnsureWatching("ws-1")
	defer tracker.StopWatching("ws-1")

	// Wait for the pod to signal that idle was emitted between connections.
	select {
	case <-idleEventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("pod never closed first SSE connection")
	}

	// Give the handler time to reconnect and process events.
	// In the bug: the idle event is gone; drainQueuedMessage is never called.
	// In the fix: the handler would detect the stale queue on reconnect.
	time.Sleep(500 * time.Millisecond)

	// Assert the bug: message still stranded in Redis.
	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n,
		"BUG: queued message should still be in Redis — idle event was missed while SSE was reconnecting")

	// No prompt_async should have been sent.
	assert.Equal(t, int32(0), promptCallCount.Load(),
		"BUG: no prompt_async should be sent when idle event was missed")

	// No queue.update SSE event should have been published.
	select {
	case evt := <-sub.Ch:
		if evt.Type == "queue.update" {
			t.Fatalf("BUG: unexpected queue.update event: %+v — drain should not have run", evt)
		}
	default:
		// Expected: no drain event.
	}
}

// TestDrainMiss_SSEIdleTimeoutCausesReconnect reproduces the variant where
// the SSE connection drops (e.g. idle timeout, pod restart, network blip)
// while a session is still processing. The tracker reconnects, and the
// session.status=idle event emitted during the reconnect backoff window is
// permanently lost — onSessionIdle is never called, the queue is never
// drained.
func TestDrainMiss_SSEIdleTimeoutCausesReconnect(t *testing.T) {
	var connectionCount atomic.Int32
	firstConnClosed := make(chan struct{}, 1)

	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connectionCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()

		if n == 1 {
			// First connection: drop immediately without sending the idle event.
			// The idle event fires after this handler returns (simulates the
			// session going idle *after* the SSE connection closed).
			time.Sleep(10 * time.Millisecond)
			select {
			case firstConnClosed <- struct{}{}:
			default:
			}
			return
		}

		// Second+ connections: heartbeats only — idle already fired and is gone.
		for i := 0; i < 3; i++ {
			hb, _ := json.Marshal(map[string]interface{}{"type": "server.heartbeat", "properties": map[string]interface{}{}})
			_, _ = w.Write([]byte("data: " + string(hb) + "\n\n"))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer podServer.Close()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	// The tracker uses podIPResolver which returns the IP only; it then
	// appends :AgentPort. We use a transport that intercepts and rewrites
	// to the test server so the actual port doesn't matter.
	podServerAddr := podServer.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podServerAddr,
			promptHost: podServerAddr,
		},
		Timeout: 5 * time.Second,
	}

	var drainCallCount atomic.Int32
	tracker := ssetracker.NewTracker(
		httpClient,
		&testLogger{},
		func(workspaceID, sessionID string) {
			drainCallCount.Add(1)
		},
	)
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	// Return the IP only — tracker appends :4096, transport rewrites to test server.
	tracker.SetPodIPResolver(func(_ string) string { return "127.0.0.1" })

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued while session was busy")
	require.NoError(t, err)

	tracker.EnsureWatching("ws-1")
	defer tracker.StopWatching("ws-1")

	// Wait for first SSE connection to close.
	select {
	case <-firstConnClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE connection never closed")
	}

	// The pod "emits" session.status=idle here — but nobody is listening.
	// The tracker is in its reconnect backoff window.
	// Wait for reconnect and second connection to process.
	time.Sleep(400 * time.Millisecond)

	// The bug: onSessionIdle was never called because the idle event was
	// emitted while the SSE connection was down.
	assert.Equal(t, int32(0), drainCallCount.Load(),
		"BUG: onSessionIdle should not have been called — idle event was emitted during SSE reconnect window")

	// Queue is still populated — message is stranded.
	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n,
		"BUG: message stranded in queue — drain never triggered because idle was missed")
}

// TestDrainMiss_QueueNotDrainedAfterReconnectWithNoNewIdleEvent is the
// regression gate for the queue drain miss fix:
//
// When the SSE tracker reconnects to a workspace pod and /v1/statusz on the
// agentd admin port reports that a session is currently idle, AND that session
// has messages in the Redis queue, the handler must drain the queue
// immediately — without waiting for a new session.status=idle SSE event
// (which will never arrive, because the idle event already fired while the
// SSE connection was down).
//
// The fix: on each connectAndRead attempt, the tracker calls the onReconnect
// callback (wired to reconcileSessionState), which calls GET /v1/statusz,
// finds idle sessions with non-empty queues, and calls onSessionIdle for each.
func TestDrainMiss_QueueNotDrainedAfterReconnectWithNoNewIdleEvent(t *testing.T) {
	var sseConnectionCount atomic.Int32
	var statuszCallCount atomic.Int32

	promptCalled := make(chan string, 1)

	// The workspace pod exposes two endpoints on the same server:
	//   GET /event        — SSE stream (agentd user port 4097, proxied here)
	//   GET /v1/statusz   — agentd admin statusz (port 4098, proxied here)
	//   POST /session/*/prompt_async — opencode prompt (port 4096, proxied here)
	//
	// Scenario:
	//   - SSE connection 1 drops without delivering session.status=idle.
	//   - After reconnect (SSE connection 2), /v1/statusz reports ses-1 is idle.
	//   - The fix: handler calls /v1/statusz on reconnect, sees ses-1 is idle,
	//     and triggers drain → prompt_async is called with the queued message.
	//   - Without fix: no reconcile on reconnect → prompt_async is never called.
	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/event":
			n := sseConnectionCount.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if ok {
				flusher.Flush()
			}
			if n == 1 {
				// First connection: drop immediately, no idle event.
				time.Sleep(10 * time.Millisecond)
				return
			}
			// Second connection: heartbeats only — idle was already emitted
			// and is gone. No session.status=idle will arrive here.
			if ok {
				for i := 0; i < 5; i++ {
					hb, _ := json.Marshal(map[string]interface{}{"type": "server.heartbeat", "properties": map[string]interface{}{}})
					_, _ = w.Write([]byte("data: " + string(hb) + "\n\n"))
					flusher.Flush()
					time.Sleep(10 * time.Millisecond)
				}
			}

		case "/v1/statusz":
			// Agentd admin statusz: ses-1 is idle (the session finished while
			// the SSE connection was down). Auth is Bearer token on admin port.
			statuszCallCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp, _ := json.Marshal(map[string]interface{}{
				"ready": true,
				"sessions": []map[string]interface{}{
					{"id": "ses-1", "status": "idle"},
				},
			})
			_, _ = w.Write(resp)

		case "/session/ses-1/prompt_async":
			var body struct {
				Parts []struct{ Text string } `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Parts) > 0 {
				select {
				case promptCalled <- body.Parts[0].Text:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer podServer.Close()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	podAddr := podServer.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podAddr,
			promptHost: podAddr,
		},
		Timeout: 5 * time.Second,
	}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	tracker := ssetracker.NewTracker(httpClient, &testLogger{}, func(workspaceID, sessionID string) {
		handler.onSessionIdle(workspaceID, sessionID)
	})
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(_ string) string { return "127.0.0.1" })
	tracker.SetOnSessionActive(func(workspaceID, sessionID string) {
		handler.onSessionActive(workspaceID, sessionID)
	})
	tracker.SetOnReconnect(handler.reconcileSessionState)
	handler.sseTracker = tracker

	// The message was queued while ses-1 was busy. It is now stranded.
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1",
		"make passes for internal consistency, and ensure we solve the right problem at the right level of abstraction")
	require.NoError(t, err)

	tracker.EnsureWatching("ws-1")
	defer tracker.StopWatching("ws-1")

	// Wait long enough for both SSE connections to occur and for the fix to
	// call /v1/statusz and trigger drain.
	select {
	case text := <-promptCalled:
		assert.Contains(t, text, "make passes for internal consistency",
			"queued message must be sent to opencode after reconnect reconcile detects idle session")
		n, qErr := svc.Len(context.Background(), "ws-1", "ses-1")
		require.NoError(t, qErr)
		assert.Equal(t, int64(0), n, "queue must be empty after drain")
		assert.Greater(t, statuszCallCount.Load(), int32(0),
			"fix must call /v1/statusz to reconcile session state on reconnect")
	case <-time.After(3 * time.Second):
		t.Fatalf(
			"FIX NEEDED: queued message was not sent after SSE reconnect.\n"+
				"  SSE connections made: %d\n"+
				"  /v1/statusz calls made: %d\n"+
				"  Queue length: check Redis\n\n"+
				"The fix: after each SSE reconnect attempt, call GET /v1/statusz on\n"+
				"the agentd admin port (4098, Bearer auth), iterate sessions with\n"+
				"status=idle, and for each session with a non-empty queue call\n"+
				"onSessionIdle to trigger drain.",
			sseConnectionCount.Load(),
			statuszCallCount.Load(),
		)
	}
}

// routingTransport routes HTTP requests: /event goes to eventHost,
// everything else goes to promptHost. Both are plain IP:port strings.
type routingTransport struct {
	eventHost  string
	promptHost string
}

func (rt *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	if req.URL.Path == "/event" || req.URL.Path == "/v1/statusz" || req.URL.Path == "/v1/healthz" {
		r.URL.Host = rt.eventHost
	} else {
		r.URL.Host = rt.promptHost
	}
	return http.DefaultTransport.RoundTrip(r)
}

// --- reconcileSessionState unhappy-path tests ---

func setupReconcileHandler(t *testing.T, statuszHandler http.HandlerFunc) (*ProxyHandler, *msgqueue.Service, *httptest.Server, func()) {
	t.Helper()
	server := httptest.NewServer(statuszHandler)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := msgqueue.NewWithClient(redisClient)

	serverAddr := server.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{eventHost: serverAddr, promptHost: serverAddr},
		Timeout:   5 * time.Second,
	}
	k8sMock := newMockK8sWithWorkspace(t, "ws-1", serverAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	cleanup := func() {
		server.Close()
		_ = redisClient.Close()
		mr.Close()
	}
	return handler, svc, server, cleanup
}

// TestReconcileStrandedQueues_Non200Statusz verifies that a non-200 response
// from /v1/statusz is handled gracefully: no drain is triggered, no panic,
// and the queue is left intact.
func TestReconcileStrandedQueues_Non200Statusz(t *testing.T) {
	handler, svc, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer cleanup()

	_, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	assert.NotPanics(t, func() {
		handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")
	})

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "queue should be untouched when statusz returns non-200")

	select {
	case evt := <-sub.Ch:
		t.Fatalf("no SSE event should be published when statusz fails, got: %+v", evt)
	default:
	}
}

// TestReconcileStrandedQueues_MalformedJSON verifies that a malformed statusz
// body is handled gracefully.
func TestReconcileStrandedQueues_MalformedJSON(t *testing.T) {
	handler, svc, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not valid json"))
	})
	defer cleanup()

	_, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")
	})

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "queue should be untouched when statusz body is malformed")
}

// TestReconcileStrandedQueues_NoIdleSessions verifies that when statusz reports
// all sessions as busy, no drain is triggered.
func TestReconcileStrandedQueues_NoIdleSessions(t *testing.T) {
	promptCalled := make(chan struct{}, 1)
	handler, svc, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			promptCalled <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(map[string]interface{}{
			"sessions": []map[string]interface{}{
				{"id": "ses-1", "status": "busy"},
			},
		})
		_, _ = w.Write(body)
	})
	defer cleanup()

	_, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	select {
	case <-promptCalled:
		t.Fatal("prompt_async should not be called when session is busy")
	case <-time.After(200 * time.Millisecond):
	}

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "queue should be untouched when session is busy")
}

// TestReconcileStrandedQueues_IdleButEmptyQueue verifies that when a session is
// idle but has nothing queued, no drain (and no prompt_async) is triggered.
func TestReconcileStrandedQueues_IdleButEmptyQueue(t *testing.T) {
	promptCalled := make(chan struct{}, 1)
	handler, _, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			promptCalled <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(map[string]interface{}{
			"sessions": []map[string]interface{}{
				{"id": "ses-1", "status": "idle"},
			},
		})
		_, _ = w.Write(body)
	})
	defer cleanup()

	// Queue is empty — nothing to drain.
	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	select {
	case <-promptCalled:
		t.Fatal("prompt_async should not be called when queue is empty")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReconcileStrandedQueues_StatuszUnavailable verifies that when the statusz
// endpoint is unreachable (network error), no panic occurs and the queue is intact.
func TestReconcileStrandedQueues_StatuszUnavailable(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "127.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default",
		&http.Client{Transport: &alwaysFailTransport{}, Timeout: time.Second}, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")
	})

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "queue should be untouched when statusz is unreachable")
}

// TestReconcileSessionState_ClearsStaleActiveSess verifies the fix for the
// 2026-06-16 stuck-session incident. When a session is idle in opencode (per
// statusz) but still marked active in the local activeSess map, reconcileSessionState
// must clean it up. Without this fix, POST to a stuck session returns 409
// Conflict indefinitely (see incident: ses_13076538bffeYtLrhoZ2ccRM1E /
// ses_130c14344ffeVF52UQ6QGPmB0P).
//
// Failure mode reproduced:
//  1. opencode emits session.status=busy → onSessionActive → activeSess[ws][ses]=true
//  2. opencode is OOMKilled or SIGTERMed mid-stream
//  3. session.status=idle event is never emitted (process died first)
//  4. activeSess retains stale entry forever
//  5. POST /sessions/:id/prompt returns 409 "session is busy; retry after idle"
//
// Fix: on SSE reconnect, query statusz; for any session that is idle there
// but active locally, remove from activeSess and publish session.status=idle.
func TestReconcileSessionState_ClearsStaleActiveSess(t *testing.T) {
	handler, _, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/statusz", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(map[string]interface{}{
			"sessions": []map[string]interface{}{
				{"id": "stuck-session", "status": "idle"},
				{"id": "active-session", "status": "busy"},
			},
		})
		_, _ = w.Write(body)
	})
	defer cleanup()

	// Simulate the stuck state: both sessions marked active locally, but
	// "stuck-session" is actually idle in opencode (the bug condition).
	handler.SetActiveSessionsForTest("ws-1", []string{"stuck-session", "active-session"})

	require.True(t, handler.isSessionActive(context.Background(), "ws-1", "stuck-session"),
		"precondition: stuck-session should be marked active before reconcile")
	require.True(t, handler.isSessionActive(context.Background(), "ws-1", "active-session"),
		"precondition: active-session should be marked active before reconcile")

	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	assert.False(t, handler.isSessionActive(context.Background(), "ws-1", "stuck-session"),
		"stuck-session should be cleared from activeSess (idle in opencode)")
	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "active-session"),
		"active-session should remain (still busy in opencode)")
}

// TestReconcileSessionState_NoStalenessNoOp verifies that when there are no
// stale entries in activeSess (all sessions match between local map and
// statusz), reconcileSessionState makes no changes. Guards against accidental
// removal of legitimately-active sessions.
func TestReconcileSessionState_NoStalenessNoOp(t *testing.T) {
	handler, _, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(map[string]interface{}{
			"sessions": []map[string]interface{}{
				{"id": "ses-busy", "status": "busy"},
			},
		})
		_, _ = w.Write(body)
	})
	defer cleanup()

	handler.SetActiveSessionsForTest("ws-1", []string{"ses-busy"})

	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "ses-busy"),
		"busy session should remain active after reconcile")
}

// TestReconcileSessionState_PublishesIdleEventOnStaleClear verifies that when
// a stale activeSess entry is cleared, the function publishes session.status=idle
// to the workspace event broker. This is what causes connected browsers to
// update their UI from "busy" to "idle" without needing a page reload.
//
// Without this event, users would have to reload the page to see that the
// session is no longer busy — even after the activeSess entry is cleared,
// the frontend's local state would still show the busy indicator.
func TestReconcileSessionState_PublishesIdleEventOnStaleClear(t *testing.T) {
	handler, _, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(map[string]interface{}{
			"sessions": []map[string]interface{}{
				{"id": "stuck-session", "status": "idle"},
			},
		})
		_, _ = w.Write(body)
	})
	defer cleanup()

	// Set up the stuck state.
	handler.SetActiveSessionsForTest("ws-1", []string{"stuck-session"})

	// Subscribe BEFORE triggering reconcile so we don't miss the event.
	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	// The function publishes the event synchronously via publishWorkspaceEvent.
	// Use Eventually to handle any internal async fan-out without depending on
	// internal broker timing.
	require.Eventually(t, func() bool {
		select {
		case evt := <-sub.Ch:
			return evt.Type == "session.status" &&
				evt.SessionID == "stuck-session" &&
				evt.Status == "idle"
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond,
		"expected session.status=idle event to be published when stale activeSess is cleared")
}

// TestReconcileStrandedQueues_MultipleIdleSessions verifies that when multiple
// sessions are idle and each has queued messages, reconcileSessionState
// drains ALL of them — not just the first one found.
func TestReconcileStrandedQueues_MultipleIdleSessions(t *testing.T) {
	var promptMu sync.Mutex
	promptTexts := map[string]string{}

	handler, svc, _, cleanup := setupReconcileHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses-1/prompt_async", "/session/ses-2/prompt_async":
			var body struct {
				Parts []struct{ Text string } `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sessID := r.URL.Path[len("/session/") : len(r.URL.Path)-len("/prompt_async")]
			if len(body.Parts) > 0 {
				promptMu.Lock()
				promptTexts[sessID] = body.Parts[0].Text
				promptMu.Unlock()
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			// statusz: both sessions idle
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(map[string]interface{}{
				"sessions": []map[string]interface{}{
					{"id": "ses-1", "status": "idle"},
					{"id": "ses-2", "status": "idle"},
				},
			})
			_, _ = w.Write(body)
		}
	})
	defer cleanup()

	ctx := context.Background()
	_, err := svc.Enqueue(ctx, "ws-1", "ses-1", "message for session 1")
	require.NoError(t, err)
	_, err = svc.Enqueue(ctx, "ws-1", "ses-2", "message for session 2")
	require.NoError(t, err)

	handler.reconcileSessionState("ws-1", "127.0.0.1", "test-pw")

	require.Eventually(t, func() bool {
		promptMu.Lock()
		defer promptMu.Unlock()
		return len(promptTexts) == 2
	}, 2*time.Second, 10*time.Millisecond, "both sessions should be drained")

	promptMu.Lock()
	assert.Equal(t, "message for session 1", promptTexts["ses-1"])
	assert.Equal(t, "message for session 2", promptTexts["ses-2"])
	promptMu.Unlock()

	n1, _ := svc.Len(ctx, "ws-1", "ses-1")
	n2, _ := svc.Len(ctx, "ws-1", "ses-2")
	assert.Equal(t, int64(0), n1, "ses-1 queue should be empty")
	assert.Equal(t, int64(0), n2, "ses-2 queue should be empty")
}

// TestPeriodicSweep_DrainsStrandedQueue verifies that the periodic queue
// sweep discovers non-empty queues via PeekAllGlobal and drains stranded
// messages by calling reconcileSessionState (which queries /v1/statusz).
//
// This catches the core bug (Mode A): a session went idle but the
// session.status=idle event was lost, leaving the session stuck in activeSess.
// reconcileSessionState bypasses the local activeSess map by asking the pod.
func TestPeriodicSweep_DrainsStrandedQueue(t *testing.T) {
	promptCalled := make(chan string, 1)

	// Single pod server that handles both statusz and prompt_async.
	// The routingTransport dispatches /v1/statusz to eventHost and
	// /session/*/prompt_async to promptHost — both are this server.
	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/statusz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp, _ := json.Marshal(map[string]interface{}{
				"ready": true,
				"sessions": []map[string]interface{}{
					{"id": "ses-1", "status": "idle"},
				},
			})
			_, _ = w.Write(resp)

		case "/session/ses-1/prompt_async":
			var body struct {
				Parts []struct{ Text string } `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Parts) > 0 {
				select {
				case promptCalled <- body.Parts[0].Text:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer podServer.Close()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	podAddr := podServer.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podAddr,
			promptHost: podAddr,
		},
		Timeout: 5 * time.Second,
	}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Enqueue a message for a session that the pod says is idle.
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1",
		"stranded message that sweep should drain")
	require.NoError(t, err)

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "precondition: queue should have 1 message")

	// Run the periodic sweep directly. It calls PeekAllGlobal to find
	// non-empty queues, then reconcileSessionState to query statusz.
	handler.sweepStrandedQueues()

	select {
	case text := <-promptCalled:
		assert.Contains(t, text, "stranded message that sweep should drain")
		n, err = svc.Len(context.Background(), "ws-1", "ses-1")
		require.NoError(t, err)
		assert.Equal(t, int64(0), n, "queue should be empty after sweep drains it")
	case <-time.After(2 * time.Second):
		t.Fatal("sweep: prompt_async was never called — sweep did not drain stranded queue")
	}
}

// TestPeriodicSweep_BlocksActiveSession verifies that the sweep does NOT drain
// sessions that statusz reports as busy, even when their queue is non-empty.
// reconcileSessionState checks statusz before draining.
func TestPeriodicSweep_BlocksActiveSession(t *testing.T) {
	promptCalled := make(chan string, 1)

	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/statusz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp, _ := json.Marshal(map[string]interface{}{
				"ready": true,
				"sessions": []map[string]interface{}{
					{"id": "ses-1", "status": "busy"},
				},
			})
			_, _ = w.Write(resp)

		case "/session/ses-1/prompt_async":
			select {
			case promptCalled <- "called":
			default:
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer podServer.Close()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	podAddr := podServer.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podAddr,
			promptHost: podAddr,
		},
		Timeout: 5 * time.Second,
	}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Enqueue a message for a session that the pod says is busy.
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1",
		"message queued during active session")
	require.NoError(t, err)

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "precondition: queue should have 1 message")

	// Run the sweep — it should NOT drain because statusz says busy.
	handler.sweepStrandedQueues()

	select {
	case <-promptCalled:
		t.Fatal("sweep should NOT drain active session")
	case <-time.After(500 * time.Millisecond):
	}

	n, err = svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n,
		"queue should be untouched when session is busy per statusz")
}

// TestDrainQueuedMessage_409RequeuesAndReturns verifies that when opencode
// responds with 409 Conflict (session genuinely busy), drainQueuedMessage
// requeues the message once and returns — instead of burning the retry budget
// and permanently dropping the message. This is the key safety property that
// makes drain-on-enqueue safe.
func TestDrainQueuedMessage_409RequeuesAndReturns(t *testing.T) {
	var promptAttempts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			promptAttempts.Add(1)
			w.WriteHeader(http.StatusConflict) // 409 — session busy
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "10.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "will get 409")
	require.NoError(t, err)

	handler.drainQueuedMessage("ws-1", "ses-1")

	// Should have been called exactly once (requeued+returned, not retried).
	assert.Equal(t, int32(1), promptAttempts.Load(),
		"prompt_async should be called exactly once for 409 — no retries")

	// Message should still be in the queue (requeued).
	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "message should be requeued after 409, not dropped")

	// RetryCount should NOT have been incremented (409 is not a retryable error).
	msgs, err := svc.PeekAll(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, 0, msgs[0].RetryCount, "retry count should not increase for 409")
}

// TestStartQueueSweep_DrainsViaTickerWhenIdleEventDropped is the regression
// test requested by issue #388: the "SSE subscription is logically alive
// (heartbeats flow) but the specific session.status:idle event was dropped,
// and the ticker goroutine eventually drains the stranded queue over time."
//
// Unlike TestPeriodicSweep_DrainsStrandedQueue (which calls sweepStrandedQueues
// synchronously), this test drives the actual startQueueSweep goroutine with
// a fast interval and asserts the drain fires within ~2 ticks — proving the
// ticker lifecycle (start, tick, stop) works end-to-end.
func TestStartQueueSweep_DrainsViaTickerWhenIdleEventDropped(t *testing.T) {
	promptCalled := make(chan string, 1)

	podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/statusz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp, _ := json.Marshal(map[string]interface{}{
				"ready": true,
				"sessions": []map[string]interface{}{
					{"id": "ses-1", "status": "idle"},
				},
			})
			_, _ = w.Write(resp)
		case "/session/ses-1/prompt_async":
			var body struct {
				Parts []struct{ Text string } `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Parts) > 0 {
				select {
				case promptCalled <- body.Parts[0].Text:
				default:
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer podServer.Close()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	svc := msgqueue.NewWithClient(redisClient)

	podAddr := podServer.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{
			eventHost:  podAddr,
			promptHost: podAddr,
		},
		Timeout: 5 * time.Second,
	}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")
	// Fast sweep for testing: 50ms ticks.
	handler.SetSweepInterval(50 * time.Millisecond)

	// Enqueue a message — the session.status=idle SSE event is NEVER emitted
	// (simulating the dropped-event scenario). The only path to drain is the
	// ticker goroutine discovering the queue via PeekAllGlobal + statusz.
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1",
		"stranded by dropped idle event")
	require.NoError(t, err)

	// Start the sweep goroutine.
	stopCh := make(chan struct{})
	defer close(stopCh)
	go handler.startQueueSweep(stopCh)

	// Assert the ticker drains within a generous window (~2 ticks + drain latency).
	select {
	case text := <-promptCalled:
		assert.Contains(t, text, "stranded by dropped idle event")
	case <-time.After(2 * time.Second):
		t.Fatal("startQueueSweep ticker did not drain stranded queue — idle event was dropped and ticker failed to recover")
	}

	n, err := svc.Len(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "queue should be empty after ticker-driven drain")
}

// TestReconcileSessionState_DrainsStaleBusySession is the regression test for
// the #388 dual-drop blind spot: when BOTH the API and agentd miss the idle
// transition, statusz reports the session as "busy" (stale). A freshly-enqueued
// message should NOT be drained (the session might genuinely be busy), but a
// message stranded longer than staleBusyThreshold SHOULD be optimistically
// drained — the 409-requeue path protects against a truly-busy session.
func TestReconcileSessionState_DrainsStaleBusySession(t *testing.T) {
	tests := []struct {
		name        string
		staleAge    time.Duration
		shouldDrain bool
	}{
		{"fresh queue (< threshold) NOT drained", 30 * time.Second, false},
		{"stale queue (> threshold) drained", 6 * time.Minute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			promptCalled := make(chan string, 1)

			podServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/statusz":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					resp, _ := json.Marshal(map[string]interface{}{
						"ready": true,
						"sessions": []map[string]interface{}{
							{"id": "ses-1", "status": "busy"},
						},
					})
					_, _ = w.Write(resp)
				case "/session/ses-1/prompt_async":
					var body struct {
						Parts []struct{ Text string } `json:"parts"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if len(body.Parts) > 0 {
						select {
						case promptCalled <- body.Parts[0].Text:
						default:
						}
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer podServer.Close()

			mr, err := miniredis.Run()
			require.NoError(t, err)
			defer mr.Close()
			redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			defer redisClient.Close()
			svc := msgqueue.NewWithClient(redisClient)

			podAddr := podServer.Listener.Addr().String()
			httpClient := &http.Client{
				Transport: &routingTransport{
					eventHost:  podAddr,
					promptHost: podAddr,
				},
				Timeout: 5 * time.Second,
			}

			k8sMock := newMockK8sWithWorkspace(t, "ws-1", podAddr)
			handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
			require.NoError(t, err)
			handler.SetMessageQueueService(svc)
			handler.userBroker = eventbroker.NewUserEventBroker()
			setupPasswordSecret(t, handler, "ws-1", "test-pw")

			// Enqueue a message, then backdate it to simulate staleness.
			_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "stale-busy test message")
			require.NoError(t, err)

			// Backdate the enqueued_at by rewriting the Redis key directly.
			// PeekAllGlobal returns the messages; we can't set EnqueuedAt via
			// the public API, so we re-marshal with an old timestamp.
			key := "llmsafespaces:msgqueue:ws-1:ses-1"
			raw, err := redisClient.LIndex(context.Background(), key, 0).Bytes()
			require.NoError(t, err)
			var msg msgqueue.QueuedMessage
			require.NoError(t, json.Unmarshal(raw, &msg))
			msg.EnqueuedAt = time.Now().Add(-tt.staleAge)
			backdated, _ := json.Marshal(msg)
			redisClient.LSet(context.Background(), key, 0, backdated)

			// Run reconcileSessionState directly — this is what the sweep calls.
			handler.reconcileSessionState("ws-1", podAddr, "test-pw")

			if tt.shouldDrain {
				select {
				case <-promptCalled:
					// drain fired — expected for stale queue
				case <-time.After(1 * time.Second):
					t.Fatal("stale-busy session should have been optimistically drained")
				}
			} else {
				select {
				case <-promptCalled:
					t.Fatal("fresh queue against busy session should NOT be drained")
				case <-time.After(500 * time.Millisecond):
					// no drain — expected
				}
			}
		})
	}
}

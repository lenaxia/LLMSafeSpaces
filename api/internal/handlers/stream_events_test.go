// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	k8swatch "k8s.io/apimachinery/pkg/watch"

	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

func newStreamEventsRouter(h *ProxyHandler) *gin.Engine {
	r := gin.New()
	r.GET("/api/v1/workspaces/:id/events", h.StreamEvents)
	return r
}

func doStreamingRequest(router *gin.Engine, path string) (cancel context.CancelFunc, body io.ReadCloser, respHeader http.Header, statusCode *int) {
	pr, pw := io.Pipe()
	sc := new(int)
	h := http.Header{}

	ctx, cancelFn := context.WithCancel(context.Background())

	go func() {
		req := httptest.NewRequestWithContext(ctx, "GET", path, nil)
		rw := &pipeResponseWriter{pw: pw, header: h, code: sc}
		router.ServeHTTP(rw, req)
		pw.Close()
	}()

	return cancelFn, pr, h, sc
}

type pipeResponseWriter struct {
	pw     *io.PipeWriter
	header http.Header
	code   *int
}

func (p *pipeResponseWriter) Header() http.Header { return p.header }
func (p *pipeResponseWriter) WriteHeader(code int) {
	if *p.code == 0 {
		*p.code = code
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if *p.code == 0 {
		*p.code = http.StatusOK
	}
	return p.pw.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

func readNextSSEDataLine(t *testing.T, r *bufio.Reader) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var m map[string]interface{}
			if jsonErr := json.Unmarshal([]byte(data), &m); jsonErr == nil {
				return m
			}
		}
		if err != nil {
			t.Fatalf("SSE stream ended unexpectedly: %v", err)
		}
	}
	t.Fatal("timed out waiting for SSE data line")
	return nil
}

// --- Tests ---

func TestStreamEvents_WorkspaceNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	wsMock.On("Get", mock.Anything, "ws-missing", metav1.GetOptions{}).
		Return(nil, fmt.Errorf("not found")).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()

	router := newStreamEventsRouter(handler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-missing/events", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestStreamEvents_SetsSSEHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, header, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer body.Close()

	time.Sleep(30 * time.Millisecond)
	cancel()

	assert.Equal(t, "text/event-stream", header.Get("Content-Type"))
	assert.Equal(t, "no-cache", header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", header.Get("Connection"))
}

// TestStreamEvents_ArmsUsageGateOnOpen (US-69.11 port of the old
// EnsureWatching test): opening the workspace stream must arm the
// busy-gated usage stream for that workspace, exactly as the tracker
// watch armed on /events open.
func TestStreamEvents_ArmsUsageGateOnOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	consumer, fc, _ := newRecordingGateConsumer(nil)
	t.Cleanup(injectUsageStream(consumer))
	// Gate arming on /events open rides the agentd-terminus path
	// (US-69.8 flag; production app wiring enables it).
	env.handler.agentdTerminus = true

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	select {
	case <-fc.connected:
	case <-time.After(3 * time.Second):
		t.Fatal("usage gate did not connect after /events was opened; UsageStream().Open not called from StreamEvents")
	}
}

// TestStreamEvents_BridgeSessionStatus_PublishesToUserStream: the
// bridge's busy/idle transitions surface as session.status on the user
// stream (the tracker's onSessionIdle/onSessionActive replacement).
func TestStreamEvents_BridgeSessionStatus_PublishesToUserStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	t.Cleanup(stubUsageStream())

	userSub, err := env.handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	bridge := &usageBridge{h: env.handler}
	bridge.SessionStatus("ws-1", "s2", true)
	bridge.SessionStatus("ws-1", "s1", false)

	evt := recvWithTimeout(t, userSub, "session.status busy")
	assert.Equal(t, "session.status", evt.Type)
	assert.Equal(t, "s2", evt.SessionID)
	assert.Equal(t, "busy", evt.Status)
	assert.Equal(t, "ws-1", evt.WorkspaceID)

	evt = recvWithTimeout(t, userSub, "session.status idle")
	assert.Equal(t, "session.status", evt.Type)
	assert.Equal(t, "s1", evt.SessionID)
	assert.Equal(t, "idle", evt.Status)
}

func TestStreamEvents_PhaseEventDeliveredToClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return broker.WorkspaceSubscriberCount("ws-1") > 0
	}, time.Second, 5*time.Millisecond)

	broker.PublishToWorkspace("ws-1", apitypes.WorkspaceSSEEvent{Type: "workspace.phase", Phase: "Suspended"})

	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "workspace.phase", evt["type"])
	assert.Equal(t, "Suspended", evt["phase"])
}

func TestStreamEvents_SessionStatusEventDeliveredToClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return broker.WorkspaceSubscriberCount("ws-1") > 0
	}, time.Second, 5*time.Millisecond)

	broker.PublishToWorkspace("ws-1", apitypes.WorkspaceSSEEvent{
		Type:      "session.status",
		SessionID: "s1",
		Status:    "idle",
	})

	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "session.status", evt["type"])
	assert.Equal(t, "s1", evt["session_id"])
	assert.Equal(t, "idle", evt["status"])
}

func TestStreamEvents_ClientDisconnectUnsubscribes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer body.Close()

	require.Eventually(t, func() bool {
		return broker.WorkspaceSubscriberCount("ws-1") > 0
	}, time.Second, 5*time.Millisecond)

	cancel()

	assert.Eventually(t, func() bool {
		return broker.WorkspaceSubscriberCount("ws-1") == 0
	}, time.Second, 5*time.Millisecond, "broker should unsubscribe disconnected client")
}

// TestStreamEvents_TooManySubscribers_Returns429 verifies that when a workspace
// reaches its subscriber limit, the next SSE request gets 429 instead of a
// nil-pointer panic (regression test for US-38.8 broker consolidation).
func TestStreamEvents_TooManySubscribers_Returns429(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.wsMock.On("Get", mock.Anything, "ws-limit", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-limit", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-limit"), nil).Maybe()

	// Exhaust the subscriber limit.
	for i := 0; i < eventbroker.MaxSubscribersPerUser; i++ {
		sub, err := broker.SubscribeWorkspace("ws-limit")
		require.NoError(t, err, "subscription %d should succeed", i)
		defer broker.UnsubscribeWorkspace("ws-limit", sub)
	}

	// Next request should get 429, not panic.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-limit/events", nil)
	newStreamEventsRouter(env.handler).ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestStreamEvents_OnPhaseChange_PublishesToBroker(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	userBroker := eventbroker.NewUserEventBroker()
	handler.userBroker = userBroker

	s, subErr := userBroker.SubscribeUser("user-1")
	require.NoError(t, subErr)
	defer userBroker.UnsubscribeUser("user-1", s)

	phases := []string{
		string(llmv1.WorkspacePhaseActive),
		"Suspending",
		"Suspended",
		"Terminating",
		"Terminated",
	}

	for _, phase := range phases {
		ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", phase, "ws-1")
		ws.Spec.Owner.UserID = "user-1"
		handler.onPhaseChange(ws)

		select {
		case evt := <-s.Ch:
			assert.Equal(t, "workspace.phase", evt.Type, "phase=%s", phase)
			assert.Equal(t, phase, evt.Phase, "phase=%s", phase)
			assert.Equal(t, "ws-1", evt.WorkspaceID, "phase=%s", phase)
		case <-time.After(time.Second):
			t.Fatalf("expected phase event for phase %s", phase)
		}
	}
}

// --- US-69.11: derived-state events via the usage bridge ---
//
// The onRawEvent → agent.event relay tests were deleted with the
// tracker (the raw dialect relay is gone). The broker fan-out contract
// for agent.event-shaped payloads is kept by
// TestStreamEvents_OpenCodeEventDeliveredToSSEClient below; derived
// session.status events now originate from usageBridge.SessionStatus.

func TestStreamEvents_OpenCodeEventDeliveredToSSEClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return broker.WorkspaceSubscriberCount("ws-1") > 0
	}, time.Second, 5*time.Millisecond)

	broker.PublishToWorkspace("ws-1", apitypes.WorkspaceSSEEvent{
		Type:      "agent.event",
		EventType: "message.part.updated",
		Data: map[string]interface{}{
			"directory": "ws-1",
			"payload": map[string]interface{}{
				"type":       "message.part.updated",
				"properties": `{"sessionID":"s1","part":{"type":"text","text":"hello"}}`,
			},
		},
	})

	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "agent.event", evt["type"])
	assert.Equal(t, "message.part.updated", evt["event_type"])
	require.Contains(t, evt, "data")
}

// --- #906 r5: stream-lifecycle logs (G7) on both SSE endpoints ---

// captureStreamLogger records Info lines for the stream lifecycle tests.
type captureStreamLogger struct {
	mu    sync.Mutex
	lines []string
	testLogger
}

func (l *captureStreamLogger) Info(msg string, kv ...interface{}) {
	l.mu.Lock()
	l.lines = append(l.lines, msg+" "+fmt.Sprint(kv...))
	l.mu.Unlock()
}

func (l *captureStreamLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }

func (l *captureStreamLogger) infos(filter string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, filter) {
			out = append(out, s)
		}
	}
	return out
}

// TestStreamEvents_LifecycleLogs pins the G7 contract on the workspace
// stream: open logs the subscriber count WITHOUT double-counting this
// stream (SubscribeWorkspace ran before the count), close logs
// duration + eventsSent (NOT counting heartbeats).
func TestStreamEvents_LifecycleLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	logger := &captureStreamLogger{}
	env.handler.logger = logger
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(env.handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool { return len(logger.infos("SSE client stream opened")) == 1 },
		2*time.Second, 5*time.Millisecond, "open must log exactly once")
	openLine := logger.infos("SSE client stream opened")[0]
	// Exact literal against the capture-logger rendering — a reintroduced
	// +1 yields "[2 1]" here and fails (round-5's loose "contains 1"
	// matched "[2 1]" and was mutation-proven vacuous).
	assert.Equal(t, "SSE client stream opened workspaceIDws-1subscribersIncludingSelf1", openLine, "no +1 double-count (+1 renders ...Self2; exact match is mutation-proof)")

	// Deliver one real event; then close and assert eventsSent==1.
	require.Eventually(t, func() bool { return broker.WorkspaceSubscriberCount("ws-1") > 0 },
		time.Second, 5*time.Millisecond)
	broker.PublishToWorkspace("ws-1", apitypes.WorkspaceSSEEvent{Type: "workspace.phase", Phase: "Active"})
	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "workspace.phase", evt["type"])

	cancel()
	require.Eventually(t, func() bool { return len(logger.infos("SSE client stream closed")) == 1 },
		2*time.Second, 5*time.Millisecond, "close must log exactly once")
	closed := logger.infos("SSE client stream closed")[0]
	assert.Contains(t, closed, "eventsSent", "close logs the event count")
	assert.Contains(t, closed, "eventsSent1", "exactly one data event sent (heartbeats are comment frames, not counted)")
}

// TestStreamEvents_429DoesNotLogLifecycle: a rejected (429) connection
// must not produce open/close lifecycle logs — the count semantics only
// apply to established streams. Uses the same rate-limit seam as
// production (sseConnAllowed).
func TestStreamEvents_429DoesNotLogLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	logger := &captureStreamLogger{}
	env.handler.logger = logger
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).
		Return(makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil).Maybe()

	// Exhaust the per-IP connection budget.
	sseConnMu.Lock()
	for k := range sseConnCounts {
		delete(sseConnCounts, k)
	}
	sseConnCounts["192.0.2.1"] = &sseConnAttempt{count: sseConnRateLimit, resetAt: time.Now().Add(time.Minute)}
	sseConnMu.Unlock()
	t.Cleanup(func() {
		sseConnMu.Lock()
		delete(sseConnCounts, "192.0.2.1")
		sseConnMu.Unlock()
	})

	// The workspace-stream endpoint (StreamEvents) has no 429 rate gate
	// itself (its cap is subscriber-count based); the USER endpoint does
	// (sseConnAllowed at stream_user_events.go:46). Drive that one.
	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "u-1")
		env.handler.StreamUserEvents(c)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code, "rate-limited attempt rejected")
	assert.Empty(t, logger.infos("SSE user stream opened"),
		"a rejected connection must not log stream-open (it never opened)")
}

// TestStreamUserEvents_LifecycleLogs (#906 r6): the USER stream gets
// the same open/close lifecycle logs, and its eventsSent EXCLUDES
// heartbeats (they are keepalive comment frames — the round-5
// inconsistency between the two endpoints, pinned by injecting a
// sentinel directly, since the 25s ticker cannot fire in a test).
func TestStreamUserEvents_LifecycleLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	logger := &captureStreamLogger{}
	env.handler.logger = logger

	env.wsMock.On("List", mock.Anything, mock.Anything).
		Return(&llmv1.WorkspaceList{}, nil).Maybe() // snapshot goroutine
	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "u-lifecycle")
		env.handler.StreamUserEvents(c)
	})

	cancel, body, _, _ := doStreamingRequest(router, "/api/v1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool { return len(logger.infos("SSE user stream opened")) == 1 },
		2*time.Second, 5*time.Millisecond, "user-stream open logs once")

	// A heartbeat sentinel (injected as heartbeatLoop would) must NOT be
	// counted; a real event must be.
	require.Eventually(t, func() bool { return broker.UserSubscriberCount("u-lifecycle") > 0 },
		time.Second, 5*time.Millisecond)
	broker.PublishToUser("u-lifecycle", apitypes.WorkspaceSSEEvent{Type: eventbroker.HeartbeatSentinelType})
	broker.PublishToUser("u-lifecycle", apitypes.WorkspaceSSEEvent{Type: "workspace.phase", Phase: "Active", WorkspaceID: "ws-x"})

	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "workspace.phase", evt["type"], "the data event arrives (heartbeat is a comment frame)")

	cancel()
	require.Eventually(t, func() bool { return len(logger.infos("SSE user stream closed")) == 1 },
		2*time.Second, 5*time.Millisecond)
	closed := logger.infos("SSE user stream closed")[0]
	assert.Contains(t, closed, "eventsSent1",
		"exactly one data event counted; the heartbeat sentinel is excluded on the user stream too")
}

// TestStreamUserEvents_SnapshotBranchCounted (#906 r7): snapshot frames
// (EventID == 0 — no id: line) go through their own increment branch.
// Wires a real watcher so the snapshot goroutine sees a non-empty phase
// (F4 skips empty phases), then asserts the snapshot frame is written
// AND counted in eventsSent.
func TestStreamUserEvents_SnapshotBranchCounted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	logger := &captureStreamLogger{}
	env.handler.logger = logger

	wsList := &llmv1.WorkspaceList{}
	wsList.Items = []llmv1.Workspace{{}}
	wsList.Items[0].Name = "ws-snap"
	wsList.Items[0].Status.Phase = llmv1.WorkspacePhaseActive
	env.wsMock.On("List", mock.Anything, mock.Anything).Return(wsList, nil).Maybe()
	env.wsMock.On("Watch", mock.Anything, mock.Anything).Return(k8swatch.NewFake(), nil).Maybe()

	// Real watcher: its seed populates knownPhases[ws-snap]=Active, which
	// the snapshot path requires (empty-phase workspaces are skipped, F4).
	w, err := workspace.NewWatcher(env.k8sMock, &testLogger{}, "default", func(*llmv1.Workspace) {})
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
	require.Eventually(t, func() bool {
		return w.GetAllKnownPhases()["ws-snap"] == "Active"
	}, 3*time.Second, 10*time.Millisecond, "watcher seed must populate the phase")
	env.handler.watcher = w

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", "u-snap")
		env.handler.StreamUserEvents(c)
	})

	cancel, body, _, _ := doStreamingRequest(router, "/api/v1/events")
	defer cancel()
	defer body.Close()

	// The snapshot frame is a data event without an id: line.
	evt := readNextSSEDataLine(t, bufio.NewReader(body))
	assert.Equal(t, "workspace.phase", evt["type"], "snapshot phase event delivered")
	assert.Equal(t, "Active", evt["phase"])

	cancel()
	require.Eventually(t, func() bool { return len(logger.infos("SSE user stream closed")) == 1 },
		2*time.Second, 5*time.Millisecond)
	closed := logger.infos("SSE user stream closed")[0]
	assert.Contains(t, closed, "eventsSent1",
		"the snapshot branch's increment is pinned (round-7 coverage gap)")
}

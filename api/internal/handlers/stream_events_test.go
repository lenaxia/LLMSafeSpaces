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

	"github.com/gin-gonic/gin"
	llmv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
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

func TestStreamEvents_EnsuresWatchingOnOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)

	trackerConnected := make(chan struct{}, 1)
	sseBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			select {
			case trackerConnected <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer sseBackend.Close()

	transport := &redirectTransport{server: sseBackend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	secret := makePasswordSecret("ws-1", "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(
		makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(llmv1.WorkspacePhaseActive), "ws-1"), nil,
	).Maybe()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	handler.sseTracker = sse.NewTracker(httpClient, &testLogger{}, handler.onSessionIdle)
	handler.sseTracker.SetPasswordGetter(handler)
	handler.sseTracker.SetPodIPResolver(handler.getPodIPForSSE)
	handler.sseTracker.SetOnSessionActive(handler.onSessionActive)
	handler.userBroker = eventbroker.NewUserEventBroker()

	cancel, body, _, _ := doStreamingRequest(newStreamEventsRouter(handler), "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	select {
	case <-trackerConnected:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE tracker did not connect to pod after /events was opened; EnsureWatching not called from StreamEvents")
	}
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

func TestStreamEvents_OnSessionIdle_PublishesToBroker(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	handler.onSessionIdle("ws-1", "s1")

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "idle", evt.Status)
	case <-time.After(time.Second):
		t.Fatal("expected session.status idle event from onSessionIdle")
	}
}

// --- onRawEvent -> broker pipeline ---

func TestStreamEvents_OnRawEvent_PublishesOpenCodeEvent(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	rawData := `{"directory":"ws-1","payload":{"type":"message.part.updated","properties":{"sessionID":"sess-1","part":{"type":"text","text":"hello"}}}}`
	handler.onRawEvent("ws-1", "message.part.updated", rawData)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
		assert.Equal(t, "message.part.updated", evt.EventType)
		require.NotNil(t, evt.Data)
		dataMap, ok := evt.Data.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "ws-1", dataMap["directory"])
	case <-time.After(time.Second):
		t.Fatal("expected opencode.event from onRawEvent")
	}
}

func TestStreamEvents_OnRawEvent_PublishesAllEventTypes(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	events := []struct {
		eventType string
		data      string
	}{
		{"message.part.updated", `{"directory":"ws-1","payload":{"type":"message.part.updated","properties":{"sessionID":"s1"}}}`},
		{"message.updated", `{"directory":"ws-1","payload":{"type":"message.updated","properties":{"sessionID":"s1"}}}`},
		{"session.diff", `{"directory":"ws-1","payload":{"type":"session.diff","properties":{"sessionID":"s1"}}}`},
		{"session.error", `{"directory":"ws-1","payload":{"type":"session.error","properties":{"sessionID":"s1","error":"something went wrong"}}}`},
	}

	for _, e := range events {
		handler.onRawEvent("ws-1", e.eventType, e.data)

		select {
		case evt := <-sub.Ch:
			assert.Equal(t, "agent.event", evt.Type)
			assert.Equal(t, e.eventType, evt.EventType)
		case <-time.After(time.Second):
			t.Fatalf("expected opencode.event for type %s", e.eventType)
		}
	}
}

func TestStreamEvents_OnRawEvent_NilBrokerDoesNotPanic(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	handler.onRawEvent("ws-1", "message.part.updated", `{"foo":"bar"}`)
}

func TestStreamEvents_OnRawEvent_UnparsableJSONData(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	handler.onRawEvent("ws-1", "session.status", "not-json-at-all")

	// With the early-return fix (US-65.5), unparsable events are dropped
	// entirely — no opencode.event with nil Data is forwarded. The channel
	// should have no events.
	select {
	case evt := <-sub.Ch:
		t.Fatalf("expected no event for unparsable data, got: %+v", evt)
	case <-time.After(100 * time.Millisecond):
		// Expected: no event forwarded.
	}
}

func TestStreamEvents_OnRawEvent_PreservesNestedStructure(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	rawData := `{"directory":"ws-1","payload":{"type":"message.part.updated","properties":{"sessionID":"sess-1","part":{"type":"text","text":"hello world"}}}}`
	handler.onRawEvent("ws-1", "message.part.updated", rawData)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
		require.NotNil(t, evt.Data)

		dataMap, ok := evt.Data.(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "ws-1", dataMap["directory"])

		payload, ok := dataMap["payload"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "message.part.updated", payload["type"])

		props, ok := payload["properties"].(map[string]interface{})
		require.True(t, ok, "properties should be a map (JSON object)")
		assert.Equal(t, "sess-1", props["sessionID"])

		part, ok := props["part"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "text", part["type"])
		assert.Equal(t, "hello world", part["text"])
	case <-time.After(time.Second):
		t.Fatal("expected opencode.event with nested structure preserved")
	}
}

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

func TestStreamEvents_OnRawEvent_DifferentWorkspaceIsolation(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	sub1, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub1)
	sub2, _ := broker.SubscribeWorkspace("ws-2")
	defer broker.UnsubscribeWorkspace("ws-2", sub2)

	handler.onRawEvent("ws-1", "message.part.updated", `{"directory":"ws-1","payload":{"type":"message.part.updated","properties":{"sessionID":"s1"}}}`)

	select {
	case evt := <-sub1.Ch:
		assert.Equal(t, "agent.event", evt.Type)
	case <-time.After(time.Second):
		t.Fatal("ws-1 subscriber should receive opencode.event")
	}

	select {
	case <-sub2.Ch:
		t.Fatal("ws-2 subscriber should NOT receive ws-1's event")
	case <-time.After(200 * time.Millisecond):
	}
}

// --- Existing onSessionActive test ---

func TestStreamEvents_OnSessionActive_PublishesToBroker(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker

	handler.SetWorkspaceConfigForTest("ws-1", wsstate.Config{MaxActiveSessions: 5})

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	handler.onSessionActive("ws-1", "s2")

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s2", evt.SessionID)
		assert.Equal(t, "busy", evt.Status)
	case <-time.After(time.Second):
		t.Fatal("expected session.status busy event from onSessionActive")
	}
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

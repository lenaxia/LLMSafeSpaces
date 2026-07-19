package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	ssetracker "github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func setupQueueTestEnv(t *testing.T) (*ProxyHandler, *msgqueue.Service, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := msgqueue.NewWithClient(client)

	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	return handler, svc, func() {
		_ = client.Close()
		mr.Close()
	}
}

func TestEnqueueMessage_Success(t *testing.T) {
	handler, svc, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	// Mark session as active so the drain-on-enqueue goroutine does not fire.
	// Without this, EnqueueMessage spawns drainQueuedMessage which calls
	// getPodIPAndPassword → k8sClient.LlmsafespacesV1() on the bare mock,
	// panicking in a background goroutine.
	handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.EnqueueMessage(c)

	assert.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp["messageID"])

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(1), n)
}

func TestEnqueueMessage_EmptyText(t *testing.T) {
	handler, _, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue", strings.NewReader(`{"text":""}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEnqueueMessage_NoQueueService(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue", strings.NewReader(`{"text":"hi"}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestEnqueueMessage_DrainsWhenIdle verifies that EnqueueMessage triggers an
// immediate drain when the session is not in activeSess (already idle). Without
// this, a message enqueued after the agent finished would sit in Redis forever
// — no session.status=idle event will arrive because the session is already quiet.
//
// Also asserts (regression for the 2026-06-29 second-pass incident) that the
// drain path does NOT send a client-supplied messageID to opencode. Letting
// opencode generate its own user-message ID is the only way to keep the
// session.prompt loop's lex-ordering invariant correct in both directions
// (above the previous-turn assistant AND below the new-turn assistant).
// See worklog for the role-flip / silent-drop incidents.
func TestEnqueueMessage_DrainsWhenIdle(t *testing.T) {
	type promptCall struct {
		text      string
		messageID string
		bodyRaw   string
	}
	promptCalled := make(chan promptCall, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			bodyBytes, _ := io.ReadAll(r.Body)
			var body struct {
				Parts     []struct{ Text string } `json:"parts"`
				MessageID string                  `json:"messageID"`
			}
			_ = json.Unmarshal(bodyBytes, &body)
			if len(body.Parts) > 0 {
				select {
				case promptCalled <- promptCall{
					text:      body.Parts[0].Text,
					messageID: body.MessageID,
					bodyRaw:   string(bodyBytes),
				}:
				default:
				}
			}
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Session is NOT in activeSess — simulates an idle session.
	assert.False(t, handler.isSessionActive(context.Background(), "ws-1", "ses-1"))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue", strings.NewReader(`{"text":"drain me"}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// The drain goroutine should send the message to opencode immediately.
	select {
	case call := <-promptCalled:
		assert.Equal(t, "drain me", call.text)

		// Regression: opencode's session.prompt loop-exit predicate depends
		// on `lastUser.id < lastAssistant.id` under lex comparison. Any
		// client-supplied messageID risks landing on the wrong side of
		// either bound (above the new-turn assistant → infinite loop; below
		// the previous-turn assistant → silent drop without LLM call).
		// Letting opencode generate the ID via its monotonic
		// MessageID.ascending() is the only design that keeps the
		// invariant correct in both directions.
		assert.Empty(t, call.messageID,
			"messageID must not be sent on prompt_async; opencode must generate it")
		assert.NotContains(t, call.bodyRaw, "messageID",
			"prompt_async body must not contain a messageID key (omitempty) — got %s",
			call.bodyRaw)
	case <-time.After(2 * time.Second):
		t.Fatal("drain-on-enqueue: prompt_async was never called for idle session")
	}

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(0), n, "queue should be empty after drain-on-enqueue")
}

// TestEnqueueMessage_NoDrainWhenActive verifies that EnqueueMessage does NOT
// trigger drain when the session is active (busy). The message should stay in
// the queue until the session goes idle and onSessionIdle fires.
func TestEnqueueMessage_NoDrainWhenActive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Mark session as active (busy).
	handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue", strings.NewReader(`{"text":"wait for idle"}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// Give drain goroutine time to fire if it was going to (it should NOT).
	time.Sleep(200 * time.Millisecond)

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(1), n, "message should stay in queue when session is active")
}

// TestEnqueueMessage_DeletedSessionDoesNotDrain verifies that the drain-on-enqueue
// guard correctly skips deleted sessions. A late enqueue hitting a deleted session
// must NOT trigger drain — the message stays in the queue until the user deletes it
// or the workspace terminates (which clears all queues).
func TestEnqueueMessage_DeletedSessionDoesNotDrain(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Mark session as deleted (simulates user deleting the session).
	handler.MarkSessionDeletedForTest("ws-1", "ses-1")

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue", strings.NewReader(`{"text":"late msg"}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// Give drain goroutine time to fire if it was going to (it should NOT).
	time.Sleep(200 * time.Millisecond)

	// Message should stay in queue — deleted sessions must not be drained.
	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(1), n, "message should stay in queue for deleted session")
}

func TestListQueue_Success(t *testing.T) {
	handler, svc, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	_, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "first")
	require.NoError(t, err)
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "second")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/queue", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.ListQueue(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []msgqueue.QueuedMessage `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Len(t, resp.Messages, 2)
	assert.Equal(t, "first", resp.Messages[0].Text)
	assert.Equal(t, "second", resp.Messages[1].Text)
}

func TestListQueue_Empty(t *testing.T) {
	handler, _, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/queue", nil)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.ListQueue(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Messages []msgqueue.QueuedMessage `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Len(t, resp.Messages, 0)
}

func TestDrainQueuedMessage_EmptyQueue(t *testing.T) {
	handler, _, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	assert.NotPanics(t, func() {
		handler.drainQueuedMessage("ws-1", "ses-1")
	})

	select {
	case <-sub.Ch:
		t.Fatal("should not publish event when queue is empty")
	default:
	}
}

func TestDrainQueuedMessage_SendsToOpencode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/session/ses-1/prompt_async", r.URL.Path)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	go handler.drainQueuedMessage("ws-1", "ses-1")

	require.Eventually(t, func() bool {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				return false
			}
			data, ok := evt.Data.(queueUpdateData)
			return ok && data.Event == "sent"
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "should publish queue.update with event=sent")

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(0), n, "message should be consumed from queue")
}

func TestDrainQueuedMessage_RequeuesOnFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "will fail")
	require.NoError(t, err)

	go handler.drainQueuedMessage("ws-1", "ses-1")

	require.Eventually(t, func() bool {
		msgs, _ := svc.PeekAll(context.Background(), "ws-1", "ses-1")
		return len(msgs) == 1 && msgs[0].RetryCount == 1
	}, 3*time.Second, 10*time.Millisecond, "message should be requeued with incremented retry count")
}

func TestDrainQueuedMessage_DropsAfterMaxRetries(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	msg := msgqueue.QueuedMessage{
		ID:          "msg_maxed",
		Text:        "maxed out",
		SessionID:   "ses-1",
		WorkspaceID: "ws-1",
		RetryCount:  maxQueueRetries,
	}
	err = svc.Requeue(context.Background(), "ws-1", "ses-1", msg)
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	go handler.drainQueuedMessage("ws-1", "ses-1")

	require.Eventually(t, func() bool {
		select {
		case evt := <-sub.Ch:
			data, ok := evt.Data.(queueUpdateData)
			return ok && data.Event == "error"
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "should publish error event after max retries")

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(0), n, "message should be dropped after max retries")
}

func TestPublishQueueEvent(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	handler.publishQueueEvent("ws-1", "ses-1", "sent", "msg_123", "")

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "queue.update", evt.Type)
		assert.Equal(t, "ses-1", evt.SessionID)
		data, ok := evt.Data.(queueUpdateData)
		require.True(t, ok)
		assert.Equal(t, "sent", data.Event)
		assert.Equal(t, "msg_123", data.MessageID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for queue event")
	}
}

func TestDeleteQueueMessage_Success(t *testing.T) {
	handler, svc, cleanup := setupQueueTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	id, err := svc.Enqueue(ctx, "ws-1", "ses-1", "to delete")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/:id/sessions/:sessionId/queue/:messageId", handler.DeleteQueueMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/ws-1/sessions/ses-1/queue/"+id, nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)

	n, _ := svc.Len(ctx, "ws-1", "ses-1")
	assert.Equal(t, int64(0), n, "message should be removed from queue")

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "queue.update", evt.Type)
		data, ok := evt.Data.(queueUpdateData)
		require.True(t, ok)
		assert.Equal(t, "dismissed", data.Event)
		assert.Equal(t, id, data.MessageID)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dismissed SSE event")
	}
}

func TestDeleteQueueMessage_NoQueueService(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/:id/sessions/:sessionId/queue/:messageId", handler.DeleteQueueMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/ws-1/sessions/ses-1/queue/msg_123", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteQueueMessage_NotFound(t *testing.T) {
	handler, svc, cleanup := setupQueueTestEnv(t)
	defer cleanup()

	id, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "keep me")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/queue", nil)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
		{Key: "messageId", Value: "nonexistent"},
	}

	handler.DeleteQueueMessage(c)

	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(1), n, "unrelated message should remain")
	_ = id
}

// --- New tests for queue lifecycle behaviors ---

// TestOnPhaseChange_SuspendPublishesDismissedAndClears verifies that when a
// workspace transitions to Suspending, the handler publishes dismissed SSE
// events for all queued messages across all sessions and then clears the queue.
func TestOnPhaseChange_SuspendPublishesDismissedAndClears(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)

	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	ctx := context.Background()
	id1, err := svc.Enqueue(ctx, "ws-1", "ses-A", "msg 1")
	require.NoError(t, err)
	id2, err := svc.Enqueue(ctx, "ws-1", "ses-B", "msg 2")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	// Build a minimal workspace object in Suspending phase
	ws := makeWorkspaceCRDWithStatus("ws-1", "", string(v1.WorkspacePhaseSuspending), "")
	handler.onPhaseChange(ws)

	// Collect dismissed events (up to 2) within a reasonable timeout
	dismissed := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(dismissed) < 2 {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				continue
			}
			data, ok := evt.Data.(queueUpdateData)
			if ok && data.Event == "dismissed" {
				dismissed[data.MessageID] = true
			}
		case <-deadline:
			t.Fatalf("timed out; only saw dismissed for: %v", dismissed)
		}
	}
	assert.True(t, dismissed[id1], "id1 should be dismissed")
	assert.True(t, dismissed[id2], "id2 should be dismissed")

	// Queue should be cleared
	n1, _ := svc.Len(ctx, "ws-1", "ses-A")
	n2, _ := svc.Len(ctx, "ws-1", "ses-B")
	assert.Equal(t, int64(0), n1, "ses-A queue should be cleared")
	assert.Equal(t, int64(0), n2, "ses-B queue should be cleared")
}

// TestAbortSession_FlushesQueueThenAborts verifies that AbortSession:
// 1. Publishes dismissed SSE events for all queued messages
// 2. Clears the queue from Redis
// 3. Proxies the abort to opencode
// 4. Launches the background flush-and-abort goroutine
func TestAbortSession_FlushesQueueThenAborts(t *testing.T) {
	abortCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/abort" {
			abortCalled = true
			w.WriteHeader(http.StatusNoContent)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	ctx := context.Background()
	id1, _ := svc.Enqueue(ctx, "ws-1", "ses-1", "queued msg 1")
	id2, _ := svc.Enqueue(ctx, "ws-1", "ses-1", "queued msg 2")

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/:id/sessions/:sessionId/abort", handler.AbortSession)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	// Abort should proxy through
	assert.True(t, abortCalled, "abort should be proxied to opencode")

	// Queue should be cleared from Redis immediately
	require.Eventually(t, func() bool {
		n, _ := svc.Len(ctx, "ws-1", "ses-1")
		return n == 0
	}, 2*time.Second, 10*time.Millisecond, "queue should be cleared")

	// dismissed SSE events should be published for each queued message
	dismissed := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(dismissed) < 2 {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				continue
			}
			data, ok := evt.Data.(queueUpdateData)
			if ok && data.Event == "dismissed" {
				dismissed[data.MessageID] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for dismissed events; got: %v", dismissed)
		}
	}
	assert.True(t, dismissed[id1], "id1 should be dismissed")
	assert.True(t, dismissed[id2], "id2 should be dismissed")
}

// TestAbortSession_EmptyQueue_JustAborts verifies that AbortSession with no
// queued messages simply proxies the abort without touching the queue.
func TestAbortSession_EmptyQueue_JustAborts(t *testing.T) {
	abortCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/abort" {
			abortCalled = true
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/:id/sessions/:sessionId/abort", handler.AbortSession)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	assert.True(t, abortCalled, "abort should be proxied even with empty queue")
}

// TestClearQueueOnDispose_PublishesDismissedAndClears verifies that
// AgentReloadHandler.clearQueueOnDispose publishes dismissed SSE events for
// all queued messages and then clears the queue.
func TestClearQueueOnDispose_PublishesDismissedAndClears(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	broker := eventbroker.NewUserEventBroker()

	k8sMock := k8smocks.NewMockKubernetesClient()
	_ = k8sMock
	handler := NewAgentReloadHandler(nil, nil, nil, nil, &testLogger{})
	handler.SetQueueClearer(svc)
	handler.SetBrokerPublisher(broker)

	ctx := context.Background()
	id1, err := svc.Enqueue(ctx, "ws-1", "ses-A", "pending msg 1")
	require.NoError(t, err)
	id2, err := svc.Enqueue(ctx, "ws-1", "ses-B", "pending msg 2")
	require.NoError(t, err)

	sub, _ := broker.SubscribeWorkspace("ws-1")
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	handler.clearQueueOnDispose(ctx, "ws-1")

	// Collect dismissed events
	dismissed := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(dismissed) < 2 {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				continue
			}
			data, ok := evt.Data.(queueUpdateData)
			if ok && data.Event == "dismissed" {
				dismissed[data.MessageID] = true
			}
		case <-deadline:
			t.Fatalf("timed out; got: %v", dismissed)
		}
	}
	assert.True(t, dismissed[id1])
	assert.True(t, dismissed[id2])

	n1, _ := svc.Len(ctx, "ws-1", "ses-A")
	n2, _ := svc.Len(ctx, "ws-1", "ses-B")
	assert.Equal(t, int64(0), n1)
	assert.Equal(t, int64(0), n2)
}

func newMockK8sWithWorkspace(t *testing.T, workspaceID, podIP string) *k8smocks.MockKubernetesClient {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus(workspaceID, podIP, string(v1.WorkspacePhaseActive), workspaceID)
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset).Maybe()
	return k8sMock
}

func setupPasswordSecret(t *testing.T, handler *ProxyHandler, workspaceID, password string) {
	t.Helper()
	secret := makePasswordSecret(workspaceID, password)
	_, err := handler.k8sClient.Clientset().CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)
}

// TestFlushAndAbortAfterIdle_SingleMessage verifies that flushAndAbortAfterIdle
// sends one message to opencode after the session goes idle and then issues
// a second abort.
func TestFlushAndAbortAfterIdle_SingleMessage(t *testing.T) {
	var receivedPaths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPaths = append(receivedPaths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "10.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	tracker := ssetracker.NewTracker(httpClient, &testLogger{}, func(workspaceID, sessionID string) {
		handler.onSessionIdle(workspaceID, sessionID)
	})
	handler.sseTracker = tracker

	sseIdle := func(sessionID string) {
		props, _ := json.Marshal(map[string]interface{}{
			"sessionID": sessionID,
			"status":    map[string]string{"type": "idle"},
		})
		tracker.DispatchProperties("ws-1", "session.status", props)
	}

	msg := msgqueue.QueuedMessage{ID: "msg_test_1", Text: "hello", SessionID: "ses-1", WorkspaceID: "ws-1"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.flushAndAbortAfterIdle("ws-1", "ses-1", []msgqueue.QueuedMessage{msg})
	}()

	// Give goroutine time to subscribe, then fire idle.
	time.Sleep(20 * time.Millisecond)
	sseIdle("ses-1")

	// After send, the session becomes busy again; fire another idle to complete the flow.
	time.Sleep(20 * time.Millisecond)
	sseIdle("ses-1")

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAndAbortAfterIdle did not complete")
	}

	// Verify: prompt_async was called once, abort was called once.
	assert.Contains(t, receivedPaths, "/session/ses-1/prompt_async", "should have sent message to opencode")
	assert.Contains(t, receivedPaths, "/session/ses-1/abort", "should have issued second abort")
}

// TestFlushAndAbortAfterIdle_MultipleMessages verifies that flushAndAbortAfterIdle
// sends each message one at a time, waiting for idle between each, so that
// no 409 "session busy" errors occur.
func TestFlushAndAbortAfterIdle_MultipleMessages(t *testing.T) {
	var mu sync.Mutex
	var receivedPaths []string

	// Simulate: prompt_async succeeds, then session becomes busy (opencode processes it).
	// The test manually fires idle between messages.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPaths = append(receivedPaths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "10.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	tracker := ssetracker.NewTracker(httpClient, &testLogger{}, func(workspaceID, sessionID string) {
		handler.onSessionIdle(workspaceID, sessionID)
	})
	handler.sseTracker = tracker

	sseIdle := func(sessionID string) {
		props, _ := json.Marshal(map[string]interface{}{
			"sessionID": sessionID,
			"status":    map[string]string{"type": "idle"},
		})
		tracker.DispatchProperties("ws-1", "session.status", props)
	}

	msgs := []msgqueue.QueuedMessage{
		{ID: "msg_a", Text: "first", SessionID: "ses-1", WorkspaceID: "ws-1"},
		{ID: "msg_b", Text: "second", SessionID: "ses-1", WorkspaceID: "ws-1"},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.flushAndAbortAfterIdle("ws-1", "ses-1", msgs)
	}()

	// Initial idle after abort → first message sent.
	time.Sleep(20 * time.Millisecond)
	sseIdle("ses-1")

	// Idle between msg1 and msg2 → second message sent.
	time.Sleep(50 * time.Millisecond)
	sseIdle("ses-1")

	// Final idle after second abort (not strictly needed for completion).
	time.Sleep(30 * time.Millisecond)
	sseIdle("ses-1")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushAndAbortAfterIdle did not complete")
	}

	mu.Lock()
	paths := make([]string, len(receivedPaths))
	copy(paths, receivedPaths)
	mu.Unlock()

	promptCount := 0
	abortCount := 0
	for _, p := range paths {
		switch p {
		case "/session/ses-1/prompt_async":
			promptCount++
		case "/session/ses-1/abort":
			abortCount++
		}
	}
	assert.Equal(t, 2, promptCount, "both messages should be sent to opencode")
	assert.Equal(t, 1, abortCount, "exactly one second abort should be issued")
}

// TestAbortSession_FailurePreservesQueue verifies that if the abort proxy
// returns an error (>=400), the queue is NOT cleared and no dismissed SSE is published.
func TestAbortSession_FailurePreservesQueue(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()

	ctx := context.Background()
	_, _ = svc.Enqueue(ctx, "ws-1", "ses-1", "should survive abort failure")

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/:id/sessions/:sessionId/abort", handler.AbortSession)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	// Queue must be untouched — abort failed.
	n, _ := svc.Len(ctx, "ws-1", "ses-1")
	assert.Equal(t, int64(1), n, "queue should be preserved when abort fails")

	// No dismissed SSE should have been published.
	select {
	case evt := <-sub.Ch:
		if evt.Type == "queue.update" {
			data, ok := evt.Data.(queueUpdateData)
			if ok && data.Event == "dismissed" {
				t.Fatalf("should not publish dismissed when abort fails, got: %+v", data)
			}
		}
	default:
		// Good — no dismissed events.
	}
}

// TestBulkReloadHandler_ClearQueueOnDispose verifies that
// BulkReloadHandler.clearQueueOnDispose publishes dismissed SSE and clears the queue.
func TestBulkReloadHandler_ClearQueueOnDispose(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	broker := eventbroker.NewUserEventBroker()

	h := &BulkReloadHandler{
		logger: &testLogger{},
	}
	h.SetQueueClearer(svc)
	h.SetBrokerPublisher(broker)

	ctx := context.Background()
	id1, err := svc.Enqueue(ctx, "ws-bulk", "ses-X", "bulk msg 1")
	require.NoError(t, err)
	id2, err := svc.Enqueue(ctx, "ws-bulk", "ses-Y", "bulk msg 2")
	require.NoError(t, err)

	sub, _ := broker.SubscribeWorkspace("ws-bulk")
	defer broker.UnsubscribeWorkspace("ws-bulk", sub)

	h.clearQueueOnDispose(ctx, "ws-bulk")

	dismissed := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(dismissed) < 2 {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				continue
			}
			data, ok := evt.Data.(queueUpdateData)
			if ok && data.Event == "dismissed" {
				dismissed[data.MessageID] = true
			}
		case <-deadline:
			t.Fatalf("timed out; got: %v", dismissed)
		}
	}
	assert.True(t, dismissed[id1])
	assert.True(t, dismissed[id2])

	n1, _ := svc.Len(ctx, "ws-bulk", "ses-X")
	n2, _ := svc.Len(ctx, "ws-bulk", "ses-Y")
	assert.Equal(t, int64(0), n1)
	assert.Equal(t, int64(0), n2)
}

// TestSendPromptAsync_RedirectsToQueueWhenNonEmpty closes the residual
// race window left by the frontend fix (PR #563). When the client's view
// of the queue is stale, a direct POST /prompt can still race ahead of
// the server-side drain goroutine — opencode assigns the direct send an
// earlier info.time.created than the still-draining queued message, so
// on next reload selectChronological places the queued message AFTER the
// direct send, breaking FIFO order.
//
// The fix: SendPromptAsync checks queueSvc.Len() and redirects to Enqueue
// when non-empty. The redirected request is enqueued behind the existing
// pending message, preserving FIFO order. This test also asserts the
// actual FIFO invariant — the pre-existing message A reaches prompt_async
// BEFORE the redirected message B.
func TestSendPromptAsync_RedirectsToQueueWhenNonEmpty(t *testing.T) {
	// Capture prompt_async bodies in order — proves FIFO ordering.
	var promptMu sync.Mutex
	var promptTexts []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			bodyBytes, _ := io.ReadAll(r.Body)
			var body struct {
				Parts []struct{ Text string } `json:"parts"`
			}
			_ = json.Unmarshal(bodyBytes, &body)
			if len(body.Parts) > 0 {
				promptMu.Lock()
				promptTexts = append(promptTexts, body.Parts[0].Text)
				promptMu.Unlock()
			}
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Mark session as active so the drain-on-enqueue goroutine does NOT
	// fire when we pre-populate the queue. Without this the drain would
	// race our test's SendPromptAsync call.
	handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	// Pre-populate the queue with one pending message (msg A).
	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "message A")
	require.NoError(t, err)
	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	require.Equal(t, int64(1), n, "precondition: queue must have 1 pending message")

	// Now mark session as idle so SendPromptAsync doesn't 409. The drain
	// goroutine will start when Enqueue/redirect fires (matches prod).
	handler.SetActiveSessionsForTest("ws-1", []string{})
	require.False(t, handler.isSessionActive(context.Background(), "ws-1", "ses-1"),
		"precondition: session must be idle")

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST",
		"/api/v1/workspaces/ws-1/sessions/ses-1/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"message B"}]}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.SendPromptAsync(c)

	// Expect: 202 Accepted with a messageID — NOT 200 from proxyToWorkspace.
	assert.Equal(t, http.StatusAccepted, w.Code,
		"queue non-empty + idle session → SendPromptAsync should redirect to Enqueue (202)")
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.NotEmpty(t, resp["messageID"], "redirect response should include a messageID")

	// FIFO assertion: drain should forward A first, then B. Wait for both.
	deadline := time.Now().Add(3 * time.Second)
	for {
		promptMu.Lock()
		count := len(promptTexts)
		promptMu.Unlock()
		if count >= 2 {
			break
		}
		if time.Now().After(deadline) {
			promptMu.Lock()
			t.Fatalf("drain did not forward both messages within deadline; got %d/%d: %v",
				count, 2, promptTexts)
			promptMu.Unlock()
		}
		time.Sleep(20 * time.Millisecond)
	}

	promptMu.Lock()
	defer promptMu.Unlock()
	assert.Equal(t, []string{"message A", "message B"}, promptTexts,
		"FIFO order: pre-existing message A must reach prompt_async BEFORE redirected message B")
}

// TestSendPromptAsync_ProceedsWhenQueueEmpty verifies that the redirect
// guard doesn't false-positive when the queue is empty — the prompt
// should be forwarded to opencode as before.
func TestSendPromptAsync_ProceedsWhenQueueEmpty(t *testing.T) {
	promptCalled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			select {
			case promptCalled <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
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
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	// Session is idle (not in activeSess) AND queue is empty.
	require.False(t, handler.isSessionActive(context.Background(), "ws-1", "ses-1"))
	n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
	require.Equal(t, int64(0), n)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST",
		"/api/v1/workspaces/ws-1/sessions/ses-1/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.SendPromptAsync(c)

	// Empty queue → forward to opencode (200), not redirect (202).
	assert.Equal(t, http.StatusOK, w.Code,
		"empty queue + idle session → SendPromptAsync should proceed as before")
	select {
	case <-promptCalled:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("prompt_async was not called — SendPromptAsync should forward when queue is empty")
	}
}

// TestSendPromptAsync_RedirectRejectsInvalidBody covers the unhappy
// paths in redirectPromptToQueue: malformed JSON, empty parts, tool-only
// parts (→ empty extracted text), and oversized text. All should return
// 400 without enqueuing anything.
func TestSendPromptAsync_RedirectRejectsInvalidBody(t *testing.T) {
	type testCase struct {
		name    string
		body    string
		wantErr string // substring of the error message
	}
	cases := []testCase{
		{
			name:    "malformed JSON",
			body:    `{"parts":[{"type":"text","text":broken`,
			wantErr: "invalid request body",
		},
		{
			name:    "empty parts array",
			body:    `{"parts":[]}`,
			wantErr: "text must not be empty",
		},
		{
			name:    "tool-only parts filtered to empty text",
			body:    `{"parts":[{"type":"tool","tool":"bash"}]}`,
			wantErr: "text must not be empty",
		},
		{
			name:    "text part with empty string",
			body:    `{"parts":[{"type":"text","text":""}]}`,
			wantErr: "text must not be empty",
		},
		{
			name:    "text exceeds 100KB",
			body:    `{"parts":[{"type":"text","text":"` + strings.Repeat("a", 100_001) + `"}]}`,
			wantErr: "text exceeds 100KB limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, svc, cleanup := setupQueueTestEnv(t)
			defer cleanup()

			// Pre-populate queue with 1 message so the redirect path is taken.
			handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})
			_, err := svc.Enqueue(context.Background(), "ws-1", "ses-1", "pre-existing")
			require.NoError(t, err)
			// Mark idle so SendPromptAsync doesn't 409.
			handler.SetActiveSessionsForTest("ws-1", []string{})

			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/prompt", strings.NewReader(tc.body))
			c.Params = gin.Params{
				{Key: "id", Value: "ws-1"},
				{Key: "sessionId", Value: "ses-1"},
			}

			handler.SendPromptAsync(c)

			assert.Equal(t, http.StatusBadRequest, w.Code,
				"%s: expected 400 for invalid body", tc.name)
			assert.Contains(t, w.Body.String(), tc.wantErr,
				"%s: error message mismatch", tc.name)

			// The pre-existing message must remain in the queue — the
			// rejected body must not have side-effects on the queue.
			n, _ := svc.Len(context.Background(), "ws-1", "ses-1")
			assert.Equal(t, int64(1), n,
				"%s: queue state must be unchanged after 400", tc.name)
		})
	}
}

// TestSendPromptAsync_FailOpenWhenLenErrors verifies that when the
// queueSvc.Len() probe fails (Redis transient), SendPromptAsync proceeds
// with a direct send rather than rejecting the request. This is the
// documented fail-open policy — rejecting legitimate traffic for a
// queue-length probe failure would be worse than a possible FIFO miss.
func TestSendPromptAsync_FailOpenWhenLenErrors(t *testing.T) {
	promptCalled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses-1/prompt_async" {
			select {
			case promptCalled <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "10.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	// Use a closed Redis client to force Len() to error.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")
	// Close miniredis BEFORE the test runs so Len() fails.
	mr.Close()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST",
		"/api/v1/workspaces/ws-1/sessions/ses-1/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.SendPromptAsync(c)

	// Fail-open: should return 200 (direct send), NOT 500 or 503.
	assert.Equal(t, http.StatusOK, w.Code,
		"Len() error → fail-open → direct send (200), not 5xx")
	select {
	case <-promptCalled:
		// ok — direct send happened despite Len() failure
	case <-time.After(2 * time.Second):
		t.Fatal("fail-open: prompt_async was not called — direct send should proceed on Len() error")
	}
}

// TestExtractPromptText unit-tests the body parser directly. Covers
// single text part, multiple text parts (concatenation), tool-only
// parts (dropped), empty parts, and malformed JSON.
func TestExtractPromptText(t *testing.T) {
	type testCase struct {
		name    string
		body    string
		want    string
		wantErr bool
	}
	cases := []testCase{
		{
			name: "single text part",
			body: `{"parts":[{"type":"text","text":"hello"}]}`,
			want: "hello",
		},
		{
			name: "multiple text parts are concatenated",
			body: `{"parts":[{"type":"text","text":"abc"},{"type":"text","text":"def"}]}`,
			want: "abcdef",
		},
		{
			name: "tool-only parts dropped (returns empty)",
			body: `{"parts":[{"type":"tool","tool":"bash","state":{"title":"run tests"}}]}`,
			want: "",
		},
		{
			name: "mixed text and tool parts (only text kept)",
			body: `{"parts":[{"type":"text","text":"keep"},{"type":"tool","tool":"x"},{"type":"text","text":"this"}]}`,
			want: "keepthis",
		},
		{
			name: "empty parts array",
			body: `{"parts":[]}`,
			want: "",
		},
		{
			name: "missing parts field",
			body: `{}`,
			want: "",
		},
		{
			name:    "malformed JSON returns error",
			body:    `{"parts":[{"type":"text","text":broken`,
			wantErr: true,
		},
		{
			name:    "valid JSON but wrong shape (parts is string)",
			body:    `{"parts":"not an array"}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractPromptText([]byte(tc.body))
			if tc.wantErr {
				assert.Error(t, err, "%s: expected error", tc.name)
				return
			}
			require.NoError(t, err, "%s: unexpected error", tc.name)
			assert.Equal(t, tc.want, got, "%s: text mismatch", tc.name)
		})
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// startV2TestServer starts an httptest.Server on a DYNAMIC port that routes
// by path: /prompt → 200 with admission body, /interrupt → 204. Enforces
// Basic auth. Dynamic port eliminates the port 4096 contention that caused
// non-deterministic CI failures.
func startV2TestServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses-1"}}`))
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newV2TestHandler creates a ProxyHandler with V2 flag on, backed by the
// given test server. The v2ClientFactory is injected so V2 methods talk to
// the test server's dynamic port, not port 4096. Routes are registered on a
// real gin router so gin's response lifecycle (WriteHeaderNow flush) runs
// correctly — calling handler methods directly with gin.CreateTestContext
// skips the flush, causing bare c.Status(204) to never reach the recorder.
func newV2TestHandler(t *testing.T, srv *httptest.Server) (*gin.Engine, *ProxyHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := newMockK8sWithWorkspace(t, "ws-1", "127.0.0.1")
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetV2SessionQueueEnabled(true)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.SetV2ClientFactory(func(ctx context.Context, workspaceID string) (V2SessionClient, error) {
		return opencode.NewClient(srv.URL, "test-pw", nil), nil
	})

	router := gin.New()
	router.POST("/:id/sessions/:sessionId/queue", handler.EnqueueMessage)
	router.POST("/:id/sessions/:sessionId/prompt_async", handler.SendPromptAsync)
	router.POST("/:id/sessions/:sessionId/abort", handler.AbortSession)
	return router, handler
}

// --- EnqueueMessage V2 ---

func TestEnqueueV2_Success(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello v2"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "msg_v2_1", resp["messageID"])
}

func TestEnqueueV2_EmptyText(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":""}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEnqueueV2_ServerError(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	srv.Close() // dead server → connection refused

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestEnqueueV2_FlagOff_FallsThroughToV1(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, handler := newV2TestHandler(t, srv)
	handler.SetV2SessionQueueEnabled(false) // flag OFF

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"flag off → V1 path → 503 (no queueSvc)")
}

func TestEnqueueV2_SessionNotFound404(t *testing.T) {
	// Server returns 404 → enqueueV2 must map ErrV2SessionNotFound → 404.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"type":"SessionNotFound","message":"session does not exist"}}`))
	}))
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/bogus/queue",
		strings.NewReader(`{"text":"hello"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code,
		"V2 enqueue must map ErrV2SessionNotFound → 404")
}

// --- SendPromptAsync V2 ---

func TestSendPromptAsyncV2_Bypasses409Guard(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, handler := newV2TestHandler(t, srv)
	handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/prompt_async",
		strings.NewReader(`{"parts":[{"type":"text","text":"busy-send"}]}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code, "V2 must bypass the 409 guard")
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "msg_v2_1", resp["messageID"])
}

func TestSendPromptAsyncV2_InvalidBody(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/prompt_async",
		strings.NewReader(`not json`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- AbortSession V2 ---

func TestAbortV2_Success(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestAbortV2_QueueSurvivesAbort(t *testing.T) {
	// The defining difference from V1: under V2, queued messages SURVIVE
	// an abort. V1's AbortSession clears the Redis queue (PeekAll+Clear)
	// and emits "dismissed" for each. V2's InterruptV2 does neither.
	//
	// This test sets up a real Redis-backed queue with messages, aborts
	// via V2, and asserts the queue is untouched — proving the V2 path
	// was taken AND that it is non-destructive (F8).
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, handler := newV2TestHandler(t, srv)

	// Set up a real Redis-backed queue with 2 messages.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()
	queueSvc := msgqueue.NewWithClient(redisClient)
	handler.SetMessageQueueService(queueSvc)

	// Enqueue 2 messages into the Redis queue.
	_, err = queueSvc.Enqueue(context.Background(), "ws-1", "ses-1", "msg-a")
	require.NoError(t, err)
	_, err = queueSvc.Enqueue(context.Background(), "ws-1", "ses-1", "msg-b")
	require.NoError(t, err)

	// Verify they're there.
	n, _ := queueSvc.Len(context.Background(), "ws-1", "ses-1")
	require.Equal(t, int64(2), n, "precondition: 2 messages queued")

	// Abort via V2 (routes through gin → AbortSession → abortV2 → InterruptV2).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)

	// THE ASSERTION: the queue must still have 2 messages. V1 would have
	// cleared it. V2 is non-destructive (F8).
	n, _ = queueSvc.Len(context.Background(), "ws-1", "ses-1")
	assert.Equal(t, int64(2), n,
		"V2 abort is non-destructive: queued messages must survive (F8)")
}

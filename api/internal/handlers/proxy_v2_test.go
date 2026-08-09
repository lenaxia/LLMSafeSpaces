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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
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

func TestAbortV2_NoQueueMutation(t *testing.T) {
	// Under V2, abort does NOT touch Redis. queueSvc is nil (no
	// SetMessageQueueService). If V2 path is taken, InterruptV2 runs (204)
	// and we return. If V1 path were taken, proxyToWorkspace would run,
	// then queueSvc.PeekAll → nil pointer panic. No panic + 204 = V2 path.
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	router, _ := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/abort", nil)

	assert.NotPanics(t, func() {
		router.ServeHTTP(w, req)
	})
	assert.Equal(t, http.StatusNoContent, w.Code,
		"V2 abort must succeed without touching queueSvc (nil)")
}

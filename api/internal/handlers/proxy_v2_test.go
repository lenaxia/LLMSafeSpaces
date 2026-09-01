// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
			// Defense-in-depth (#707): include the real opencode 1.18.10
			// timeCreated-as-number shape in the canned body so any future
			// re-introduction of a typed V2PromptResponse.TimeCreated field
			// fails at the integration layer too, not just at the unit layer.
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses-1","timeCreated":1786316936471}}`))
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

	// US-63.5/US-69.11: enqueueV2 emits no SSE itself — the enqueued pill
	// derives from the outbox staging event, and stranded-input recovery
	// is owned by the outbox ledger (outbox_terminus_test.go).
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

// ---------------------------------------------------------------------------
// US-63.5: SSE Event Bridge tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// US-63.9: Stranded-Input Recovery tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// US-69.11: reconcileSessionState's surviving duties — stale activeSess
// clearing via statusz (the OOM/SIGTERM phantom-busy fix) and the
// session.status=idle re-publish. The V2 wake machinery was deleted with
// v2Pending (the outbox ledger owns stranded-input recovery).
// ---------------------------------------------------------------------------

// TestReconcileSessionState_ClearsStaleActiveSess: a session idle in the
// agent but busy in our local map is cleared and the idle transition is
// re-published to the workspace stream.
func TestReconcileSessionState_ClearsStaleActiveSess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[{"id":"ses-stale","status":"idle"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvAddr := srv.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{eventHost: srvAddr, promptHost: srvAddr},
		Timeout:   5 * time.Second,
	}
	k8sMock := newMockK8sWithWorkspace(t, "ws-1", srvAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(stubUsageStream())

	sub, err := handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	handler.SetActiveSessionsForTest("ws-1", []string{"ses-stale"})

	// podIP must be a bare host — reconcile formats "http://%s:%d".
	host, _, err := net.SplitHostPort(srvAddr)
	require.NoError(t, err)
	handler.reconcileSessionState("ws-1", host, "test-pw")

	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"),
		"session idle in opencode must be cleared from the local active map")
	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "ses-stale", evt.SessionID)
		assert.Equal(t, "idle", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile must re-publish session.status=idle so open UIs clear their busy indicator")
	}
}

// TestReconcileSessionState_LargeStatuszDecodes (#892 D2 regression):
// statusz embeds one entry per session in opencode's DB. The old
// 16 KB io.LimitReader overflowed at >~55 sessions, silently no-op'ing
// the reconcile — stale activeSess entries persisted as phantom-busy.
func TestReconcileSessionState_LargeStatuszDecodes(t *testing.T) {
	// Build a statusz body comfortably over 16 KB (~120 sessions with
	// realistic per-session metadata) but well under the 1 MB cap.
	var sb strings.Builder
	sb.WriteString(`{"sessions":[`)
	for i := 0; i < 120; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb,
			`{"id":"ses-%03d-paddingpaddingpaddingpaddingpadding","title":"session %d with a realistic length title","status":"idle","model":"glm-5.3","contextUsed":%d}`,
			i, i, 100000+i*7)
	}
	sb.WriteString(`]}`)
	body := sb.String()
	require.Greater(t, len(body), 16*1024, "fixture must overflow the old cap")
	require.Less(t, len(body), 1<<20, "fixture must fit the new cap")

	target := "ses-000-paddingpaddingpaddingpaddingpadding"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvAddr := srv.Listener.Addr().String()
	httpClient := &http.Client{
		Transport: &routingTransport{eventHost: srvAddr, promptHost: srvAddr},
		Timeout:   5 * time.Second,
	}
	k8sMock := newMockK8sWithWorkspace(t, "ws-1", srvAddr)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(stubUsageStream())

	handler.SetActiveSessionsForTest("ws-1", []string{target})

	host, _, err := net.SplitHostPort(srvAddr)
	require.NoError(t, err)
	handler.reconcileSessionState("ws-1", host, "test-pw")

	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"),
		"a >16 KB statusz must still decode and clear the stale entry — pre-fix this silently no-op'd")
}

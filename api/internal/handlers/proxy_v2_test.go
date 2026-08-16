// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
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

	router, handler := newV2TestHandler(t, srv)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ws-1/sessions/ses-1/queue",
		strings.NewReader(`{"text":"hello v2"}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "msg_v2_1", resp["messageID"])

	// US-63.5: enqueueV2 no longer emits enqueued SSE itself — it's derived
	// from the V2 PromptAdmitted event. The session IS tracked for US-63.9.
	assert.True(t, handler.v2Pending.has("ws-1", "ses-1"),
		"enqueueV2 must track the session for US-63.9 stranded-input recovery")
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

func TestV2SSEBridge_AdmittedToEnqueued(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	_, handler := newV2TestHandler(t, srv)

	// Subscribe to the broker to capture events.
	sub, err := handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	admittedEvent := `{"id":"evt_1","type":"session.next.prompt.admitted","properties":{"messageID":"msg_abc","sessionID":"ses-1","timestamp":"2026-08-09T16:11:17.289Z","prompt":{"text":"hi"},"delivery":"queue"}}`
	handler.onRawEvent("ws-1", "session.next.prompt.admitted", admittedEvent)

	// onRawEvent fires opencode.event (raw relay) THEN queue.update (V2 bridge).
	// Drain until we find the queue.update.
	var queueEvent *apitypes.WorkspaceSSEEvent
drainAdmitted:
	for {
		select {
		case e := <-sub.Ch:
			if e.Type == "queue.update" {
				queueEvent = &e
				break drainAdmitted
			}
		case <-time.After(time.Second):
			t.Fatal("timeout: no queue.update event received")
		}
	}
	require.NotNil(t, queueEvent)
	data, _ := json.Marshal(queueEvent.Data)
	assert.Contains(t, string(data), "enqueued")
	assert.Contains(t, string(data), "msg_abc")
	// Note: v2Pending tracking is done in enqueueV2, not in bridgeV2Admitted
	// (would double-count). The SSE bridge only synthesizes the event.
}

func TestV2SSEBridge_PromptedToSent(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	_, handler := newV2TestHandler(t, srv)

	sub, err := handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	handler.onRawEvent("ws-1", "session.next.prompt.admitted",
		`{"id":"e1","type":"session.next.prompt.admitted","properties":{"messageID":"msg_x","sessionID":"ses-1","delivery":"queue"}}`)
	handler.onRawEvent("ws-1", "session.next.prompted",
		`{"id":"e2","type":"session.next.prompted","properties":{"messageID":"msg_x","sessionID":"ses-1","delivery":"queue"}}`)

	var types []string
drainLoop:
	for {
		select {
		case e := <-sub.Ch:
			if e.Type == "queue.update" {
				data, _ := json.Marshal(e.Data)
				types = append(types, string(data))
			}
		case <-time.After(200 * time.Millisecond):
			break drainLoop
		}
	}

	require.Len(t, types, 2, "should see enqueued + sent")
	assert.Contains(t, types[0], "enqueued")
	assert.Contains(t, types[1], "sent")
	assert.False(t, handler.v2Pending.has("ws-1", "ses-1"),
		"Prompted must clear session from pending tracking (drained)")
}

func TestV2SSEBridge_IgnoresSteerDelivery(t *testing.T) {
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	_, handler := newV2TestHandler(t, srv)

	sub, err := handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	handler.onRawEvent("ws-1", "session.next.prompt.admitted",
		`{"id":"e1","type":"session.next.prompt.admitted","properties":{"messageID":"msg_s","sessionID":"ses-1","delivery":"steer"}}`)

	// opencode.event relay fires, but no queue.update.
drainSteer:
	for {
		select {
		case e := <-sub.Ch:
			if e.Type == "queue.update" {
				t.Fatalf("steer delivery must not synthesize queue.update: %+v", e)
			}
		case <-time.After(100 * time.Millisecond):
			break drainSteer
		}
	}
	assert.False(t, handler.v2Pending.has("ws-1", "ses-1"),
		"steer delivery must not be tracked for US-63.9")
}

// ---------------------------------------------------------------------------
// US-63.9: Stranded-Input Recovery tests
// ---------------------------------------------------------------------------

func TestV2PendingSessions_TrackAndClear(t *testing.T) {
	v := newV2PendingSessions()

	v.add("ws-1", "ses-a")
	v.add("ws-1", "ses-b")
	assert.True(t, v.has("ws-1", "ses-a"))
	assert.True(t, v.has("ws-1", "ses-b"))
	assert.False(t, v.has("ws-1", "ses-c"))

	sessions := v.sessionsForWorkspace("ws-1")
	assert.Len(t, sessions, 2)

	v.remove("ws-1", "ses-a")
	assert.False(t, v.has("ws-1", "ses-a"))
	assert.True(t, v.has("ws-1", "ses-b"))

	v.remove("ws-1", "ses-b")
	assert.Empty(t, v.sessionsForWorkspace("ws-1"))
}

func TestV2PendingSessions_ReferenceCountedMultiInput(t *testing.T) {
	// Multiple queue deliveries to the same session must not clear tracking
	// when only one drains. The count must reach zero before removal.
	v := newV2PendingSessions()

	v.add("ws-1", "ses-a") // 1 pending
	v.add("ws-1", "ses-a") // 2 pending
	assert.True(t, v.has("ws-1", "ses-a"))

	v.remove("ws-1", "ses-a") // 1 remaining
	assert.True(t, v.has("ws-1", "ses-a"),
		"session with 1 remaining pending input must stay tracked")

	v.remove("ws-1", "ses-a") // 0 remaining
	assert.False(t, v.has("ws-1", "ses-a"),
		"session with 0 pending inputs must be removed from tracking")
}

func TestV2StrandedRecovery_WakesIdleSession(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":99,"id":"msg_wake","sessionID":"ses-stranded"}}`))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-stranded")

	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"stranded session must receive exactly one wake prompt")
}

func TestV2StrandedRecovery_NoWakeForUntrackedSession(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)

	assert.Equal(t, int32(0), atomic.LoadInt32(&wakeCount),
		"untracked sessions must not receive wake prompts")
}

func TestV2StrandedRecovery_WakeSendsNewlineWithDeliveryQueue(t *testing.T) {
	// Verify the wake prompt body: text="\n", delivery="queue".
	// This is the exact contract that triggers execution.wake.
	var bodyBytes []byte
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			bodyBytes, _ = io.ReadAll(r.Body)
			atomic.AddInt32(&wakeCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_w","sessionID":"ses-1"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-1")

	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)

	require.NotEmpty(t, bodyBytes, "wake prompt must send a body")
	var body struct {
		Prompt   struct{ Text string } `json:"prompt"`
		Delivery string                `json:"delivery"`
	}
	require.NoError(t, json.Unmarshal(bodyBytes, &body))
	assert.Equal(t, "\n", body.Prompt.Text, "wake text must be \\n (minimal non-empty)")
	assert.Equal(t, "queue", body.Delivery, "wake delivery must be queue")
}

func TestV2StrandedRecovery_IntegrationWithReconcile(t *testing.T) {
	// Real integration test through reconcileSessionState — the sole call
	// site that makes US-63.9 functional. Uses the existing test pattern
	// from proxy_queue_drain_miss_test.go: routingTransport routes the
	// statusz query to the test server; v2ClientFactory routes V2 calls.
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[{"id":"ses-stranded","status":"idle"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_w","sessionID":"ses-stranded"}}`))
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
	handler.SetV2ClientFactory(func(ctx context.Context, workspaceID string) (V2SessionClient, error) {
		return opencode.NewClient(srv.URL, "test-pw", nil), nil
	})

	// Simulate a stranded session: mark it as having pending V2 input.
	handler.v2Pending.add("ws-1", "ses-stranded")

	// Call the REAL reconcileSessionState — the full wiring path.
	// podIP must be a bare host — reconcile formats "http://%s:%d". Passing
	// srvAddr (host:port) builds a double-port URL that Go 1.26's stricter
	// net/url rejects (1.25 parsed it leniently; the routing transport's
	// host rewrite masked the malformation).
	host, _, err := net.SplitHostPort(srvAddr)
	require.NoError(t, err)
	handler.reconcileSessionState("ws-1", host, "test-pw")

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"reconcileSessionState must wake stranded session with pending V2 input")
}

func TestV2Lifecycle_NoDoubleCounting(t *testing.T) {
	// Combined lifecycle: enqueue (tracks) → V2 PromptAdmitted (SSE only,
	// does NOT re-track) → V2 Prompted (untracks). Count must be 0 at end.
	// This catches the double-counting bug the reviewer flagged: if
	// bridgeV2Admitted also called v2Pending.add, the count would be 2
	// after enqueue+admit and 1 after prompted — permanently stuck.
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()
	_, handler := newV2TestHandler(t, srv)

	// Step 1: enqueueV2 tracks the session (count = 1).
	handler.v2Pending.add("ws-1", "ses-1")
	assert.True(t, handler.v2Pending.has("ws-1", "ses-1"))

	// Step 2: V2 PromptAdmitted event fires (SSE bridge synthesizes
	// enqueued, but does NOT re-track).
	handler.onRawEvent("ws-1", "session.next.prompt.admitted",
		`{"id":"e1","type":"session.next.prompt.admitted","properties":{"messageID":"msg_x","sessionID":"ses-1","delivery":"queue"}}`)
	assert.True(t, handler.v2Pending.has("ws-1", "ses-1"),
		"after admit: session must still be tracked (count=1, not 2)")

	// Step 3: V2 Prompted event fires (SSE bridge synthesizes sent,
	// untracks).
	handler.onRawEvent("ws-1", "session.next.prompted",
		`{"id":"e2","type":"session.next.prompted","properties":{"messageID":"msg_x","sessionID":"ses-1","delivery":"queue"}}`)
	assert.False(t, handler.v2Pending.has("ws-1", "ses-1"),
		"after prompted: session must be untracked (count=0)")
}

func TestV2StrandedRecovery_WakeErrorDoesNotPanic(t *testing.T) {
	// Wake error path: client construction fails (bad pod IP) → logged,
	// no panic, remaining sessions still attempted.
	srv := startV2TestServer(t, "test-pw")
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)

	// Override factory to return an error for the first session,
	// succeed for the second.
	callCount := int32(0)
	handler.SetV2ClientFactory(func(ctx context.Context, workspaceID string) (V2SessionClient, error) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			return nil, errors.New("simulated client construction failure")
		}
		return opencode.NewClient(srv.URL, "test-pw", nil), nil
	})

	handler.v2Pending.add("ws-1", "ses-fail")
	handler.v2Pending.add("ws-1", "ses-ok")

	assert.NotPanics(t, func() {
		handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)
	})

	assert.Equal(t, int32(2), atomic.LoadInt32(&callCount),
		"both sessions must be attempted despite the first failing")
}

func TestV2StrandedRecovery_PromptV2FailureContinuesToNextSession(t *testing.T) {
	// Distinct from the construction-failure test: here the client
	// constructs successfully but PromptV2 itself fails (e.g. pod
	// restarted between construction and the HTTP call). The wake
	// loop must continue to the next session.
	var promptCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&promptCalls, 1)
		// Always return 500 — PromptV2 fails, but the loop should continue.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-a")
	handler.v2Pending.add("ws-1", "ses-b")

	assert.NotPanics(t, func() {
		handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)
	})

	assert.Equal(t, int32(2), atomic.LoadInt32(&promptCalls),
		"both sessions must be attempted despite PromptV2 returning 500")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// TestReconcileSessionState_LargeStatuszDecodes (#892 D2 regression):
// statusz embeds one entry per session in opencode's DB. The old
// 16 KB io.LimitReader overflowed at >~55 sessions, silently no-op'ing
// the reconcile — stale activeSess entries persisted as client-side
// phantom-busy. Mirrors the #801 fix shape (proxy_connections.go).
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

	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_w","sessionID":"ses-000-paddingpaddingpaddingpaddingpadding"}}`))
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
	handler.SetV2ClientFactory(func(ctx context.Context, workspaceID string) (V2SessionClient, error) {
		return opencode.NewClient(srv.URL, "test-pw", nil), nil
	})

	handler.v2Pending.add("ws-1", "ses-000-paddingpaddingpaddingpaddingpadding")

	host, _, err := net.SplitHostPort(srvAddr)
	require.NoError(t, err)
	handler.reconcileSessionState("ws-1", host, "test-pw")

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"a >16 KB statusz must still decode and wake the stranded session — pre-fix this silently no-op'd")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// --- #744 Finding 1: busy-session guard ---

func TestV2StrandedRecovery_SkipsBusySession(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-idle")
	handler.v2Pending.add("ws-1", "ses-busy")

	busySessions := map[string]bool{"ses-busy": true}
	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", busySessions)

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"only the idle session must be woken; busy session must be skipped")
}

func TestV2StrandedRecovery_NilBusySet_WakesAll(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-a")
	handler.v2Pending.add("ws-1", "ses-b")

	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)

	assert.Equal(t, int32(2), atomic.LoadInt32(&wakeCount),
		"nil busySet must wake all pending sessions (backward compat)")
}

// --- #744 Finding 2: TTL pruning ---

func TestV2PendingSessions_TTLPrunesStaleEntries(t *testing.T) {
	v := newV2PendingSessions()

	v.add("ws-1", "ses-fresh")
	v.add("ws-1", "ses-stale")

	// Manually age the stale entry past the TTL threshold.
	v.mu.Lock()
	e := v.data["ws-1"]["ses-stale"]
	e.lastAdded = time.Now().Add(-v2PendingTTL - time.Minute)
	v.data["ws-1"]["ses-stale"] = e
	v.mu.Unlock()

	// Trigger a read which prunes.
	sessions := v.sessionsForWorkspace("ws-1")

	assert.Len(t, sessions, 1, "stale entry must be pruned, fresh entry retained")
	assert.Contains(t, sessions, "ses-fresh")
	assert.False(t, v.has("ws-1", "ses-stale"), "stale entry must not be visible after prune")
	assert.True(t, v.has("ws-1", "ses-fresh"), "fresh entry must survive")
}

func TestV2PendingSessions_TTLPrunesOnRemove(t *testing.T) {
	v := newV2PendingSessions()

	v.add("ws-1", "ses-stale")
	v.add("ws-1", "ses-fresh")

	// Age the stale entry.
	v.mu.Lock()
	e := v.data["ws-1"]["ses-stale"]
	e.lastAdded = time.Now().Add(-v2PendingTTL - time.Minute)
	v.data["ws-1"]["ses-stale"] = e
	v.mu.Unlock()

	v.remove("ws-1", "ses-fresh")

	assert.False(t, v.has("ws-1", "ses-stale"), "stale entry must be pruned during remove")
	assert.False(t, v.has("ws-1", "ses-fresh"), "fresh entry removed normally")
}

func TestV2PendingSessions_TTLFreshEntrySurvives(t *testing.T) {
	v := newV2PendingSessions()
	v.add("ws-1", "ses-recent")

	// Entry is fresh (just added) — should survive reads.
	assert.True(t, v.has("ws-1", "ses-recent"))
	sessions := v.sessionsForWorkspace("ws-1")
	assert.Contains(t, sessions, "ses-recent")
}

// --- #744 Finding 3: nil-guard symmetry ---

func TestEnqueueV2_NilV2Pending_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_1","sessionID":"ses_1"}}`))
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending = nil

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{"prompt":{"parts":[{"type":"text","text":"hi"}]}}`))

	assert.NotPanics(t, func() {
		handler.enqueueV2(c, "ws-1", "ses-1", "hi")
	})
	assert.Equal(t, http.StatusAccepted, w.Code,
		"enqueueV2 must succeed even when v2Pending is nil")
}

// --- Busy-guard integration through reconcileSessionState ---

func TestReconcileSessionState_BusySessionNotWoken(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[{"id":"ses-busy","status":"busy"},{"id":"ses-idle","status":"idle"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_w","sessionID":"ses-w"}}`))
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
	assert.NoError(t, err)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.SetV2ClientFactory(func(ctx context.Context, workspaceID string) (V2SessionClient, error) {
		return opencode.NewClient(srv.URL, "test-pw", nil), nil
	})

	handler.v2Pending.add("ws-1", "ses-busy")
	handler.v2Pending.add("ws-1", "ses-idle")

	// podIP must be a bare host — reconcile formats "http://%s:%d". Passing
	// srvAddr (host:port) builds a double-port URL that Go 1.26's stricter
	// net/url rejects (1.25 parsed it leniently; the routing transport's
	// host rewrite masked the malformation).
	host, _, _ := net.SplitHostPort(srvAddr)
	handler.reconcileSessionState("ws-1", host, "test-pw")

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"only the idle session must be woken; busy session must be skipped by the guard")
}

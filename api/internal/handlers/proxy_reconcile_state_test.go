// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
)

// Session-state reconciliation tests (statusz-driven) — moved out of the
// deleted proxy_v2_test.go (#828 batch 2): they cover reconcileSessionState,
// not the V2 queue paths that died with enqueueV2/abortV2.

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

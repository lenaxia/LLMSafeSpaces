// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// newStatuszTestEnv creates a test env whose handler.httpClient routes
// statusz requests to the given test server. PodIP is set to the server's
// listen address; httpClient uses a custom transport that rewrites the
// host portion to point at the test server regardless of the URL's port.
func newStatuszTestEnv(t *testing.T, statuszHandler http.HandlerFunc) (*testEnv, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(statuszHandler)
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Custom transport routes all requests to our test server.
	env.handler.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &rewritingTransport{target: srv.URL},
	}
	env.wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			PodIP: "10.0.0.1",
		},
	}, nil).Maybe()
	env.handler.SetCachedPasswordForTest("ws-1", "test-password")
	return env, srv
}

type rewritingTransport struct{ target string }

func (t *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Scheme = "http"
	cloned.URL.Host = strings.TrimPrefix(t.target, "http://")
	return http.DefaultTransport.RoundTrip(cloned)
}

// TestGetAuthoritativeActiveSessions_QueriesStatusz is the primary
// regression test for #792 Pattern 1 (stuck-busy root cause). The
// session list endpoint must query the workspace pod's /v1/statusz
// for ground-truth busy/idle status, not rely on the in-memory
// activeSess map which goes stale when SSE events are missed.
func TestGetAuthoritativeActiveSessions_QueriesStatusz(t *testing.T) {
	gin.SetMode(gin.TestMode)

	statuszCalled := int32(0)
	env, srv := newStatuszTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			atomic.StoreInt32(&statuszCalled, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sessions":[
				{"id":"ses_idle","status":"idle"},
				{"id":"ses_busy","status":"busy"}
			]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	activeSet := env.handler.GetAuthoritativeActiveSessions(context.Background(), "ws-1")

	assert.Equal(t, int32(1), atomic.LoadInt32(&statuszCalled),
		"must call /v1/statusz for ground truth")
	assert.True(t, activeSet["ses_busy"], "busy session must be in the set")
	assert.False(t, activeSet["ses_idle"], "idle session must NOT be in the set")
}

// TestGetAuthoritativeActiveSessions_ReconcilesStaleActiveSess verifies
// that when statusz reports a session as idle but the in-memory
// activeSess map has it as active (the stuck-busy bug), the stale entry
// is cleaned up during the statusz query.
func TestGetAuthoritativeActiveSessions_ReconcilesStaleActiveSess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env, srv := newStatuszTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"sessions":[
				{"id":"ses_stale","status":"idle"}
			]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	// Seed a stale active session — this is the stuck-busy state.
	env.handler.SetActiveSessionsForTest("ws-1", []string{"ses_stale"})
	assert.True(t, env.handler.isSessionActive(context.Background(), "ws-1", "ses_stale"),
		"precondition: session must be active in memory")

	activeSet := env.handler.GetAuthoritativeActiveSessions(context.Background(), "ws-1")

	assert.False(t, activeSet["ses_stale"], "stale session should not be active")
	assert.False(t, env.handler.isSessionActive(context.Background(), "ws-1", "ses_stale"),
		"stale activeSess entry must be removed after ground-truth reconciliation")
}

// TestGetAuthoritativeActiveSessions_FallbackOnNotReady verifies that
// when the workspace is not Active (pod restarting, suspended), the
// method falls back to the in-memory activeSess map rather than failing.
func TestGetAuthoritativeActiveSessions_FallbackOnNotReady(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.wsMock.On("Get", mock.Anything, "ws-1", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)
	env.handler.SetCachedPasswordForTest("ws-1", "test-password")
	env.handler.SetActiveSessionsForTest("ws-1", []string{"ses_fallback"})

	activeSet := env.handler.GetAuthoritativeActiveSessions(context.Background(), "ws-1")

	assert.True(t, activeSet["ses_fallback"],
		"must fall back to in-memory state when workspace not ready")
}

// TestGetAuthoritativeActiveSessions_StatuszError_ReturnsEmpty verifies
// that when the pod is Active but statusz returns an error (agentd
// crashed, port not ready), no sessions are claimed active.
func TestGetAuthoritativeActiveSessions_StatuszError_ReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env, srv := newStatuszTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	activeSet := env.handler.GetAuthoritativeActiveSessions(context.Background(), "ws-1")

	assert.Empty(t, activeSet,
		"statusz error must not claim any sessions active")
}

// TestGetAuthoritativeActiveSessions_LargeStatuszOver16KB is the
// regression test for the statusz decode cap fix. The previous code
// used io.LimitReader(resp.Body, 16*1024) — each session entry in
// statusz is ~300 bytes, so a workspace with ~55 sessions exceeds the
// cap. The decode silently failed and GetAuthoritativeActiveSessions
// returned an empty set, breaking the stuck-busy self-heal for heavy
// users.
//
// This test generates a statusz body with 100 sessions (>16 KB, ~30 KB
// total) where one session is busy. It asserts the busy session is
// detected. Reverting to 16*1024 would truncate the JSON, the decode
// would fail, and the busy session would NOT be in the set.
func TestGetAuthoritativeActiveSessions_LargeStatuszOver16KB(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Build a statusz body with 100 idle sessions + 1 busy session.
	// Total size will be >16 KB, well over the old cap.
	var sb strings.Builder
	sb.WriteString(`{"sessions":[`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		// Each entry is ~80 bytes of JSON; 100 entries with padding.
		// Add padding via a long title to push total over 16 KB.
		sb.WriteString(`{"id":"ses_idle_`)
		fmt.Fprintf(&sb, "%03d", i)
		sb.WriteString(`","status":"idle","title":"`)
		sb.WriteString(strings.Repeat("p", 200))
		sb.WriteString(`"}`)
	}
	// The busy session we actually care about detecting.
	sb.WriteString(`,{"id":"ses_busy_target","status":"busy","title":"`)
	sb.WriteString(strings.Repeat("p", 200))
	sb.WriteString(`"}]}`)

	body := sb.String()
	require.Greater(t, len(body), 16*1024,
		"test statusz body must exceed the old 16 KB cap (got %d bytes)", len(body))

	env, srv := newStatuszTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/statusz" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	activeSet := env.handler.GetAuthoritativeActiveSessions(context.Background(), "ws-1")

	assert.True(t, activeSet["ses_busy_target"],
		"busy session must be detected even when statusz body >16 KB (got %d bytes)", len(body))
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Subtask root-session resolution (US-65.x), ported to the US-69.11
// usage-bridge seam: the ABI InputRequest may carry root_session_id
// directly; when it does not, the bridge resolves the parent chain via
// the session-parent cache (unchanged). The user-visible event keeps
// SessionID = the subtask and RootSessionID = the user-visible parent.

func newSubtaskBridgeEnv(t *testing.T, backend http.HandlerFunc) (*testEnv, *eventbroker.Subscriber) {
	t.Helper()
	env := newTestEnvWithBackend(t, backend)
	env.handler.dialect = &agentoc.Dialect{}
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	t.Cleanup(stubUsageStream())
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	sub, _ := env.handler.userBroker.SubscribeUser("user-1")
	t.Cleanup(func() { env.handler.userBroker.UnsubscribeUser("user-1", sub) })
	return env, sub
}

func abiPermission(id, sessionID string) *abiv1.InputRequest {
	return &abiv1.InputRequest{
		Id: id, SessionId: sessionID, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: "shell", Patterns: []string{"ls"},
	}
}

func TestSubtaskPermission_BubblesToRootSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-password", pass)

		switch r.URL.Path {
		case "/session/ses_child":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":       "ses_child",
				"parentID": "ses_root",
			})
		case "/session/ses_root":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "ses_root",
			})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer backend.Close()

	env, sub := newSubtaskBridgeEnv(t, backend.Config.Handler.(http.HandlerFunc))
	env.handler.EnableSessionParentResolution()

	(&usageBridge{h: env.handler}).InputRequested("ws-1", abiPermission("per_subtask", "ses_child"))

	evt := recvWithTimeout(t, sub, "agent.permission")
	req, ok := evt.Data.(agent.PermissionRequest)
	require.True(t, ok, "event data should be *agent.PermissionRequest, got %T", evt.Data)
	assert.Equal(t, "per_subtask", req.ID)
	assert.Equal(t, "ses_child", req.SessionID, "SessionID stays the subtask")
	assert.Equal(t, "ses_root", req.RootSessionID, "RootSessionID points to user-visible parent")
}

// The ABI request may carry root_session_id directly (the pod knows its
// own session tree) — the bridge must use it without a parent walk.
func TestSubtaskPermission_ABIProvidesRoot_NoWalk(t *testing.T) {
	env, sub := newSubtaskBridgeEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no pod fetch expected when the ABI carries root_session_id, got %s", r.URL.Path)
	})
	env.handler.EnableSessionParentResolution()

	req := abiPermission("per_abiroot", "ses_child")
	req.RootSessionId = "ses_root"
	(&usageBridge{h: env.handler}).InputRequested("ws-1", req)

	evt := recvWithTimeout(t, sub, "agent.permission")
	pr := evt.Data.(agent.PermissionRequest)
	assert.Equal(t, "ses_child", pr.SessionID)
	assert.Equal(t, "ses_root", pr.RootSessionID)
}

func TestSubtaskPermission_ResolutionDisabled_RootEqualsSelf(t *testing.T) {
	env, sub := newSubtaskBridgeEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	(&usageBridge{h: env.handler}).InputRequested("ws-1", abiPermission("per_x", "ses_x"))

	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(agent.PermissionRequest)
	assert.Equal(t, "ses_x", req.SessionID)
	assert.Equal(t, "ses_x", req.RootSessionID, "fallback to self when resolution is disabled")
}

func TestSubtaskPermission_TopLevelSession_RootEqualsSelf(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses_top" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "ses_top",
			})
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	defer backend.Close()

	env, sub := newSubtaskBridgeEnv(t, backend.Config.Handler.(http.HandlerFunc))
	env.handler.EnableSessionParentResolution()

	(&usageBridge{h: env.handler}).InputRequested("ws-1", abiPermission("per_top", "ses_top"))

	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(agent.PermissionRequest)
	assert.Equal(t, "ses_top", req.SessionID)
	assert.Equal(t, "ses_top", req.RootSessionID, "top-level session is its own root")
}

func TestSubtaskQuestion_BubblesToRootSession(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_child":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_child", "parentID": "ses_root"})
		case "/session/ses_root":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_root"})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer backend.Close()

	env, sub := newSubtaskBridgeEnv(t, backend.Config.Handler.(http.HandlerFunc))
	env.handler.EnableSessionParentResolution()

	(&usageBridge{h: env.handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "que_subtask", SessionId: "ses_child", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Pick one", Header: "Choose",
		Options: []*abiv1.InputOption{{Label: "A", Description: "x"}},
	})

	evt := recvWithTimeout(t, sub, "agent.question")
	req := evt.Data.(agent.QuestionRequest)
	assert.Equal(t, "que_subtask", req.ID)
	assert.Equal(t, "ses_child", req.SessionID)
	assert.Equal(t, "ses_root", req.RootSessionID)
}

func TestSubtaskPermission_FetcherFails_FallsBackToSelf(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	env, sub := newSubtaskBridgeEnv(t, backend.Config.Handler.(http.HandlerFunc))
	env.handler.EnableSessionParentResolution()

	(&usageBridge{h: env.handler}).InputRequested("ws-1", abiPermission("per_x", "ses_unreachable"))

	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(agent.PermissionRequest)
	assert.Equal(t, "ses_unreachable", req.SessionID)
	assert.Equal(t, "ses_unreachable", req.RootSessionID, "fallback to self when fetch fails")
}

func TestSessionParentCache_InvalidateOnWorkspaceCacheFlush(t *testing.T) {
	calls := 0
	env := newInputTestEnv(t)
	env.handler.sessionParents = newSessionParentCache(
		func(_ context.Context, _, _ string) (string, error) {
			calls++
			return "", nil
		},
	)

	_ = env.handler.sessionParents.resolveRoot(context.Background(), "ws-1", "ses_x")
	require.Equal(t, 1, calls)

	env.handler.invalidateCaches(context.Background(), "ws-1")

	_ = env.handler.sessionParents.resolveRoot(context.Background(), "ws-1", "ses_x")
	require.Equal(t, 2, calls, "cache must be invalidated on workspace cache flush")
}

var _ = metav1.GetOptions{}
var _ = (*agent.PermissionRequest)(nil)

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

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

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.EnableSessionParentResolution()

	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_subtask",
		"sessionID":  "ses_child",
		"permission": "shell",
		"patterns":   []string{"ls"},
	})
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt := recvWithTimeout(t, sub, "agent.permission")

	req, ok := evt.Data.(*agent.PermissionRequest)
	require.True(t, ok, "event data should be *agent.PermissionRequest, got %T", evt.Data)
	assert.Equal(t, "per_subtask", req.ID)
	assert.Equal(t, "ses_child", req.SessionID, "SessionID stays the subtask")
	assert.Equal(t, "ses_root", req.RootSessionID, "RootSessionID points to user-visible parent")
}

func TestSubtaskPermission_ResolutionDisabled_RootEqualsSelf(t *testing.T) {
	env := newInputTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()

	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_x",
		"sessionID":  "ses_x",
		"permission": "shell",
	})

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(*agent.PermissionRequest)
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

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.EnableSessionParentResolution()

	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_top",
		"sessionID":  "ses_top",
		"permission": "shell",
	})
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(*agent.PermissionRequest)
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

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.EnableSessionParentResolution()

	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_subtask",
		"sessionID": "ses_child",
		"questions": []map[string]interface{}{
			{
				"question": "Pick one",
				"header":   "Choose",
				"options":  []map[string]string{{"label": "A", "description": "x"}},
			},
		},
	})
	env.handler.onRawEvent("ws-1", "question.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt := recvWithTimeout(t, sub, "agent.question")
	req := evt.Data.(*agent.QuestionRequest)
	assert.Equal(t, "que_subtask", req.ID)
	assert.Equal(t, "ses_child", req.SessionID)
	assert.Equal(t, "ses_root", req.RootSessionID)
}

func TestSubtaskPermission_FetcherFails_FallsBackToSelf(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.EnableSessionParentResolution()

	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_x",
		"sessionID":  "ses_unreachable",
		"permission": "shell",
	})
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt := recvWithTimeout(t, sub, "agent.permission")
	req := evt.Data.(*agent.PermissionRequest)
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

func TestE2E_SubtaskPermission_BubblesThroughSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.EnableSessionParentResolution()

	router := newStreamEventsRouter(env.handler)

	cancel, body, _, _ := doStreamingRequest(router, "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return env.handler.userBroker.WorkspaceSubscriberCount("ws-1") > 0
	}, 2*time.Second, 5*time.Millisecond, "subscriber should register on /events open")

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_e2e",
		"sessionID":  "ses_child",
		"permission": "shell",
		"patterns":   []string{"rm -rf /"},
	})
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	reader := bufio.NewReader(body)

	rawEvt := readNextSSEDataLine(t, reader)
	assert.Equal(t, "agent.event", rawEvt["type"], "first SSE line should be the raw opencode passthrough")

	normEvt := readNextSSEDataLine(t, reader)
	assert.Equal(t, "agent.permission", normEvt["type"])

	data, ok := normEvt["data"].(map[string]interface{})
	require.True(t, ok, "agent.permission.data must be an object, got %T", normEvt["data"])
	assert.Equal(t, "per_e2e", data["id"])
	assert.Equal(t, "ses_child", data["session_id"], "session_id stays the subtask")
	assert.Equal(t, "ses_root", data["root_session_id"], "root_session_id resolved to user-visible parent")
	assert.Equal(t, "shell", data["permission"])
}

func TestE2E_SubtaskQuestion_BubblesThroughSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_child":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_child", "parentID": "ses_root"})
		case "/session/ses_root":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_root"})
		default:
			http.Error(w, "unexpected: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer backend.Close()

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.EnableSessionParentResolution()

	router := newStreamEventsRouter(env.handler)
	cancel, body, _, _ := doStreamingRequest(router, "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return env.handler.userBroker.WorkspaceSubscriberCount("ws-1") > 0
	}, 2*time.Second, 5*time.Millisecond)

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_e2e",
		"sessionID": "ses_child",
		"questions": []map[string]interface{}{
			{
				"question": "Confirm?",
				"header":   "Confirm action",
				"options":  []map[string]string{{"label": "Yes", "description": "go"}},
			},
		},
	})
	env.handler.onRawEvent("ws-1", "question.asked", envelope)

	reader := bufio.NewReader(body)
	_ = readNextSSEDataLine(t, reader)

	normEvt := readNextSSEDataLine(t, reader)
	assert.Equal(t, "agent.question", normEvt["type"])
	data := normEvt["data"].(map[string]interface{})
	assert.Equal(t, "que_e2e", data["id"])
	assert.Equal(t, "ses_child", data["session_id"])
	assert.Equal(t, "ses_root", data["root_session_id"])
}

func TestE2E_NestedSubtask_TwoLevelsBubbleToRoot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/ses_grandchild":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_grandchild", "parentID": "ses_child"})
		case "/session/ses_child":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_child", "parentID": "ses_root"})
		case "/session/ses_root":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "ses_root"})
		default:
			http.Error(w, "unexpected: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer backend.Close()

	env := newInputTestEnv(t)
	env.handler.httpClient = &http.Client{
		Transport: &redirectTransport{server: backend},
		Timeout:   5 * time.Second,
	}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.EnableSessionParentResolution()

	router := newStreamEventsRouter(env.handler)
	cancel, body, _, _ := doStreamingRequest(router, "/api/v1/workspaces/ws-1/events")
	defer cancel()
	defer body.Close()

	require.Eventually(t, func() bool {
		return env.handler.userBroker.WorkspaceSubscriberCount("ws-1") > 0
	}, 2*time.Second, 5*time.Millisecond)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_nested",
		"sessionID":  "ses_grandchild",
		"permission": "shell",
		"patterns":   []string{"ls"},
	})
	env.handler.onRawEvent("ws-1", "permission.asked", envelope)

	reader := bufio.NewReader(body)
	_ = readNextSSEDataLine(t, reader)

	normEvt := readNextSSEDataLine(t, reader)
	require.Equal(t, "agent.permission", normEvt["type"])
	data := normEvt["data"].(map[string]interface{})
	assert.Equal(t, "ses_grandchild", data["session_id"])
	assert.Equal(t, "ses_root", data["root_session_id"], "two-level walk should reach the root")
}

var _ = metav1.GetOptions{}
var _ = (*agent.PermissionRequest)(nil)

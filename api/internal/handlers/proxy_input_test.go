// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func recvWithTimeout(t *testing.T, sub *eventbroker.Subscriber, what string) apitypes.WorkspaceSSEEvent {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s event", what)
		return apitypes.WorkspaceSSEEvent{}
	}
}

func newInputTestEnv(t *testing.T) *testEnv {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok, "Basic Auth should be present")
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-password", pass)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"method": r.Method,
			"path":   r.URL.Path,
		})
	})

	env.handler.dialect = &agentoc.Dialect{}

	proxy := env.router.Group("/api/v1/workspaces/:id")
	{
		proxy.GET("/question", env.handler.ListQuestions)
		proxy.POST("/question/:requestID/reply", env.handler.QuestionReply)
		proxy.POST("/question/:requestID/reject", env.handler.QuestionReject)
		proxy.GET("/permission", env.handler.ListPermissions)
		proxy.POST("/permission/:requestID/reply", env.handler.PermissionReply)
	}

	return env
}

func TestProxyInput_ListQuestions(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/question", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "GET", resp["method"])
	assert.Equal(t, "/question", resp["path"])
}

func TestProxyInput_QuestionReply(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := strings.NewReader(`{"answers":[["Go"]]}`)
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply", body)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "POST", resp["method"])
	assert.Equal(t, "/question/que_abc123/reply", resp["path"])
}

func TestProxyInput_QuestionReject(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/que_abc123/reject", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "/question/que_abc123/reject", resp["path"])
}

func TestProxyInput_ListPermissions(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/permission", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "GET", resp["method"])
	assert.Equal(t, "/permission", resp["path"])
}

func TestProxyInput_PermissionReply(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := strings.NewReader(`{"reply":"always"}`)
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/per_xyz789/reply", body)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "POST", resp["method"])
	assert.Equal(t, "/permission/per_xyz789/reply", resp["path"])
}

func TestProxyInput_InvalidQuestionID_NoPrefix(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/invalid/reply", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid question request ID format")
}

func TestProxyInput_InvalidQuestionID_WrongPrefix(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/question/per_abc/reply", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxyInput_InvalidPermissionID(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/permission/que_abc/reply", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid permission request ID format")
}

func TestProxyInput_WorkspaceNotActive(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-suspended", "", string(v1.WorkspacePhaseSuspended), "ws-suspended")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-suspended/question", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestProxyInput_WorkspaceNotFound(t *testing.T) {
	env := newInputTestEnv(t)
	env.wsMock.On("Get", mock.Anything, "ws-nonexistent", metav1.GetOptions{}).Return(nil, fmt.Errorf("not found")).Once()

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-nonexistent/question", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxyInput_BodyForwardedCorrectly(t *testing.T) {
	var receivedBody string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		_, _, _ = r.BasicAuth()
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`true`))
	})
	env.handler.dialect = &agentoc.Dialect{}

	proxy := env.router.Group("/api/v1/workspaces/:id")
	proxy.POST("/question/:requestID/reply", env.handler.QuestionReply)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := `{"answers":[["Go","Rust"]]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/question/que_abc123/reply", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, receivedBody)
}

func TestProxyInput_DialectNil(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/question", env.handler.ListQuestions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/question", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "dialect not configured")
}

// TestEpic25G1_fetchFromPod_LimitReader verifies that fetchFromPod truncates
// response bodies at 1 MiB, preventing unbounded memory allocation from a
// misbehaving upstream pod. (Epic 25 G1)
func TestEpic25G1_fetchFromPod_LimitReader(t *testing.T) {
	const respSize = 1<<20 + 200000 // 1.2 MiB
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", respSize)))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	// Replace httpClient with one that rewrites all requests to the test backend.
	handler.httpClient = &http.Client{
		Transport: &urlRewriteTransport{target: backend.URL},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, err := handler.fetchFromPod(ctx, "localhost", "test-pw", "/test")
	require.NoError(t, err)
	assert.Equal(t, 1<<20, len(body), "response body must be truncated to 1 MiB (got %d)", len(body))
}

type urlRewriteTransport struct {
	target    string
	transport http.RoundTripper
}

func (t *urlRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.target)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	if t.transport == nil {
		t.transport = http.DefaultTransport
	}
	return t.transport.RoundTrip(req)
}
func TestEpic13_wsConfig_PopulatesMaxActiveSessions(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	// Create a workspace CRD with MaxActiveSessions=10 and AutoApprovePermissions=false
	ws := makeWorkspaceCRD("ws-1", 10)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	// Call shouldAutoApprovePermissions — this is the production code path
	// that populates wsConfig from the workspace CRD.
	result := handler.shouldAutoApprovePermissions(context.Background(), "ws-1")
	assert.False(t, result, "workspace CRD has AutoApprovePermissions=false")

	// Verify wsConfig was populated with all fields from the CRD.
	cfg, ok := handler.GetWorkspaceConfigForTest("ws-1")
	require.True(t, ok, "wsConfig must be populated after shouldAutoApprovePermissions call")
	assert.Equal(t, 10, cfg.MaxActiveSessions)
	assert.False(t, cfg.AutoApprovePermissions)
}

// ===== US-69.11: input events via the usage bridge =====
//
// The tracker's dialect translation (onRawEvent →
// emitNormalizedInputEvent) is retired; pending-input lifecycle events
// now reach the user stream from the busy-gated ABI consumer through
// the production usageBridge (proxy_usagestream.go). The wire shapes
// the frontend consumes are unchanged.

func newBridgeInputHandler(t *testing.T) (*ProxyHandler, *eventbroker.UserEventBroker) {
	t.Helper()
	// The bridge's auto-approve check reads the Workspace spec via K8s:
	// mock a workspace with auto-approve OFF so permission inputs publish
	// to the user stream.
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, "ws-1", mock.Anything).Return(&v1.Workspace{}, nil)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	broker := eventbroker.NewUserEventBroker()
	handler.userBroker = broker
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	t.Cleanup(stubUsageStream())
	return handler, broker
}

func TestBridgeInput_QuestionAsked_ReachesUserStream(t *testing.T) {
	handler, broker := newBridgeInputHandler(t)
	userSub, _ := broker.SubscribeUser("user-1")
	defer broker.UnsubscribeUser("user-1", userSub)

	(&usageBridge{h: handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "que_abc", SessionId: "ses_xyz", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Pick?", Header: "H", Multiple: true,
		Options: []*abiv1.InputOption{{Label: "A", Description: "a"}},
	})

	evt := recvWithTimeout(t, userSub, "agent.question")
	assert.Equal(t, "agent.question", evt.Type)
	assert.Equal(t, "ws-1", evt.WorkspaceID, "user stream copy MUST have WorkspaceID")
	assert.Equal(t, "ses_xyz", evt.SessionID)
	assert.Equal(t, "que_abc", evt.RequestID)
	data, err := json.Marshal(evt.Data)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"id":"que_abc"`)
	assert.Contains(t, string(data), `"session_id":"ses_xyz"`)
	assert.Contains(t, string(data), `"question":"Pick?"`)
}

func TestBridgeInput_PermissionAsked_ReachesUserStream(t *testing.T) {
	handler, broker := newBridgeInputHandler(t)
	userSub, _ := broker.SubscribeUser("user-1")
	defer broker.UnsubscribeUser("user-1", userSub)

	(&usageBridge{h: handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "per_abc", SessionId: "ses_xyz", Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: "shell", Patterns: []string{"rm -rf /tmp"},
	})

	evt := recvWithTimeout(t, userSub, "agent.permission")
	assert.Equal(t, "agent.permission", evt.Type)
	data, err := json.Marshal(evt.Data)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"id":"per_abc"`)
	assert.Contains(t, string(data), `"permission":"shell"`)
}

// The resolved event is kind-agnostic on the user stream: both question
// and permission resolutions publish agent.question.resolved.
func TestBridgeInput_Resolved_ReachesUserStream(t *testing.T) {
	handler, broker := newBridgeInputHandler(t)
	userSub, _ := broker.SubscribeUser("user-1")
	defer broker.UnsubscribeUser("user-1", userSub)

	(&usageBridge{h: handler}).InputResolved("ws-1", "ses_xyz", "que_abc")

	evt := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "agent.question.resolved", evt.Type)
	assert.Equal(t, "ws-1", evt.WorkspaceID)
	data, ok := evt.Data.(map[string]string)
	require.True(t, ok, "resolved event data must be the request_id/session_id map")
	assert.Equal(t, "que_abc", data["request_id"])
	assert.Equal(t, "ses_xyz", data["session_id"])
}

func TestBridgeInput_UnknownOwner_SkipsUserStream(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	// Note: RecordWorkspaceOwner NOT called — owner unknown
	t.Cleanup(stubUsageStream())

	userSub, _ := handler.userBroker.SubscribeUser("user-unknown")
	defer handler.userBroker.UnsubscribeUser("user-unknown", userSub)

	(&usageBridge{h: handler}).InputRequested("ws-1", &abiv1.InputRequest{
		Id: "que_noowner", SessionId: "ses_noowner", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Pick?",
	})

	select {
	case evt := <-userSub.Ch:
		t.Fatalf("expected NO user stream event when owner unknown, got: %+v", evt)
	case <-time.After(200 * time.Millisecond):
		// expected — no event
	}
}

func TestBridgeInput_NilBrokerAndRequest_NoPanic(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = nil

	assert.NotPanics(t, func() {
		(&usageBridge{h: handler}).InputRequested("ws-1", &abiv1.InputRequest{Id: "q", SessionId: "s"})
		(&usageBridge{h: handler}).InputRequested("ws-1", nil)
		(&usageBridge{h: handler}).InputResolved("ws-1", "s", "q")
	})
}

// TestForgottenPublishGuard_BridgeInputReachesUserStream: every
// sidebar-relevant input event reaches the user stream — the guard
// against the Epic 55 bug class (a control event published
// workspace-only, dual-publish forgotten), ported to the bridge seams.
func TestForgottenPublishGuard_BridgeInputReachesUserStream(t *testing.T) {
	cases := []struct {
		name string
		fire func(h *ProxyHandler)
		want string
	}{
		{
			name: "agent.question",
			fire: func(h *ProxyHandler) {
				(&usageBridge{h: h}).InputRequested("ws-guard", &abiv1.InputRequest{
					Id: "que_guard", SessionId: "ses_guard", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Q?",
				})
			},
			want: "agent.question",
		},
		{
			name: "agent.permission",
			fire: func(h *ProxyHandler) {
				(&usageBridge{h: h}).InputRequested("ws-guard", &abiv1.InputRequest{
					Id: "per_guard", SessionId: "ses_guard", Kind: abiv1.InputKind_INPUT_KIND_PERMISSION, Permission: "edit",
				})
			},
			want: "agent.permission",
		},
		{
			name: "agent.question.resolved (question)",
			fire: func(h *ProxyHandler) {
				(&usageBridge{h: h}).InputResolved("ws-guard", "ses_guard", "que_guard_r")
			},
			want: "agent.question.resolved",
		},
		{
			name: "agent.question.resolved (permission)",
			fire: func(h *ProxyHandler) {
				(&usageBridge{h: h}).InputResolved("ws-guard", "ses_guard", "per_guard_r")
			},
			want: "agent.question.resolved",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			k8sMock := k8smocks.NewMockKubernetesClient()
			llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
			wsMock := k8smocks.NewMockWorkspaceInterface()
			k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
			llmMock.On("Workspaces", "default").Return(wsMock)
			wsMock.On("Get", mock.Anything, "ws-guard", mock.Anything).Return(&v1.Workspace{}, nil)
			handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
			require.NoError(t, err)
			handler.userBroker = eventbroker.NewUserEventBroker()
			handler.userBroker.RecordWorkspaceOwner("ws-guard", "user-guard")
			t.Cleanup(stubUsageStream())

			userSub, _ := handler.userBroker.SubscribeUser("user-guard")
			defer handler.userBroker.UnsubscribeUser("user-guard", userSub)

			tt.fire(handler)

			var found bool
			timeout := time.After(2 * time.Second)
			for {
				select {
				case evt := <-userSub.Ch:
					if evt.Type == tt.want {
						assert.Equal(t, "ws-guard", evt.WorkspaceID,
							"%s: user-stream event must carry WorkspaceID", tt.want)
						assert.NotEmpty(t, evt.SessionID,
							"%s: user-stream event must carry SessionID", tt.want)
						assert.NotEmpty(t, evt.RequestID,
							"%s: user-stream event must carry RequestID", tt.want)
						found = true
					}
				case <-timeout:
					if !found {
						t.Fatalf("FORGOTTEN PUBLISH: %s did not reach the user stream. "+
							"This means a sidebar-relevant event is missing its PublishToUser "+
							"path in the usage bridge.", tt.want)
					}
					return
				}
			}
		})
	}
}

// ===== US-55.3: Snapshot Marker + Anti-Entropy =====

func TestEmitPendingInputRequests_EmitsSnapshotCompleteMarker(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, fmt.Errorf("test: k8s unavailable")).Maybe()
	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	// emitPendingInputRequests will fail early (LlmsafespacesV1 returns error),
	// but the defer must still emit the marker.
	handler.emitPendingInputRequests(context.Background(), "ws-1")

	// Drain the begin marker, then expect complete.
	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")

	// The marker must be delivered to the user stream
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	assert.Equal(t, "agent.input.snapshot_complete", marker.Type)
	assert.Equal(t, "ws-1", marker.WorkspaceID)
	// Marker is per-workspace, not per-request
	assert.Empty(t, marker.SessionID)
	assert.Empty(t, marker.RequestID)
}

func TestEmitPendingInputRequests_MarkerFiresOnTimeout(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, fmt.Errorf("test: k8s unavailable")).Maybe()
	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	// k8s client returns an error from LlmsafespacesV1
	// → emitPendingInputRequests returns early, but defer fires marker.
	handler.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")

	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	assert.Equal(t, "agent.input.snapshot_complete", marker.Type)
	assert.Equal(t, "ws-1", marker.WorkspaceID)
}

func TestSnapshotUserWorkspaces_FansOutPendingForActiveWorkspaces(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("List", mock.Anything, mock.MatchedBy(func(opts metav1.ListOptions) bool {
		return opts.LabelSelector == labelUserID+"=user-1"
	})).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}},
		},
	}, nil)
	// emitPendingInputRequests will try Get("ws-1") and fail — marker still fires
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(nil, fmt.Errorf("test: not found"))

	broker := eventbroker.NewUserEventBroker()
	broker.RecordWorkspaceOwner("ws-1", "user-1")

	watcher, _ := workspace.NewWatcher(k8sMock, &testLogger{}, "default", func(*v1.Workspace) {})
	watcher.SetKnownPhase("ws-1", string(v1.WorkspacePhaseActive))

	h := &ProxyHandler{
		k8sClient:  k8sMock,
		logger:     &testLogger{},
		namespace:  "default",
		userBroker: broker,
		watcher:    watcher,
		dialect:    &agentoc.Dialect{},
	}

	userSub, _ := broker.SubscribeUser("user-1")
	defer broker.UnsubscribeUser("user-1", userSub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.snapshotUserWorkspaces(ctx, userSub, "user-1")

	// Should receive workspace.phase event for ws-1
	phaseEvt := recvWithTimeout(t, userSub, "workspace.phase")
	assert.Equal(t, "workspace.phase", phaseEvt.Type)
	assert.Equal(t, "ws-1", phaseEvt.WorkspaceID)

	// Drain the begin marker emitted by the fan-out before expecting complete.
	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")

	// Should receive the snapshot_complete marker for ws-1 (fan-out fired)
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	assert.Equal(t, "agent.input.snapshot_complete", marker.Type)
	assert.Equal(t, "ws-1", marker.WorkspaceID)
}

// ===== Snapshot begin/ok contract (false-interrupted-banner fixes) =====
//
// D10: the snapshot markers carry an ok flag so the frontend can distinguish
// "pod answered, zero pending" (authoritative — safe to reconcile) from
// "fetch failed/timed out" (keep existing pending state). A snapshot_begin
// marker opens the staging window before the fetch so snapshot-emitted
// question/permission events are staged separately from organic ones.

func TestEmitPendingInputRequests_BeginAndOKMarkerOnSuccess(t *testing.T) {
	env := newInputTestEnv(t)
	// Backend returns empty arrays for /question and /permission — a
	// successful fetch of an empty pending set.
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")

	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	env.handler.emitPendingInputRequests(context.Background(), "ws-1")

	begin := recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	assert.Equal(t, "agent.input.snapshot_begin", begin.Type)
	assert.Equal(t, "ws-1", begin.WorkspaceID)

	complete := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	assert.Equal(t, "agent.input.snapshot_complete", complete.Type)
	require.NotNil(t, complete.SnapshotOK, "ok marker must be present on success")
	assert.True(t, *complete.SnapshotOK)
}

func TestEmitPendingInputRequests_MarkerOKFalseOnBackendError(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	env.handler.dialect = &agentoc.Dialect{}
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")

	userSub, _ := env.handler.userBroker.SubscribeUser("user-1")
	defer env.handler.userBroker.UnsubscribeUser("user-1", userSub)

	env.handler.emitPendingInputRequests(context.Background(), "ws-1")

	begin := recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	assert.Equal(t, "agent.input.snapshot_begin", begin.Type)

	complete := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, complete.SnapshotOK, "ok marker must be present on failure")
	assert.False(t, *complete.SnapshotOK)
}

func TestEmitPendingInputRequests_MarkerOKFalseOnK8sFailure(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, fmt.Errorf("test: k8s unavailable")).Maybe()
	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	handler.emitPendingInputRequests(context.Background(), "ws-1")

	begin := recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	assert.Equal(t, "agent.input.snapshot_begin", begin.Type)

	complete := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, complete.SnapshotOK)
	assert.False(t, *complete.SnapshotOK)
}

// TestSnapshotOK_WireContract asserts the raw JSON encoding of a FAILED
// snapshot marker contains "snapshot_ok":false. The field is *bool +
// omitempty by design — if it were ever changed to bool + omitempty, false
// would silently vanish from the wire and every client would treat failed
// fetches as authoritative. Unit-level (not HTTP) because the SSE writer
// marshals this exact struct with encoding/json.
func TestSnapshotOK_WireContract(t *testing.T) {
	ok := false
	evt := apitypes.WorkspaceSSEEvent{
		Type:        "agent.input.snapshot_complete",
		WorkspaceID: "ws-1",
		SnapshotOK:  &ok,
		SnapshotID:  "ws-1-123",
	}
	raw, err := json.Marshal(evt)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"snapshot_ok":false`,
		"failed-snapshot markers MUST carry snapshot_ok:false on the wire")
	assert.Contains(t, string(raw), `"snapshot_id":"ws-1-123"`)

	// Success marker carries true; unrelated events carry neither field.
	okTrue := true
	evtOk := apitypes.WorkspaceSSEEvent{Type: "session.status", SnapshotOK: &okTrue}
	rawOk, _ := json.Marshal(evtOk)
	assert.Contains(t, string(rawOk), `"snapshot_ok":true`)

	evtNone := apitypes.WorkspaceSSEEvent{Type: "session.status", Status: "busy"}
	rawNone, _ := json.Marshal(evtNone)
	assert.NotContains(t, string(rawNone), "snapshot_ok")
	assert.NotContains(t, string(rawNone), "snapshot_id")
}

// ===== US-55.4: Regression Guards =====

// TestInputEventEnvelope_JSONRoundTrip verifies the D10 fields (request_id,
// session_id) survive JSON marshaling with the keys the frontend expects.
func TestInputEventEnvelope_JSONRoundTrip(t *testing.T) {
	evt := apitypes.WorkspaceSSEEvent{
		Type:        "agent.question",
		WorkspaceID: "ws-1",
		SessionID:   "ses-1",
		RequestID:   "que_1",
	}

	data, err := json.Marshal(evt)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	assert.Equal(t, "que_1", parsed["request_id"],
		"request_id must serialize as 'request_id' (frontend reads evt.request_id)")
	assert.Equal(t, "ses-1", parsed["session_id"],
		"session_id must serialize as 'session_id' (frontend reads evt.session_id)")
	assert.Equal(t, "ws-1", parsed["workspace_id"],
		"workspace_id must serialize as 'workspace_id' (frontend reads evt.workspace_id)")
}

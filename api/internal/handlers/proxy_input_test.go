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
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
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

// ===== US-16.3: Normalized Event Emission Tests =====

func makeEnvelope(eventType string, props map[string]interface{}) string {
	propsJSON, _ := json.Marshal(props)
	envelope, _ := json.Marshal(map[string]interface{}{
		"type":       eventType,
		"properties": json.RawMessage(propsJSON),
	})
	return string(envelope)
}

func TestNormalizedEvents_QuestionAsked(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_abc",
		"sessionID": "ses_xyz",
		"questions": []map[string]interface{}{
			{
				"question": "Pick?",
				"header":   "H",
				"options":  []map[string]string{{"label": "A", "description": "a"}},
			},
		},
	})
	handler.onRawEvent("ws-1", "question.asked", envelope)

	evt1 := recvWithTimeout(t, sub, "agent.event")
	assert.Equal(t, "agent.event", evt1.Type)
	assert.Equal(t, "question.asked", evt1.EventType)

	evt2 := recvWithTimeout(t, sub, "agent.question")
	assert.Equal(t, "agent.question", evt2.Type)
	data, err := json.Marshal(evt2.Data)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"id":"que_abc"`)
	assert.Contains(t, string(data), `"session_id":"ses_xyz"`)
}

func TestNormalizedEvents_QuestionResolved(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("question.replied", map[string]interface{}{
		"id":        "que_abc",
		"sessionID": "ses_xyz",
		"answers":   [][]string{{"Go"}},
	})
	handler.onRawEvent("ws-1", "question.replied", envelope)

	evt1 := recvWithTimeout(t, sub, "agent.event")
	assert.Equal(t, "agent.event", evt1.Type)

	evt2 := recvWithTimeout(t, sub, "agent.question.resolved")
	assert.Equal(t, "agent.question.resolved", evt2.Type)
	data := evt2.Data.(map[string]string)
	assert.Equal(t, "que_abc", data["request_id"])
	assert.Equal(t, "ses_xyz", data["session_id"])
}

func TestNormalizedEvents_QuestionRejected(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("question.rejected", map[string]interface{}{
		"id":        "que_abc",
		"sessionID": "ses_xyz",
	})
	handler.onRawEvent("ws-1", "question.rejected", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt2 := recvWithTimeout(t, sub, "agent.question.resolved")
	assert.Equal(t, "agent.question.resolved", evt2.Type)
}

func TestNormalizedEvents_PermissionAsked(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	ws := &v1.Workspace{
		Spec:   v1.WorkspaceSpec{AutoApprovePermissions: false},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1"},
	}
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_abc",
		"sessionID":  "ses_xyz",
		"permission": "shell",
		"patterns":   []string{"rm -rf /tmp"},
	})
	handler.onRawEvent("ws-1", "permission.asked", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt2 := recvWithTimeout(t, sub, "agent.permission")
	assert.Equal(t, "agent.permission", evt2.Type)
	data, _ := json.Marshal(evt2.Data)
	assert.Contains(t, string(data), `"id":"per_abc"`)
	assert.Contains(t, string(data), `"permission":"shell"`)
}

func TestNormalizedEvents_PermissionResolved(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("permission.replied", map[string]interface{}{
		"id":        "per_abc",
		"sessionID": "ses_xyz",
		"reply":     "always",
	})
	handler.onRawEvent("ws-1", "permission.replied", envelope)

	recvWithTimeout(t, sub, "agent.event")
	evt2 := recvWithTimeout(t, sub, "agent.permission.resolved")
	assert.Equal(t, "agent.permission.resolved", evt2.Type)
	data := evt2.Data.(map[string]string)
	assert.Equal(t, "per_abc", data["request_id"])
	assert.Equal(t, "always", data["reply"])
}

func TestNormalizedEvents_RawEventAlwaysPublished(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("session.diff", map[string]interface{}{"some": "data"})
	handler.onRawEvent("ws-1", "session.diff", envelope)

	evt := recvWithTimeout(t, sub, "agent.event")
	assert.Equal(t, "agent.event", evt.Type)
	assert.Equal(t, "session.diff", evt.EventType)

	select {
	case <-sub.Ch:
		t.Fatal("unexpected second event for unrelated event type")
	default:
	}
}

func TestNormalizedEvents_ParseError_NoNormalizedEvent(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeEnvelope("question.asked", map[string]interface{}{"invalid": true})
	handler.onRawEvent("ws-1", "question.asked", envelope)

	evt := recvWithTimeout(t, sub, "agent.event")
	assert.Equal(t, "agent.event", evt.Type)

	select {
	case <-sub.Ch:
		t.Fatal("should not publish normalized event on parse error")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNormalizedEvents_BrokerNil_NoPanic(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.dialect = &agentoc.Dialect{}

	envelope := `{"type":"question.asked","properties":{"id":"que_abc","sessionID":"ses_xyz","questions":[]}}`
	handler.onRawEvent("ws-1", "question.asked", envelope)
}

// ===== US-16.3 integration: real wiring through SSETracker.processEvent =====

func makePermissionAskedEvent(reqID, sessionID, permission string, patterns []string) string {
	props := map[string]interface{}{
		"id":         reqID,
		"sessionID":  sessionID,
		"permission": permission,
		"patterns":   patterns,
	}
	propsJSON, _ := json.Marshal(props)
	envelope, _ := json.Marshal(map[string]interface{}{
		"type":       "permission.asked",
		"properties": json.RawMessage(propsJSON),
	})
	return string(envelope)
}

func makeQuestionAskedEvent(reqID, sessionID string) string {
	props := map[string]interface{}{
		"id":        reqID,
		"sessionID": sessionID,
		"questions": []map[string]interface{}{
			{
				"question": "Pick?",
				"header":   "H",
				"options": []map[string]string{
					{"label": "A", "description": "a"},
				},
			},
		},
	}
	propsJSON, _ := json.Marshal(props)
	envelope, _ := json.Marshal(map[string]interface{}{
		"type":       "question.asked",
		"properties": json.RawMessage(propsJSON),
	})
	return string(envelope)
}

func makeResolutionEvent(eventType, reqID, sessionID, reply string) string {
	props := map[string]interface{}{
		"id":        reqID,
		"sessionID": sessionID,
	}
	if reply != "" {
		props["reply"] = reply
	}
	propsJSON, _ := json.Marshal(props)
	envelope, _ := json.Marshal(map[string]interface{}{
		"type":       eventType,
		"properties": json.RawMessage(propsJSON),
	})
	return string(envelope)
}

func TestNormalizedEvents_E2E_PermissionAsked_ViaProcessEvent(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	ws := &v1.Workspace{
		Spec:   v1.WorkspaceSpec{AutoApprovePermissions: false},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1"},
	}
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	tracker := sse.NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, func(string, string) {})
	tracker.SetOnRawEvent(handler.onRawEvent)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makePermissionAskedEvent("per_abc", "ses_xyz", "shell", []string{"rm -rf /tmp"})
	tracker.ProcessEvent("ws-1", envelope)

	evt1 := recvWithTimeout(t, sub, "opencode.event (permission.asked)")
	assert.Equal(t, "agent.event", evt1.Type)
	assert.Equal(t, "permission.asked", evt1.EventType)

	evt2 := recvWithTimeout(t, sub, "agent.permission")
	assert.Equal(t, "agent.permission", evt2.Type)
	data, mErr := json.Marshal(evt2.Data)
	require.NoError(t, mErr)
	assert.Contains(t, string(data), `"id":"per_abc"`)
	assert.Contains(t, string(data), `"session_id":"ses_xyz"`)
	assert.Contains(t, string(data), `"permission":"shell"`)
}

func TestNormalizedEvents_E2E_QuestionAsked_ViaProcessEvent(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	tracker := sse.NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, func(string, string) {})
	tracker.SetOnRawEvent(handler.onRawEvent)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeQuestionAskedEvent("que_abc", "ses_xyz")
	tracker.ProcessEvent("ws-1", envelope)

	evt1 := recvWithTimeout(t, sub, "opencode.event (question.asked)")
	assert.Equal(t, "agent.event", evt1.Type)
	assert.Equal(t, "question.asked", evt1.EventType)

	evt2 := recvWithTimeout(t, sub, "agent.question")
	assert.Equal(t, "agent.question", evt2.Type)
	data, mErr := json.Marshal(evt2.Data)
	require.NoError(t, mErr)
	assert.Contains(t, string(data), `"id":"que_abc"`)
	assert.Contains(t, string(data), `"session_id":"ses_xyz"`)
}

func TestNormalizedEvents_E2E_PermissionResolved_ViaProcessEvent(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	tracker := sse.NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, func(string, string) {})
	tracker.SetOnRawEvent(handler.onRawEvent)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeResolutionEvent("permission.replied", "per_abc", "ses_xyz", "always")
	tracker.ProcessEvent("ws-1", envelope)

	recvWithTimeout(t, sub, "opencode.event (permission.replied)")
	evt2 := recvWithTimeout(t, sub, "agent.permission.resolved")
	assert.Equal(t, "agent.permission.resolved", evt2.Type)
	data := evt2.Data.(map[string]string)
	assert.Equal(t, "per_abc", data["request_id"])
	assert.Equal(t, "ses_xyz", data["session_id"])
	assert.Equal(t, "always", data["reply"])
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

func TestNormalizedEvents_E2E_QuestionResolved_ViaProcessEvent(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.dialect = &agentoc.Dialect{}

	tracker := sse.NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, func(string, string) {})
	tracker.SetOnRawEvent(handler.onRawEvent)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	envelope := makeResolutionEvent("question.replied", "que_abc", "ses_xyz", "")
	tracker.ProcessEvent("ws-1", envelope)

	recvWithTimeout(t, sub, "opencode.event (question.replied)")
	evt2 := recvWithTimeout(t, sub, "agent.question.resolved")
	assert.Equal(t, "agent.question.resolved", evt2.Type)
	data := evt2.Data.(map[string]string)
	assert.Equal(t, "que_abc", data["request_id"])
	assert.Equal(t, "ses_xyz", data["session_id"])
}

// ===== US-55.2: Dual-Publish Input Events to User Stream =====

func TestNormalizedEvents_QuestionAsked_DualPublish(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	wsSub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", wsSub)
	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_dual",
		"sessionID": "ses_dual",
		"questions": []map[string]interface{}{
			{"question": "Pick?", "header": "H", "options": []map[string]string{{"label": "A", "description": "a"}}},
		},
	})
	handler.onRawEvent("ws-1", "question.asked", envelope)

	// Workspace stream
	recvWithTimeout(t, wsSub, "agent.event")
	wsEvt := recvWithTimeout(t, wsSub, "agent.question")
	assert.Equal(t, "agent.question", wsEvt.Type)
	assert.Equal(t, "ses_dual", wsEvt.SessionID)
	assert.Equal(t, "que_dual", wsEvt.RequestID)
	assert.Empty(t, wsEvt.WorkspaceID, "workspace stream copy should NOT have WorkspaceID")

	// User stream
	userEvt := recvWithTimeout(t, userSub, "agent.question")
	assert.Equal(t, "agent.question", userEvt.Type)
	assert.Equal(t, "ws-1", userEvt.WorkspaceID, "user stream copy MUST have WorkspaceID")
	assert.Equal(t, "ses_dual", userEvt.SessionID)
	assert.Equal(t, "que_dual", userEvt.RequestID)
}

func TestNormalizedEvents_PermissionAsked_DualPublish(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}
	handler.SetWorkspaceConfigForTest("ws-1", wsstate.Config{AutoApprovePermissions: false})

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_dual",
		"sessionID":  "ses_dual",
		"permission": "edit",
		"patterns":   []string{"file.go"},
	})
	handler.onRawEvent("ws-1", "permission.asked", envelope)

	userEvt := recvWithTimeout(t, userSub, "agent.permission")
	assert.Equal(t, "agent.permission", userEvt.Type)
	assert.Equal(t, "ws-1", userEvt.WorkspaceID)
	assert.Equal(t, "ses_dual", userEvt.SessionID)
	assert.Equal(t, "per_dual", userEvt.RequestID)
}

func TestNormalizedEvents_QuestionResolved_DualPublish(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	envelope := makeEnvelope("question.replied", map[string]interface{}{
		"id":        "que_resolve",
		"sessionID": "ses_resolve",
		"answers":   [][]string{{"Go"}},
	})
	handler.onRawEvent("ws-1", "question.replied", envelope)

	userEvt := recvWithTimeout(t, userSub, "agent.question.resolved")
	assert.Equal(t, "agent.question.resolved", userEvt.Type)
	assert.Equal(t, "ws-1", userEvt.WorkspaceID)
	assert.Equal(t, "ses_resolve", userEvt.SessionID)
	assert.Equal(t, "que_resolve", userEvt.RequestID)
}

func TestNormalizedEvents_PermissionResolved_DualPublish(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	envelope := makeEnvelope("permission.replied", map[string]interface{}{
		"id":        "per_resolve",
		"sessionID": "ses_resolve",
		"reply":     "once",
	})
	handler.onRawEvent("ws-1", "permission.replied", envelope)

	userEvt := recvWithTimeout(t, userSub, "agent.permission.resolved")
	assert.Equal(t, "agent.permission.resolved", userEvt.Type)
	assert.Equal(t, "ws-1", userEvt.WorkspaceID)
	assert.Equal(t, "ses_resolve", userEvt.SessionID)
	assert.Equal(t, "per_resolve", userEvt.RequestID)
}

func TestNormalizedEvents_UnknownOwner_SkipsUserStream(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	// Note: RecordWorkspaceOwner NOT called — owner unknown
	handler.dialect = &agentoc.Dialect{}

	wsSub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", wsSub)
	userSub, _ := handler.userBroker.SubscribeUser("user-unknown")
	defer handler.userBroker.UnsubscribeUser("user-unknown", userSub)

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_noowner",
		"sessionID": "ses_noowner",
		"questions": []map[string]interface{}{
			{"question": "Pick?", "header": "H", "options": []map[string]string{{"label": "A", "description": "a"}}},
		},
	})
	handler.onRawEvent("ws-1", "question.asked", envelope)

	// Workspace stream still receives
	recvWithTimeout(t, wsSub, "agent.event")
	wsEvt := recvWithTimeout(t, wsSub, "agent.question")
	assert.Equal(t, "agent.question", wsEvt.Type)

	// User stream does NOT receive
	select {
	case evt := <-userSub.Ch:
		t.Fatalf("expected NO user stream event when owner unknown, got: %+v", evt)
	case <-time.After(200 * time.Millisecond):
		// expected — no event
	}
}

func TestNormalizedEvents_NilUserBroker_NoPanic(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	handler.userBroker = nil
	handler.dialect = &agentoc.Dialect{}

	envelope := makeEnvelope("question.asked", map[string]interface{}{
		"id":        "que_nil",
		"sessionID": "ses_nil",
		"questions": []map[string]interface{}{
			{"question": "Pick?", "header": "H", "options": []map[string]string{{"label": "A", "description": "a"}}},
		},
	})
	// Should not panic
	handler.onRawEvent("ws-1", "question.asked", envelope)
}

func TestNormalizedEvents_AutoApprove_NeitherStream(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, fmt.Errorf("test: k8s unavailable")).Maybe()
	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	handler.dialect = &agentoc.Dialect{}

	// Enable auto-approve for this workspace
	handler.SetWorkspaceConfigForTest("ws-1", wsstate.Config{AutoApprovePermissions: true})

	wsSub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", wsSub)
	userSub, _ := handler.userBroker.SubscribeUser("user-1")
	defer handler.userBroker.UnsubscribeUser("user-1", userSub)

	envelope := makeEnvelope("permission.asked", map[string]interface{}{
		"id":         "per_auto",
		"sessionID":  "ses_auto",
		"permission": "edit",
		"patterns":   []string{"file.go"},
	})
	handler.onRawEvent("ws-1", "permission.asked", envelope)

	// opencode.event fires (raw), but agent.permission should NOT
	recvWithTimeout(t, wsSub, "agent.event")

	// Neither workspace nor user stream should receive agent.permission
	select {
	case evt := <-wsSub.Ch:
		if evt.Type == "agent.permission" {
			t.Fatal("auto-approved permission should NOT be published to workspace stream")
		}
	case <-time.After(200 * time.Millisecond):
		// expected
	}
	select {
	case evt := <-userSub.Ch:
		if evt.Type == "agent.permission" {
			t.Fatal("auto-approved permission should NOT be published to user stream")
		}
	case <-time.After(200 * time.Millisecond):
		// expected
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

// TestForgottenPublishGuard verifies every sidebar-relevant input event type
// reaches the user stream (PublishToUser). This guard prevents the bug class
// that caused Epic 55: a new control event type published to the workspace
// stream only, with the dual-publish forgotten.
//
// Sidebar-relevant event types and their dedicated coverage:
//   - session.status (busy/idle) — proxy_session_status_test.go
//   - agent_died                 — proxy_broker_agentdied_test.go
//   - agent.question/permission/.resolved — THIS TEST (the US-55.2 additions)
func TestForgottenPublishGuard_InputEventsReachUserStream(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		envelope  string
		wantType  string
	}{
		{
			name:      "agent.question",
			eventType: "question.asked",
			envelope: makeEnvelope("question.asked", map[string]interface{}{
				"id":        "que_guard",
				"sessionID": "ses_guard",
				"questions": []map[string]interface{}{
					{"question": "Q?", "header": "H", "options": []map[string]string{{"label": "A", "description": "a"}}},
				},
			}),
			wantType: "agent.question",
		},
		{
			name:      "agent.permission",
			eventType: "permission.asked",
			envelope: makeEnvelope("permission.asked", map[string]interface{}{
				"id":         "per_guard",
				"sessionID":  "ses_guard",
				"permission": "edit",
				"patterns":   []string{"file.go"},
			}),
			wantType: "agent.permission",
		},
		{
			name:      "agent.question.resolved",
			eventType: "question.replied",
			envelope: makeEnvelope("question.replied", map[string]interface{}{
				"id":        "que_guard_r",
				"sessionID": "ses_guard",
				"answers":   [][]string{{"Go"}},
			}),
			wantType: "agent.question.resolved",
		},
		{
			name:      "agent.permission.resolved",
			eventType: "permission.replied",
			envelope: makeEnvelope("permission.replied", map[string]interface{}{
				"id":        "per_guard_r",
				"sessionID": "ses_guard",
				"reply":     "once",
			}),
			wantType: "agent.permission.resolved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sMock := k8smocks.NewMockKubernetesClient()
			handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
			handler.userBroker = eventbroker.NewUserEventBroker()
			handler.userBroker.RecordWorkspaceOwner("ws-guard", "user-guard")
			handler.dialect = &agentoc.Dialect{}
			handler.SetWorkspaceConfigForTest("ws-guard", wsstate.Config{AutoApprovePermissions: false})

			userSub, _ := handler.userBroker.SubscribeUser("user-guard")
			defer handler.userBroker.UnsubscribeUser("user-guard", userSub)

			handler.onRawEvent("ws-guard", tt.eventType, tt.envelope)

			// Skip opencode.event (raw passthrough) — look for the normalized type
			var found bool
			timeout := time.After(2 * time.Second)
			for {
				select {
				case evt := <-userSub.Ch:
					if evt.Type == tt.wantType {
						assert.Equal(t, "ws-guard", evt.WorkspaceID,
							"%s: user-stream event must carry WorkspaceID", tt.wantType)
						assert.NotEmpty(t, evt.SessionID,
							"%s: user-stream event must carry SessionID", tt.wantType)
						assert.NotEmpty(t, evt.RequestID,
							"%s: user-stream event must carry RequestID", tt.wantType)
						found = true
					}
				case <-timeout:
					if !found {
						t.Fatalf("FORGOTTEN PUBLISH: %s did not reach the user stream. "+
							"This means a sidebar-relevant event is workspace-only — "+
							"add PublishToUser to its emit path.", tt.wantType)
					}
					return
				}
			}
		})
	}
}

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

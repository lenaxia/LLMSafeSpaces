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
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
)

// US-65 e2e integration test: exercises the real handler stack with a
// real Adapter against a mock opencode backend. Verifies the full
// pipeline: gin router -> ProxyHandler -> Adapter -> HTTP -> translate
// -> contract JSON response.

func TestE2E_Adapter_GetHistory_FullPipeline(t *testing.T) {
	// Mock opencode backend returning opencode-shaped history.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return opencode-shaped array (info+parts).
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"info": {"role":"user","id":"msg_1"},
				"parts": [{"type":"text","text":"hello"}]
			},
			{
				"info": {"role":"assistant","id":"msg_2","modelID":"gpt-4o"},
				"parts": [
					{"type":"text","text":"hi there"},
					{"type":"step-start"},
					{"type":"step-finish"}
				]
			}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)

	require.Equal(t, http.StatusOK, w.Code, "handler must return 200")

	// Verify contract-shaped JSON (NOT opencode-shaped).
	var msgs []session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs), "response must be valid contract JSON")
	require.Len(t, msgs, 2, "both messages must survive translation")

	// First message: user text.
	assert.Equal(t, "msg_1", msgs[0].ID)
	assert.Equal(t, session.MessageUser, msgs[0].Type)
	require.Len(t, msgs[0].Parts, 1, "user message has one text part")
	assert.Equal(t, "hello", msgs[0].Parts[0].Text)

	// Second message: assistant text, step-start/step-finish dropped.
	assert.Equal(t, "msg_2", msgs[1].ID)
	assert.Equal(t, session.MessageAssistant, msgs[1].Type)
	require.Len(t, msgs[1].Parts, 1, "step-start and step-finish must be dropped by translator")
	assert.Equal(t, "hi there", msgs[1].Parts[0].Text)
}

func TestE2E_Adapter_ListSessions_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"id":"ses_1","title":"First","status":{"type":"idle"}},
			{"id":"ses_2","title":"Second","status":{"type":"busy"}}
		]`))
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)

	require.Equal(t, http.StatusOK, w.Code)
	var sessions []session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sessions))
	require.Len(t, sessions, 2)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "First", sessions[0].Title)
	assert.Equal(t, session.StatusIdle, sessions[0].Status)
	assert.Equal(t, session.StatusBusy, sessions[1].Status)
}

func TestE2E_Adapter_CreateSession_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"ses_new","title":"New","status":{"type":"idle"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions", strings.NewReader(`{}`))

	require.Equal(t, http.StatusOK, w.Code)
	var s session.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	assert.Equal(t, "ses_new", s.ID)
	assert.Equal(t, "New", s.Title)
}

func TestE2E_Adapter_SendMessage_FullPipeline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/message") {
			// Read the request body to verify text extraction.
			body, _ := io.ReadAll(r.Body)
			assert.Contains(t, string(body), "hello world")

			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"info": {"role":"assistant","id":"msg_reply"},
				"parts": [{"type":"text","text":"I received your message"}]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello world"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", body)

	require.Equal(t, http.StatusOK, w.Code)
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_reply", msg.ID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
	require.Len(t, msg.Parts, 1)
	assert.Equal(t, "I received your message", msg.Parts[0].Text)
}

func TestE2E_Adapter_SendMessage_Error_IncludesCredentialHint(t *testing.T) {
	// When adapter.Send fails AND credentials are stale, the error
	// response must include the needsRefresh hint (same as legacy path).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	// Wire agentStateChecker that reports stale credentials.
	env.handler.agentStateChecker = stubAgentStateChecker{
		changedAt: time.Now().Add(-5 * time.Minute),
	}

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`)
	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", body)

	require.Equal(t, http.StatusBadGateway, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["agentNeedsRefresh"],
		"enrichment must add agentNeedsRefresh when credentials are stale")
	assert.NotEmpty(t, resp["credentialsPendingSince"],
		"enrichment must include timestamp when credentials are stale")
}

func TestE2E_Adapter_GetHistory_Backend500_Returns502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	w := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestE2E_Adapter_AbortSession_FullPipeline(t *testing.T) {
	abortCalled := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/interrupt") {
			abortCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)
	env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/abort", nil)
	assert.True(t, abortCalled, "adapter must call the interrupt endpoint")
}

// --- E2E test environment ---

type e2eEnv struct {
	handler *ProxyHandler
	router  *gin.Engine
}

func newE2EEnv(t *testing.T, backend *httptest.Server) *e2eEnv {
	t.Helper()
	backendHost, backendPortStr, ok := strings.Cut(strings.TrimPrefix(backend.URL, "http://"), ":")
	require.True(t, ok, "backend URL must contain a port: %s", backend.URL)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	wsCRD := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: backendHost},
	}
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(wsCRD, nil).Maybe()

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)
	secret := makePasswordSecret("ws-1", "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	port := extractPort(t, backend.URL)
	_ = backendPortStr // used for assertion only
	adapter := opencode.NewAdapter(
		handler.AdapterPasswordResolver(),
		handler.AdapterPodIPResolver(),
		nil,
		opencode.WithAdapterHTTPClient(backend.Client()),
		opencode.WithAdapterPort(port),
	)
	handler.SetAdapter(adapter)
	require.NotNil(t, handler.adapter, "adapter must be wired")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	{
		proxy.POST("/sessions", handler.CreateSession)
		proxy.GET("/sessions", handler.ListSessions)
		proxy.POST("/sessions/:sessionId/message", handler.SendMessage)
		proxy.GET("/sessions/:sessionId/message", handler.GetHistory)
		proxy.GET("/sessions/:sessionId", handler.GetSession)
		proxy.POST("/sessions/:sessionId/abort", handler.AbortSession)
		proxy.DELETE("/sessions/:sessionId", handler.DeleteSession)
	}

	return &e2eEnv{handler: handler, router: router}
}

func (e *e2eEnv) do(method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	e.router.ServeHTTP(w, req)
	return w
}

func extractPort(t *testing.T, url string) int {
	t.Helper()
	// url is "http://127.0.0.1:PORT"
	idx := strings.LastIndex(url, ":")
	require.Greater(t, idx, 0, "URL must contain a port: %s", url)
	var port int
	_, err := fmt.Sscanf(url[idx+1:], "%d", &port)
	require.NoError(t, err)
	return port
}

// Ensure wsstate import stays alive (used in proxy_handler construction).
var _ = wsstate.NewInMemoryStore

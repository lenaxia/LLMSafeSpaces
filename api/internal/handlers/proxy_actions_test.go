// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// --- US-69.9: the API edge of the actions union (flag gate, path
// authoritative sessionId, passthrough + typed error mapping) ---

// actStubPod serves the Act procedure: records the request, returns a
// canned envelope (success or a connect error).
type actStubPod struct {
	server *httptest.Server
	got    chan map[string]any
	fail   bool // serve a NotSupported connect error envelope
}

func newActStubPod(t *testing.T, fail bool) *actStubPod {
	t.Helper()
	stub := &actStubPod{got: make(chan map[string]any, 4), fail: fail}
	mux := http.NewServeMux()
	mux.HandleFunc("/llmsafespaces.abi.v1.HarnessABIService/Act", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		stub.got <- m
		w.Header().Set("Content-Type", "application/json")
		if stub.fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"code": "unimplemented", "message": "not declared in this authority's capability report",
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
			"sessionId": m["sessionId"], "interrupt": map[string]any{},
		}})
	})
	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func newActionsTestEnv(t *testing.T, terminus bool, podURL string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	log := &testLogger{}
	handler, err := NewProxyHandler(k8sMock, log, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.userBroker = nil

	wsName := "ws-act"
	podHost := strings.TrimPrefix(podURL, "http://")
	if idx := strings.LastIndex(podHost, ":"); idx >= 0 {
		podHost = podHost[:idx] // the port rides agentdPortOverride
	}
	ws := makeWorkspaceCRDWithStatus(wsName, podHost, string(v1.WorkspacePhaseActive), wsName)
	wsMock.On("Get", mock.Anything, wsName, mock.Anything).Return(ws, nil).Maybe()

	pwSecret := makePasswordSecret(wsName, "pw")
	_, err = fakeClientset.CoreV1().Secrets("default").Create(context.Background(), pwSecret, metav1.CreateOptions{})
	require.NoError(t, err)

	if terminus {
		handler.SetAgentdTerminus(true)
	}
	if podURL != "" {
		// Dial the stub's port instead of the real agentd port.
		if idx := strings.LastIndex(podURL, ":"); idx >= 0 {
			port, _ := strconv.Atoi(podURL[idx+1:])
			handler.agentdPortOverride = port
		}
	}

	router := gin.New()
	g := router.Group("/api/v1/workspaces/:id")
	g.POST("/sessions/:sessionId/actions", handler.SessionAction)
	return router
}

func TestSessionAction_FlagOffIsTyped501(t *testing.T) {
	router := newActionsTestEnv(t, false, "http://127.0.0.1:1")

	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-act/sessions/s1/actions",
		strings.NewReader(`{"interrupt":{}}`))
	router.ServeHTTP(res, req)

	require.Equal(t, http.StatusNotImplemented, res.Code)
	var body struct {
		Error struct {
			Capability string `json:"capability"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	assert.Equal(t, "abi.actions", body.Error.Capability, "D4: the surface does not exist off-regime")
}

func TestSessionAction_ForwardsUnionWithPathSessionID(t *testing.T) {
	stub := newActStubPod(t, false)
	router := newActionsTestEnv(t, true, stub.server.URL)

	res := httptest.NewRecorder()
	// The body's sessionId must NOT win over the path (the path is
	// authoritative — one session's action cannot mutate another's).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-act/sessions/s-real/actions",
		strings.NewReader(`{"sessionId":"s-forged","switchModel":{"model":{"id":"m1","provider":"p"}}}`))
	router.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	select {
	case got := <-stub.got:
		assert.Equal(t, "s-real", got["sessionId"], "path sessionId injected authoritatively")
		model, ok := got["switchModel"].(map[string]any)
		require.True(t, ok, "union passthrough: switchModel survived verbatim")
		assert.Equal(t, "m1", model["model"].(map[string]any)["id"])
	case <-time.After(2 * time.Second):
		t.Fatal("pod never received the action")
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.Equal(t, "s-real", out["sessionId"])
	assert.NotNil(t, out["interrupt"])
}

func TestSessionAction_NotSupportedMapsTo501(t *testing.T) {
	stub := newActStubPod(t, true)
	router := newActionsTestEnv(t, true, stub.server.URL)

	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-act/sessions/s1/actions",
		strings.NewReader(`{"compact":{}}`))
	router.ServeHTTP(res, req)

	require.Equal(t, http.StatusNotImplemented, res.Code, "typed NotSupported crosses the edge as 501, never a 500 guessing game")
	assert.Contains(t, res.Body.String(), "not declared")
}

func TestSessionAction_NonObjectBodyIs400(t *testing.T) {
	stub := newActStubPod(t, false)
	router := newActionsTestEnv(t, true, stub.server.URL)

	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-act/sessions/s1/actions",
		strings.NewReader(`"interrupt"`))
	router.ServeHTTP(res, req)
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

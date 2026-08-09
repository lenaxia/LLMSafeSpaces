// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// setupV2TestEnv creates a ProxyHandler with the V2 flag enabled, backed by
// a mock k8s client that returns a workspace CRD pointing at the given
// podIP. The cached password is pre-seeded so getPassword does not hit K8s
// Secrets.
func setupV2TestEnv(t *testing.T, podIP string) (*ProxyHandler, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus("ws-1", podIP, string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset).Maybe()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{}, nil)
	require.NoError(t, err)
	handler.SetV2SessionQueueEnabled(true)
	handler.SetCachedPasswordForTest("ws-1", "test-pw")
	handler.userBroker = eventbroker.NewUserEventBroker()

	return handler, func() {}
}

// --- EnqueueMessage V2 tests ---

func TestEnqueueV2_FlagOn_Success(t *testing.T) {
	srv := startV2Server(t, "test-pw", http.StatusOK,
		`{"data":{"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses-1"}}`)
	defer srv.Close()

	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":"hello v2"}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.EnqueueMessage(c)

	assert.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "msg_v2_1", resp["messageID"])
}

func TestEnqueueV2_FlagOn_EmptyText(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":""}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEnqueueV2_FlagOn_UnreachablePod(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "192.0.2.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":"hello"}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestEnqueueV2_FlagOff_UsesLegacyRedis(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "192.0.2.1")
	defer cleanup()
	handler.SetV2SessionQueueEnabled(false)

	// Without queueSvc, the legacy path returns 503 — proving the V2
	// path was NOT taken (it would have tried to reach the pod).
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":"hello"}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// --- SendPromptAsync V2 tests ---

func TestSendPromptAsyncV2_FlagOn_BypassesGuard(t *testing.T) {
	srv := startV2Server(t, "test-pw", http.StatusOK,
		`{"data":{"admittedSeq":2,"id":"msg_v2_2","sessionID":"ses-1"}}`)
	defer srv.Close()

	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	// Mark session as "active" — under V1, SendPromptAsync returns 409.
	// Under V2, the 409 guard is bypassed.
	handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/prompt_async",
		strings.NewReader(`{"parts":[{"type":"text","text":"busy-send"}]}`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.SendPromptAsync(c)

	// V2 path: 202 (not 409). The 409 guard was bypassed.
	assert.Equal(t, http.StatusAccepted, w.Code)
	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, "msg_v2_2", resp["messageID"])
}

func TestSendPromptAsyncV2_FlagOn_InvalidBody(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/prompt_async",
		strings.NewReader(`not json`))
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.SendPromptAsync(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- AbortSession V2 tests ---

func TestAbortV2_FlagOn_Success(t *testing.T) {
	srv := startV2Server(t, "test-pw", http.StatusNoContent, "")
	defer srv.Close()

	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/abort", nil)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.AbortSession(c)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestAbortV2_FlagOn_NoQueueMutation(t *testing.T) {
	// Under V2, abort does NOT touch Redis (queue survives). queueSvc is
	// nil by default (no SetMessageQueueService call). If the V2 path
	// touched it, the legacy code path would panic. 204 = V2 succeeded
	// without touching queueSvc.
	srv := startV2Server(t, "test-pw", http.StatusNoContent, "")
	defer srv.Close()

	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/abort", nil)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses-1"},
	}

	handler.AbortSession(c)
	assert.Equal(t, http.StatusNoContent, w.Code,
		"V2 abort must succeed without touching queueSvc (nil)")
}

// --- helpers ---

// startV2Server starts an httptest server on port 4096 that enforces Basic
// auth and returns the canned response. Skips the test if port 4096 is
// unavailable (same pattern as models_test.go).
func startV2Server(t *testing.T, password string, status int, body string) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skipf("port 4096 not available: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	srv.Listener = listener
	srv.Start()
	return srv
}

// silence metav1 unused (imported for consistency with sibling test files)
var _ = metav1.ObjectMeta{}

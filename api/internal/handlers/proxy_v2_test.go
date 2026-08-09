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

// TestV2HandlerPaths exercises all three V2 handler paths (enqueue,
// sendPromptAsync, abort) against a single shared httptest server on port
// 4096. Consolidating into one test function eliminates port-4096 contention
// between tests — the -race detector exposed cross-test interference where
// one test's srv.Close() would block for 5s while the next test's request
// hit the stale server.
//
// Tests that don't need a live server (validation, flag-gating,
// unreachable-pod) run as standalone tests below — they don't touch port 4096.
func TestV2HandlerPaths(t *testing.T) {
	// Single server for the entire test. Routes by PATH (not mutable state)
	// to eliminate the race between the test goroutine setting response
	// params and the server goroutine reading them.
	listener, err := net.Listen("tcp", "127.0.0.1:4096")
	if err != nil {
		t.Skipf("port 4096 not available: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "test-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_v2_1","sessionID":"ses-1"}}`))
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	t.Run("Enqueue_Success", func(t *testing.T) {
		handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
		defer cleanup()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/queue",
			strings.NewReader(`{"text":"hello v2"}`))
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}
		handler.EnqueueMessage(c)
		assert.Equal(t, http.StatusAccepted, w.Code)
		var resp map[string]string
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "msg_v2_1", resp["messageID"])
	})

	t.Run("SendPromptAsync_Bypasses409Guard", func(t *testing.T) {
		handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
		defer cleanup()
		handler.SetActiveSessionsForTest("ws-1", []string{"ses-1"})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"busy-send"}]}`))
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}
		handler.SendPromptAsync(c)
		assert.Equal(t, http.StatusAccepted, w.Code, "V2 must bypass the 409 guard")
		var resp map[string]string
		require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal(t, "msg_v2_1", resp["messageID"])
	})

	t.Run("Abort_Success", func(t *testing.T) {
		handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
		defer cleanup()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/abort", nil)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}
		handler.AbortSession(c)
		assert.Equal(t, http.StatusNoContent, w.Code)
	})

	t.Run("Abort_NoQueueMutation", func(t *testing.T) {
		handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
		defer cleanup()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/abort", nil)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}
		handler.AbortSession(c)
		assert.Equal(t, http.StatusNoContent, w.Code,
			"V2 abort must succeed without touching queueSvc (nil)")
	})
}

// --- Tests that don't need a live server (no port 4096) ---

func TestEnqueueV2_FlagOn_EmptyText(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":""}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

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
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestEnqueueV2_FlagOff_UsesLegacyRedis(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "192.0.2.1")
	defer cleanup()
	handler.SetV2SessionQueueEnabled(false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/queue",
		strings.NewReader(`{"text":"hello"}`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.EnqueueMessage(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestSendPromptAsyncV2_FlagOn_InvalidBody(t *testing.T) {
	handler, cleanup := setupV2TestEnv(t, "127.0.0.1")
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/prompt_async",
		strings.NewReader(`not json`))
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses-1"}}

	handler.SendPromptAsync(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

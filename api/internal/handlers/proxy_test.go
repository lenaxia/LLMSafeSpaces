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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
)

type testLogger struct{}

func (l *testLogger) Debug(msg string, kv ...interface{})                  {}
func (l *testLogger) Info(msg string, kv ...interface{})                   {}
func (l *testLogger) Warn(msg string, kv ...interface{})                   {}
func (l *testLogger) Error(msg string, err error, kv ...interface{})       {}
func (l *testLogger) Fatal(msg string, err error, kv ...interface{})       {}
func (l *testLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *testLogger) Sync() error                                          { return nil }

type redirectTransport struct {
	server *httptest.Server
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

type failFirstTransport struct {
	server   *httptest.Server
	attempts int32
	failIP   string
	newIP    string
}

func (t *failFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if strings.HasPrefix(host, t.failIP) {
		if atomic.AddInt32(&t.attempts, 1) == 1 {
			return nil, fmt.Errorf("dial tcp %s: connection refused", t.failIP)
		}
		req.URL.Host = t.newIP
	}
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

type alwaysFailTransport struct{}

func (t *alwaysFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp %s: connection refused", req.URL.Host)
}

type testEnv struct {
	handler   *ProxyHandler
	k8sMock   *k8smocks.MockKubernetesClient
	llmMock   *k8smocks.MockLLMSafespacesV1Interface
	wsMock    *k8smocks.MockWorkspaceInterface
	clientset *k8sfake.Clientset
	backend   *httptest.Server
	router    *gin.Engine
	log       *testLogger
}

func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok, "Basic Auth should be present")
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-password", pass)

		w.Header().Set("Content-Type", "application/json")
		// GET .../message must return a JSON array — that's what opencode
		// returns and what the paginated GetHistory handler expects to
		// decode. Tests that don't care about body content can leave it
		// empty; tests that DO care set up their own backend handler.
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/message") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"method": r.Method,
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
		})
	})
}

func newTestEnvWithBackend(t *testing.T, backendHandler http.HandlerFunc) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	backend := httptest.NewServer(backendHandler)
	t.Cleanup(func() { backend.Close() })

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	log := &testLogger{}
	handler, err := NewProxyHandler(k8sMock, log, "default", httpClient, nil)
	require.NoError(t, err)

	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	{
		proxy.POST("/sessions", handler.CreateSession)
		proxy.GET("/sessions", handler.ListSessions)
		proxy.POST("/sessions/:sessionId/message", handler.SendMessage)
		proxy.POST("/sessions/:sessionId/prompt", handler.SendPromptAsync)
		proxy.GET("/sessions/:sessionId/message", handler.GetHistory)
		proxy.GET("/sessions/:sessionId", handler.GetSession)
		proxy.POST("/sessions/:sessionId/abort", handler.AbortSession)
		proxy.DELETE("/sessions/:sessionId", handler.DeleteSession)
		proxy.GET("/events", handler.StreamEvents)
	}

	return &testEnv{
		handler:   handler,
		k8sMock:   k8sMock,
		llmMock:   llmMock,
		wsMock:    wsMock,
		clientset: fakeClientset,
		backend:   backend,
		router:    router,
		log:       log,
	}
}

func (e *testEnv) setupPasswordWithT(t *testing.T, workspaceID, password string) {
	secret := makePasswordSecret(workspaceID, password)
	_, err := e.clientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (e *testEnv) setupWorkspaceWithT(t *testing.T, name string, maxSessions int) {
	ws := makeWorkspaceCRD(name, maxSessions)
	e.wsMock.On("Get", mock.Anything, name, metav1.GetOptions{}).Return(ws, nil).Maybe()
}

func (e *testEnv) setupWorkspacePodWithT(t *testing.T, workspaceID, podIP, phase, _ string) {
	ws := makeWorkspaceCRDWithStatus(workspaceID, podIP, phase, "")
	e.wsMock.On("Get", mock.Anything, workspaceID, metav1.GetOptions{}).Return(ws, nil).Maybe()
}
func (e *testEnv) doRequestWithT(t *testing.T, method, path string, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	e.router.ServeHTTP(w, req)
	return w
}

// fakePWProvider implements interfaces.WorkspacePasswordProvider for tests.
type fakePWProvider struct {
	pw  string
	err error
}

func (f fakePWProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return f.pw, f.err
}

func TestProxy_ProxiesGETRequest(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "GET", resp["method"])
	assert.Equal(t, "/session", resp["path"])
}

func TestProxy_ProxiesPOSTRequest(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	body := strings.NewReader(`{"message":"hello"}`)
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions", body)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "POST", resp["method"])
	assert.Equal(t, "/session", resp["path"])
}

func TestProxy_SendsBasicAuth(t *testing.T) {
	var capturedUser, capturedPass string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		capturedUser, capturedPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "my-secret-pw")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "opencode", capturedUser)
	assert.Equal(t, "my-secret-pw", capturedPass)
}

func TestProxy_ForwardsQueryParameters(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions?limit=10&offset=0", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "limit=10&offset=0", resp["query"])
}

func TestProxy_StreamingResponse(t *testing.T) {
	// StreamEvents is now broker-based; it no longer proxies to the pod.
	// Verify: with a broker attached, the endpoint sets SSE headers and returns 200.
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	cancel, body, header, code := doStreamingRequest(env.router, "/api/v1/workspaces/ws-1/events")
	defer body.Close()

	// Allow the handler to write response headers.
	time.Sleep(30 * time.Millisecond)
	cancel()

	assert.Equal(t, http.StatusOK, *code)
	assert.Equal(t, "text/event-stream", header.Get("Content-Type"))
	assert.Equal(t, "no-cache", header.Get("Cache-Control"))
}

func TestProxy_StreamEvents_NilBrokerReturns503(t *testing.T) {
	// StreamEvents must not panic if broker is nil (Start() not called yet).
	env := newTestEnv(t)
	// Deliberately do NOT set env.handler.userBroker
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/events", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "event broker not initialized")
}

// TestProxy_SSEStreamPassthrough previously tested transparent proxy forwarding
// to the pod's /event endpoint. StreamEvents is now broker-based and no longer
// proxies to the pod; passthrough behavior is covered by stream_events_test.go.

func TestProxy_RetriesOnStaleIP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer backend.Close()

	transport := &failFirstTransport{
		server: backend,
		failIP: "10.0.0.1:4096",
		newIP:  "10.0.0.2:4096",
	}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	oldCRD := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	newCRD := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.2", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(oldCRD, nil).Once()
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(newCRD, nil).Once()

	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 5)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/sessions", handler.ListSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/sessions", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// attempts == 1 proves the first request hit the stale IP and failed.
	// w.Code == 200 proves the retry with the fresh IP succeeded.
	// Together they confirm a retry occurred and reached the backend.
	assert.Equal(t, int32(1), atomic.LoadInt32(&transport.attempts), "exactly one request should have hit the stale IP")
}

func TestProxy_ConnectionFailureReturns503(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)

	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 5)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{
		Transport: &alwaysFailTransport{},
		Timeout:   2 * time.Second,
	}, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/sessions", handler.ListSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/sessions", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "10", w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), "workspace connection failed")

	// Verify the new structured fields that the frontend uses for
	// contextual recovery messaging (PR #810 Part 3).
	body := w.Body.String()
	assert.Contains(t, body, `"code":"service_unavailable"`, "503 must carry code field")
	assert.Contains(t, body, `"reason":"agent_unreachable"`, "503 must carry reason field")
	assert.Contains(t, body, `"message":`, "503 must carry a human-readable message field")
}

func TestProxy_WorkspaceNotRunning(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		podIP string
	}{
		{"Pending phase", "Pending", ""},
		{"Creating phase", "Creating", ""},
		{"Suspended phase", "Suspended", ""},
		{"Running but no PodIP", string(v1.WorkspacePhaseActive), ""},
		{"Suspending phase", "Suspending", "10.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			sb := makeWorkspaceCRDWithStatus("ws-1", tt.podIP, tt.phase, "ws-1")
			env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(sb, nil).Once()

			w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Equal(t, "10", w.Header().Get("Retry-After"))
		})
	}
}

func TestProxy_WorkspaceNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.wsMock.On("Get", mock.Anything, "sb-missing", metav1.GetOptions{}).Return(nil, fmt.Errorf("not found")).Once()

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/sb-missing/sessions", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxy_PasswordCachedAfterFirstRead(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Track how many times the k8s secret is read
	var secretReadCount int32
	env.clientset.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&secretReadCount, 1)
		return false, nil, nil // fall through to default handler
	})

	w1 := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusOK, w1.Code)
	assert.Equal(t, int32(1), atomic.LoadInt32(&secretReadCount), "secret should be read exactly once on first request")

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w2 := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, int32(1), atomic.LoadInt32(&secretReadCount), "secret should NOT be re-read on second request (served from cache)")
}

func TestProxy_SecretNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "failed to retrieve workspace credentials")
}

func TestProxy_EmptyPasswordKey(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-pw-ws-1", Namespace: "default"},
		Data:       map[string][]byte{"password": {}},
	}
	_, err := env.clientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestProxy_ActiveSessionLimit(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 2)

	for i := 0; i < 2; i++ {
		env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
		sid := fmt.Sprintf("session-%d", i)
		w := env.doRequestWithT(t, "POST", fmt.Sprintf("/api/v1/workspaces/ws-1/sessions/%s/message", sid), strings.NewReader(`{"msg":"hi"}`))
		assert.Equal(t, http.StatusOK, w.Code, "session %s should succeed", sid)
	}

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/session-2/message", strings.NewReader(`{"msg":"hi"}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "10", w.Header().Get("Retry-After"))

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(2), body["maxActiveSessions"])
	assert.Equal(t, float64(10), body["retryAfter"])
	assert.Contains(t, body["error"], "active session limit reached")
}

func TestProxy_AlreadyActiveSessionSucceeds(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 1)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w1 := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"msg":"hi"}`))
	assert.Equal(t, http.StatusOK, w1.Code)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w2 := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"msg":"hi2"}`))
	assert.Equal(t, http.StatusOK, w2.Code, "same session should not be double-counted")
}

func TestProxy_ReadOnlyBypassesSessionLimit(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 1)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w1 := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"msg":"hi"}`))
	assert.Equal(t, http.StatusOK, w1.Code)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w2 := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/s1/message", nil)
	assert.Equal(t, http.StatusOK, w2.Code, "read-only GET history should bypass limit")
}

func TestProxy_CreateSessionBypassesLimit(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 0)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusOK, w.Code, "create session should bypass limit")
}

func TestProxy_ConnectionCeiling(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 100)

	for i := 0; i < 10; i++ {
		env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	}
	require.True(t, env.handler.acquireConnection("ws-1"))

	for i := 0; i < 9; i++ {
		assert.True(t, env.handler.acquireConnection("ws-1"))
	}

	assert.False(t, env.handler.acquireConnection("ws-1"), "11th connection should be rejected")

	env.handler.releaseConnection("ws-1")
	assert.True(t, env.handler.acquireConnection("ws-1"), "connection after release should succeed")
}

func TestProxy_ConnectionCeiling_Returns429(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 100)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = 10
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "connection limit reached")
}

func TestProxy_EndpointMapping(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		expectedTarget string
	}{
		{"create session", "POST", "/api/v1/workspaces/ws-1/sessions", "/session"},
		{"list sessions", "GET", "/api/v1/workspaces/ws-1/sessions", "/session"},
		{"send message", "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", "/session/s1/message"},
		{"get history", "GET", "/api/v1/workspaces/ws-1/sessions/s1/message", "/session/s1/message"},
		{"get session", "GET", "/api/v1/workspaces/ws-1/sessions/s1", "/session/s1"},
		{"delete session", "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", "/session/s1"},
		// NOTE: "events" is intentionally omitted — StreamEvents is broker-based
		// and does not proxy to the pod; it is covered by stream_events_test.go.
		// NOTE: "prompt async" and "abort" are intentionally omitted — these are
		// served by the V2 path (adapter/V2SessionClient) and do not proxy to
		// opencode's /session/<id>/prompt_async or /session/<id>/abort.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedPath string
			env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				// GET history must respond with a JSON array — opencode's
				// /session/{id}/message endpoint returns an array, and the
				// API's paginated GetHistory handler decodes one.
				if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/message") {
					_, _ = w.Write([]byte(`[]`))
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path})
			})
			env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
			env.setupPasswordWithT(t, "ws-1", "test-password")
			env.setupWorkspaceWithT(t, "ws-1", 5)

			w := env.doRequestWithT(t, tt.method, tt.path, nil)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, tt.expectedTarget, capturedPath, "proxy should map to correct target path")
		})
	}
}

func TestProxy_E2E_FullFlow(t *testing.T) {
	var requests []string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/session":
			if r.Method == "POST" {
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]string{"id": "sess-1"})
			} else {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []string{"sess-1"}})
			}
		case "/session/sess-1/message":
			if r.Method == "POST" {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"status": "streaming"})
			} else {
				// GET history must be a JSON array — that's the opencode contract.
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[]`))
			}
		case "/session/sess-1":
			if r.Method == "DELETE" {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
			}
			// NOTE: /event is intentionally omitted — StreamEvents no longer proxies to the pod.
			// NOTE: /session/<id>/prompt_async and /session/<id>/abort are intentionally
			// omitted — these are served by the V2 path (adapter/V2SessionClient) and
			// do not proxy to opencode in this test fixture.
		}
	})
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions", strings.NewReader(`{"runtime":"python"}`))
	assert.Equal(t, http.StatusCreated, w.Code)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/sess-1/message", strings.NewReader(`{"content":"hello"}`))
	assert.Equal(t, http.StatusOK, w.Code)

	env.handler.removeActiveSession(context.Background(), "ws-1", "sess-1")

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/sess-1/message", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/sess-1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	expected := []string{
		"POST /session",
		"GET /session",
		"POST /session/sess-1/message",
		"GET /session/sess-1/message",
		"DELETE /session/sess-1",
	}
	assert.Equal(t, expected, requests)
}

func TestProxy_E2E_MultipleWorkspaceIsolation(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	env.setupPasswordWithT(t, "ws-1", "pw-1")
	env.setupPasswordWithT(t, "sb-2", "pw-2")
	env.setupWorkspaceWithT(t, "ws-1", 1)
	env.setupWorkspaceWithT(t, "sb-2", 1)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusOK, w.Code)

	env.setupWorkspacePodWithT(t, "sb-2", "10.0.0.2", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "POST", "/api/v1/workspaces/sb-2/sessions/s2/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusOK, w.Code, "different workspace should have independent session tracking")

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w = env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s3/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "ws-1 should be at limit")
}

func TestProxy_WorkspaceNotFound_UsesDefaults(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-missing")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	for i := 0; i < 6; i++ {
		env.wsMock.On("Get", mock.Anything, "ws-missing", metav1.GetOptions{}).Return(nil, fmt.Errorf("not found")).Once()
	}

	for i := 0; i < 5; i++ {
		env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-missing")
		sid := fmt.Sprintf("s%d", i)
		w := env.doRequestWithT(t, "POST", fmt.Sprintf("/api/v1/workspaces/ws-1/sessions/%s/message", sid), strings.NewReader(`{}`))
		assert.Equal(t, http.StatusOK, w.Code, "session %s with default limit 5 should succeed", sid)
	}

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-missing")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s6/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "6th session with default limit 5 should be rejected")
}

func TestProxy_BackendErrorPassthrough(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal opencode error"})
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal opencode error")
}

func TestProxy_Backend404Passthrough(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/missing/message", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxy_CacheInvalidation(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetCachedPasswordForTest("ws-1", "old-password")
	handler.SetWorkspaceConfigForTest("ws-1", wsstate.Config{MaxActiveSessions: 5})
	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	handler.invalidateCaches(context.Background(), "ws-1")

	_, pwOk := handler.GetCachedPasswordForTest("ws-1")
	assert.False(t, pwOk, "password cache should be cleared")

	_, wsOk := handler.GetWorkspaceConfigForTest("ws-1")
	assert.False(t, wsOk, "workspace config cache should be cleared")

	assert.False(t, handler.HasActiveWorkspaceForTest("ws-1"), "active sessions should be cleared")
}

func TestProxy_PhaseChangeCallback(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetCachedPasswordForTest("ws-1", "password")
	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	phases := []string{phaseSuspending, phaseSuspended, phaseTerminating, phaseTerminated}
	for _, phase := range phases {
		sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", phase, "ws-1")
		handler.onPhaseChange(sb)
	}

	_, pwOk := handler.GetCachedPasswordForTest("ws-1")
	assert.False(t, pwOk, "phase change to %s should invalidate password cache")
}

func TestProxy_PhaseChange_RunningNoInvalidation(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetCachedPasswordForTest("ws-1", "password")

	// Set prior phase to Active so this is a no-op Active→Active reconcile event.
	// The seed call (prior=="") intentionally invalidates caches and restarts SSE subscriptions.
	handler.SetPriorPhaseForTest("ws-1", string(phaseActive))

	sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb)

	_, pwOk := handler.GetCachedPasswordForTest("ws-1")
	assert.True(t, pwOk, "phase change to Running should NOT invalidate cache")
}

func TestProxy_ConcurrentRequests(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	env.setupPasswordWithT(t, "ws-1", "test-password")
	for i := 0; i < 5; i++ {
		env.setupWorkspaceWithT(t, "ws-1", 100)
		env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	}

	results := make(chan int, 5)
	for i := 0; i < 5; i++ {
		go func() {
			w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions", nil)
			results <- w.Code
		}()
	}

	for i := 0; i < 5; i++ {
		select {
		case code := <-results:
			assert.Equal(t, http.StatusOK, code)
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent request timed out")
		}
	}
}

func TestProxy_E2E_MaxActiveSessionsCustom(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 3)

	for i := 0; i < 3; i++ {
		env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
		sid := fmt.Sprintf("s%d", i)
		w := env.doRequestWithT(t, "POST", fmt.Sprintf("/api/v1/workspaces/ws-1/sessions/%s/message", sid), strings.NewReader(`{}`))
		assert.Equal(t, http.StatusOK, w.Code)
	}

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s3/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestProxy_RemoveActiveSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetActiveSessionsForTest("ws-1", []string{"s1", "s2"})

	handler.removeActiveSession(context.Background(), "ws-1", "s1")
	assert.Equal(t, 1, handler.activeSessionCount(context.Background(), "ws-1"))

	handler.removeActiveSession(context.Background(), "ws-1", "s2")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"))

	assert.False(t, handler.HasActiveWorkspaceForTest("ws-1"), "empty session set should be cleaned up")
}

func TestProxy_RemoveNonexistentSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.removeActiveSession(context.Background(), "sb-missing", "s1")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "sb-missing"))
}

func TestIsConnectionError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		isConn bool
	}{
		{"connection refused", fmt.Errorf("dial tcp 10.0.0.1:4096: connection refused"), true},
		{"no such host", fmt.Errorf("dial tcp: lookup ws-1.default.svc: no such host"), true},
		{"connection reset", fmt.Errorf("read tcp: connection reset by peer"), true},
		{"i/o timeout", fmt.Errorf("dial tcp 10.0.0.1:4096: i/o timeout"), true},
		{"EOF", fmt.Errorf("unexpected EOF"), true},
		{"network unreachable", fmt.Errorf("dial tcp: network is unreachable"), true},
		{"nil error", nil, false},
		{"other error", fmt.Errorf("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.isConn, isConnectionError(tt.err))
		})
	}
}

func TestProxy_NewProxyHandler_Validation(t *testing.T) {
	tests := []struct {
		name      string
		k8sClient pkginterfaces.KubernetesClient
		logger    pkginterfaces.LoggerInterface
		expectErr string
	}{
		{"nil k8s client", nil, &testLogger{}, "kubernetes client cannot be nil"},
		{"nil logger", k8smocks.NewMockKubernetesClient(), nil, "logger cannot be nil"},
		{"both valid", k8smocks.NewMockKubernetesClient(), &testLogger{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProxyHandler(tt.k8sClient, tt.logger, "default", nil, nil)
			if tt.expectErr != "" {
				assert.EqualError(t, err, tt.expectErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestProxy_DefaultNamespace(t *testing.T) {
	h, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "default", h.namespace)
}

func TestProxy_CustomHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 10 * time.Second}
	h, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "ns", custom, nil)
	require.NoError(t, err)
	assert.Equal(t, custom, h.httpClient)
}

func TestProxy_ConnectionCountTracking(t *testing.T) {
	h, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	assert.Equal(t, 0, h.connectionCount("ws-1"))
	h.acquireConnection("ws-1")
	assert.Equal(t, 1, h.connectionCount("ws-1"))
	h.acquireConnection("ws-1")
	assert.Equal(t, 2, h.connectionCount("ws-1"))
	h.releaseConnection("ws-1")
	assert.Equal(t, 1, h.connectionCount("ws-1"))
	h.releaseConnection("ws-1")
	assert.Equal(t, 0, h.connectionCount("ws-1"))
}

func TestProxy_E2E_SSEDrivenSessionLifecycle(t *testing.T) {
	idleSignal := make(chan struct{}, 1)

	sseBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			flusher := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			select {
			case <-idleSignal:
				// Emit real opencode flat-format session.status event
				type statusObj struct {
					Type string `json:"type"`
				}
				type sessionStatusProps struct {
					SessionID string    `json:"sessionID"`
					Status    statusObj `json:"status"`
				}
				payload := struct {
					Type       string             `json:"type"`
					Properties sessionStatusProps `json:"properties"`
				}{
					Type:       "session.status",
					Properties: sessionStatusProps{SessionID: "s1", Status: statusObj{Type: "idle"}},
				}
				data, _ := json.Marshal(payload)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer sseBackend.Close()

	transport := &redirectTransport{server: sseBackend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 1)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(
		makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil,
	)

	handler.sseTracker = sse.NewTracker(httpClient, &testLogger{}, handler.onSessionIdle)
	handler.sseTracker.SetPasswordGetter(handler)
	handler.sseTracker.SetPodIPResolver(handler.getPodIPForSSE)
	handler.sseTracker.SetOnSessionActive(handler.onSessionActive)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	proxy := router.Group("/api/v1/workspaces/:id")
	proxy.POST("/sessions/:sessionId/message", handler.SendMessage)

	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(
		makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil,
	)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"msg":"hi"}`))
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, handler.activeSessionCount(context.Background(), "ws-1"), "session s1 should be active")

	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(
		makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil,
	)

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s2/message", strings.NewReader(`{"msg":"hi"}`))
	router.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code, "limit of 1 should reject second session")

	idleSignal <- struct{}{}

	require.Eventually(t, func() bool {
		return handler.activeSessionCount(context.Background(), "ws-1") == 0
	}, 3*time.Second, 50*time.Millisecond, "SSE idle event should clear session s1")

	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(
		makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1"), nil,
	)

	handler.sseTracker.StopWatching("ws-1")

	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s2/message", strings.NewReader(`{"msg":"hi"}`))
	router.ServeHTTP(w3, req3)
	assert.Equal(t, http.StatusOK, w3.Code, "after SSE idle, new session should succeed")
}

func TestProxy_E2E_SSEBusyEventAddsActiveSession(t *testing.T) {
	handler, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	handler.SetWorkspaceConfigForTest("ws-1", wsstate.Config{MaxActiveSessions: 5})

	handler.onSessionActive("ws-1", "s1")
	assert.Equal(t, 1, handler.activeSessionCount(context.Background(), "ws-1"), "busy event should add session s1")

	handler.onSessionActive("ws-1", "s1")
	assert.Equal(t, 1, handler.activeSessionCount(context.Background(), "ws-1"), "duplicate busy should not double-count")

	handler.onSessionActive("ws-1", "s2")
	assert.Equal(t, 2, handler.activeSessionCount(context.Background(), "ws-1"), "second busy session should be counted")
}

func TestProxy_SessionLeak_NotOnConnectionCeilingReject(t *testing.T) {
	env := newTestEnv(t)
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")

	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = 10
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	assert.Equal(t, 0, env.handler.activeSessionCount(context.Background(), "ws-1"),
		"session should not leak into active set when connection ceiling rejects")
}

func TestProxy_SessionLeak_CleanedUpOn503(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 5)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{
		Transport: &alwaysFailTransport{},
		Timeout:   2 * time.Second,
	}, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/message", handler.SendMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{}`))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"),
		"active session should be cleaned up when proxy fails with 503")
}

func TestProxy_GetPodIPForSSE_RunningReturnsIP(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	ip := handler.getPodIPForSSE("ws-1")
	assert.Equal(t, "10.0.0.1", ip)
}

func TestProxy_GetPodIPForSSE_SuspendedReturnsEmpty(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	crd := makeWorkspaceCRDWithStatus("ws-1", "", "Suspended", "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	ip := handler.getPodIPForSSE("ws-1")
	assert.Equal(t, "", ip)
}

func TestProxy_GetPodIPForSSE_NotFoundReturnsEmpty(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	wsMock.On("Get", mock.Anything, "sb-missing", metav1.GetOptions{}).Return(nil, fmt.Errorf("not found")).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	ip := handler.getPodIPForSSE("sb-missing")
	assert.Equal(t, "", ip)
}

func TestProxy_OnPhaseChange_SuspendingStopsSSE(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.sseTracker = sse.NewTracker(
		&http.Client{},
		&testLogger{},
		func(workspaceID, sessionID string) {},
	)
	handler.sseTracker.SetPasswordGetter(fakePWProvider{pw: "pw"})
	handler.sseTracker.SetPodIPResolver(func(workspaceID string) string { return "10.0.0.1" })

	handler.sseTracker.EnsureWatching("ws-1")
	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount())

	phases := []string{phaseSuspending, phaseSuspended, phaseTerminating, phaseTerminated}
	for _, phase := range phases {
		handler.sseTracker.EnsureWatching("ws-1")
		sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", phase, "ws-1")
		handler.onPhaseChange(sb)
		assert.Equal(t, 0, handler.sseTracker.SubscriptionCount(),
			"SSE subscription should be stopped on phase %s", phase)
	}
}

func TestProxy_OnPhaseChange_RunningKeepsSSE(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.sseTracker = sse.NewTracker(
		&http.Client{},
		&testLogger{},
		func(workspaceID, sessionID string) {},
	)
	handler.sseTracker.SetPasswordGetter(fakePWProvider{pw: "pw"})
	handler.sseTracker.SetPodIPResolver(func(workspaceID string) string { return "10.0.0.1" })

	handler.sseTracker.EnsureWatching("ws-1")
	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount())

	// Set prior to Active — this is the Active→Active reconcile path. The subscription
	// must NOT be reset (no Stop+EnsureWatching) because the workspace hasn't changed phase.
	handler.SetPriorPhaseForTest("ws-1", string(phaseActive))

	sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb)

	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount(),
		"SSE subscription should NOT be stopped on Active→Active reconcile")
}

// TestProxy_OnPhaseChange_CreatingToActive_ResetsSSETracker verifies that when
// the workspace transitions from Creating (or any non-Active phase) to Active,
// the SSE tracker subscription is reset (Stop + EnsureWatching) so that any
// backoff accumulated during the Creating phase is cleared immediately.
//
// Regression test for: workspace becomes Active but SSETracker is mid-backoff
// (30s max) from repeated "no pod IP" failures during Creating phase. User
// sends a message within the backoff window → idle event never reaches broker
// → response doesn't appear until page reload.
func TestProxy_OnPhaseChange_CreatingToActive_ResetsSSETracker(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.sseTracker = sse.NewTracker(
		&http.Client{},
		&testLogger{},
		func(workspaceID, sessionID string) {},
	)
	handler.sseTracker.SetPasswordGetter(fakePWProvider{pw: "pw"})
	handler.sseTracker.SetPodIPResolver(func(workspaceID string) string { return "10.0.0.1" })

	// Simulate the scenario: subscription is already running (started while
	// workspace was Creating) and has been backed off.
	handler.sseTracker.EnsureWatching("ws-1")
	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount(), "subscription must exist before transition")

	// Record the subscription cancel func address before the transition so we
	// can verify it was replaced (Stop + re-EnsureWatching creates a new one).
	// We can't directly inspect the cancel func, but we can verify the count
	// stays at 1 (Stop decrements to 0, EnsureWatching brings back to 1).

	// Set prior phase to Creating to trigger the reset path.
	handler.SetPriorPhaseForTest("ws-1", "Creating")

	sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb)

	// Subscription must still exist (was reset, not stopped permanently).
	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount(),
		"SSE subscription must be re-established after Creating→Active transition")
}

// TestProxy_OnPhaseChange_ActiveToActive_NoReset verifies that Active→Active
// reconcile calls (no phase transition) do NOT reset the SSE tracker, since
// the subscription is already healthy.
func TestProxy_OnPhaseChange_ActiveToActive_NoReset(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.sseTracker = sse.NewTracker(
		&http.Client{},
		&testLogger{},
		func(workspaceID, sessionID string) {},
	)
	handler.sseTracker.SetPasswordGetter(fakePWProvider{pw: "pw"})
	handler.sseTracker.SetPodIPResolver(func(workspaceID string) string { return "10.0.0.1" })

	handler.sseTracker.EnsureWatching("ws-1")
	// Prime the cache with Active so onPhaseChange sees Active→Active.
	handler.SetPriorPhaseForTest("ws-1", string(phaseActive))

	sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb)

	// Subscription must still exist and must NOT have been reset.
	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount(),
		"Active→Active reconcile must not reset the SSE subscription")
}

// TestProxy_OnPhaseChange_SecondActiveNoManualSeed_PreservesState is the
// regression test for a US-45.1 bug that almost shipped: the new
// InvalidateAll cleared priorPhase, which made every subsequent
// Active→Active reconcile event look like the first invocation and
// therefore wipe active sessions / deleted tombstones / password cache.
//
// The original invalidateCaches (pre-US-45.1) deliberately did NOT touch
// priorPhase. This test exercises the full path: two onPhaseChange
// calls with phase=Active and no manual seeding of state between them —
// proving that the second call (which goes through the prior==Active
// branch) preserves the state the first call established.
func TestProxy_OnPhaseChange_SecondActiveNoManualSeed_PreservesState(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	// Seed state that the first onPhaseChange(Active) would naturally
	// establish via downstream code paths (active session via prompt,
	// deleted tombstone via DeleteSession, cached password via getPassword).
	handler.SetActiveSessionsForTest("ws-1", []string{"sess-1"})
	handler.MarkSessionDeletedForTest("ws-1", "sess-deleted")
	handler.SetCachedPasswordForTest("ws-1", "pw")

	// First onPhaseChange(Active): no prior, so invalidateCaches is
	// called (would wipe state in a buggy impl), then prior is set.
	sb1 := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb1)

	// Re-seed state that was just cleared by the first call (this mirrors
	// what real downstream code does after the first Active event).
	handler.SetActiveSessionsForTest("ws-1", []string{"sess-1"})
	handler.MarkSessionDeletedForTest("ws-1", "sess-deleted")
	handler.SetCachedPasswordForTest("ws-1", "pw")

	// Second onPhaseChange(Active): prior=="Active", so this is the
	// reconcile branch. State MUST be preserved.
	sb2 := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb2)

	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "sess-1"),
		"Active→Active reconcile must preserve active sessions (regression: US-45.1 InvalidateAll was clearing priorPhase, wiping activeSess)")
	assert.True(t, handler.isSessionDeleted("ws-1", "sess-deleted"),
		"Active→Active reconcile must preserve deleted tombstones (regression: late SSE events for deleted sessions would resurrect zombie session_index rows)")
	_, pwOk := handler.GetCachedPasswordForTest("ws-1")
	assert.True(t, pwOk,
		"Active→Active reconcile must preserve password cache (regression: extra K8s Secret fetches on every redundant watch event)")
}

// TestProxy_OnPhaseChange_SeedCallActive_StartsSSETracker verifies that when onPhaseChange
// is called for an Active workspace with no prior phase (prior=="", the seed case fired by
// seedResourceVersion on API restart), the SSE tracker starts a new subscription.
// This is the regression test for the startup race where EnsureWatching was never called for
// already-Active workspaces because the watcher goroutine seeded knownPhases asynchronously.
func TestProxy_OnPhaseChange_SeedCallActive_StartsSSETracker(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.sseTracker = sse.NewTracker(
		&http.Client{},
		&testLogger{},
		func(workspaceID, sessionID string) {},
	)
	handler.sseTracker.SetPasswordGetter(fakePWProvider{pw: "pw"})
	handler.sseTracker.SetPodIPResolver(func(workspaceID string) string { return "10.0.0.1" })

	// No prior phase set — this is the seed call path.
	assert.Equal(t, 0, handler.sseTracker.SubscriptionCount(), "no subscription before seed")

	ws := makeWorkspaceCRDWithStatus("ws-seed", "10.0.0.1", string(phaseActive), "ws-seed")
	handler.onPhaseChange(ws)

	assert.Equal(t, 1, handler.sseTracker.SubscriptionCount(),
		"SSE subscription must be started for Active workspace on seed call (prior==\"\")")
}

func TestProxy_ActivityNotRecordedOnProxyFailure(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 5)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{
		Transport: &alwaysFailTransport{},
		Timeout:   2 * time.Second,
	}, nil)
	require.NoError(t, err)

	tracker := activity.NewActivityTracker(k8sMock, &testLogger{}, "default")
	handler.activityTracker = tracker

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/sessions", handler.ListSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/sessions", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, tracker.PendingCount(),
		"activity should NOT be recorded when proxy call fails")
}

func TestProxy_ActivityRecordedOnSuccess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	secret := makePasswordSecret("ws-1", "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	ws := makeWorkspaceCRD("ws-1", 5)
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	tracker := activity.NewActivityTracker(k8sMock, &testLogger{}, "default")
	handler.activityTracker = tracker

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/sessions", handler.ListSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/sessions", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, tracker.PendingCount(),
		"activity should be recorded (pending for flush) when proxy succeeds")
}

func TestProxy_OnSessionIdle_RecordsActivityWithoutWsConfig(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	tracker := activity.NewActivityTracker(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default")
	handler.activityTracker = tracker

	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	handler.onSessionIdle("ws-1", "s1")

	assert.Equal(t, 1, tracker.PendingCount(),
		"activity should be recorded on idle even without wsConfig entry (US-6.5 fix)")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"),
		"session should still be removed from active set")
}

// --- Epic 25 B2: mid-stream upstream read error → SSE error event ---

// midStreamResetTransport sends one chunk then injects a TCP RST-like error
// on the next read to simulate a pod crash mid-stream.
type midStreamResetTransport struct{}

func (t *midStreamResetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		// Write one valid SSE chunk, then inject an error simulating a pod crash.
		_, _ = pw.Write([]byte("data: {\"type\":\"session.started\"}\n\n"))
		pw.CloseWithError(fmt.Errorf("read tcp: connection reset by peer"))
	}()
	return resp, nil
}

// TestProxy_B2_MidStreamReadError_WritesSSEErrorEvent verifies that when the
// upstream pod closes the connection mid-stream (non-EOF error after the
// first bytes have been flushed), doProxy writes an SSE error event into the
// response body so the client can distinguish "pod died" from "clean end".
func TestProxy_B2_MidStreamReadError_WritesSSEErrorEvent(t *testing.T) {
	httpClient := &http.Client{Transport: &midStreamResetTransport{}, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/message", handler.SendMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	body := w.Body.String()
	// The response starts with 200 (committed on first chunk) and must contain
	// an SSE error event so the client can detect the upstream failure.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, "event: error", "response must contain SSE error event after mid-stream failure")
	assert.Contains(t, body, "upstream connection lost", "SSE error event must describe the failure")
}

// TestProxy_B2_CleanStreamEnd_NoSSEError verifies that normal stream
// completion for a non-SSE response (JSON REST) does NOT emit a spurious
// SSE error event. US-44.1 scope-limits the agent_died heuristic to
// text/event-stream responses, so JSON/REST passthrough is unaffected.
//
// The SSE false-positive case (clean SSE completion DOES emit agent_died
// per US-44.1 design) is asserted by
// TestProxy_US44_1_SSECleanClose_AcceptableFalsePositive in
// proxy_terminal_events_test.go.
func TestProxy_B2_CleanStreamEnd_NoSSEError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"method":"GET","path":"/session"}`))
		// Backend closes after writing — EOF on reader side after data.
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/message", handler.SendMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, body, "event: error",
		"non-SSE JSON stream completion must not emit SSE error event")
}

// --- Epic 25 B5: activity tracker map growth on NotFound ---

// TestActivityTracker_B5_NotFound_RemovesMapEntry verifies that when a
// workspace has been deleted (K8s returns NotFound on Get), flushOne removes
// the workspace from the activity map so it does not accumulate unbounded
// entries across workspace creates/deletes.
func TestActivityTracker_B5_NotFound_RemovesMapEntry(t *testing.T) {
	wsMock := k8smocks.NewMockWorkspaceInterface()
	tracker := newTestTracker(wsMock)

	notFoundErr := apierrors.NewNotFound(
		schema.GroupResource{Group: "llmsafespaces.dev", Resource: "workspaces"},
		"ws-deleted",
	)
	// US-23.3: tracker now calls Patch (not Get+UpdateStatus).
	wsMock.On("Patch", mock.Anything, "ws-deleted", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, notFoundErr)

	tracker.Record("ws-deleted")
	require.Equal(t, 1, tracker.PendingCount(), "entry must be present before flush")

	tracker.Flush()

	assert.Equal(t, 0, tracker.PendingCount(),
		"NotFound workspace must be removed from activity map so it does not grow unboundedly")
	wsMock.AssertNotCalled(t, "UpdateStatus")
}

// TestActivityTracker_B5_NotFound_DoesNotAffectOtherEntries verifies that
// purging a NotFound workspace only removes that workspace's entry, leaving
// other workspaces' entries intact.
func TestActivityTracker_B5_NotFound_DoesNotAffectOtherEntries(t *testing.T) {
	wsMock := k8smocks.NewMockWorkspaceInterface()
	tracker := newTestTracker(wsMock)

	existing := makeWorkspaceCRD("ws-alive", 5)
	notFoundErr := apierrors.NewNotFound(
		schema.GroupResource{Group: "llmsafespaces.dev", Resource: "workspaces"},
		"ws-deleted",
	)
	// US-23.3: tracker now calls Patch (not Get+UpdateStatus).
	wsMock.On("Patch", mock.Anything, "ws-deleted", mock.Anything, mock.Anything, mock.Anything).
		Return(nil, notFoundErr).Once()
	wsMock.On("Patch", mock.Anything, "ws-alive", mock.Anything, mock.Anything, mock.Anything).
		Return(existing, nil).Once()

	tracker.Record("ws-deleted")
	tracker.Record("ws-alive")

	tracker.Flush()

	// ws-deleted must be gone; ws-alive must remain.
	assert.Equal(t, 1, tracker.PendingCount(), "NotFound workspace must be removed, ws-alive must remain")
	wsMock.AssertNumberOfCalls(t, "Patch", 2)
}

// TestActivityTracker_B5_Delete_RemovesEntry verifies the Delete method
// removes a workspace entry from both activity and lastFlush maps.
func TestActivityTracker_B5_Delete_RemovesEntry(t *testing.T) {
	tracker := newTestTracker(k8smocks.NewMockWorkspaceInterface())

	tracker.Record("ws-1")
	require.Equal(t, 1, tracker.PendingCount())

	tracker.Delete("ws-1")

	assert.Equal(t, 0, tracker.PendingCount(), "Delete must remove the activity entry")
}

// TestProxy_B5_OnPhaseTerminated_DeletesActivityEntry verifies that when the
// workspace watcher delivers a Terminated phase event, the ProxyHandler
// removes the workspace from the activity tracker map so it does not accumulate
// unboundedly. (Epic 25 B5 — cleanup hook via onPhaseChange)
func TestProxy_B5_OnPhaseTerminated_DeletesActivityEntry(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	tracker := activity.NewActivityTracker(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default")
	handler.activityTracker = tracker

	// Pre-populate the tracker so there is an entry to delete.
	tracker.Record("ws-1")
	require.Equal(t, 1, tracker.PendingCount())

	sb := makeWorkspaceCRDWithStatus("ws-1", "", phaseTerminated, "ws-1")
	handler.onPhaseChange(sb)

	assert.Equal(t, 0, tracker.PendingCount(),
		"Terminated phase must remove workspace from activity tracker")
}

func makePasswordSecret(workspaceID, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("workspace-pw-%s", workspaceID),
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password": []byte(password),
		},
	}
}

func makeWorkspaceCRD(name string, maxActiveSessions int) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1.WorkspaceSpec{
			Owner:             v1.WorkspaceOwner{UserID: "user-1"},
			MaxActiveSessions: int32(maxActiveSessions),
		},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			PodIP: "10.0.0.1",
		},
	}
}

func makeWorkspaceCRDWithStatus(name, podIP, phase, _ string) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-1"},
			Runtime: "python:3.11",
		},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhase(phase),
			PodIP: podIP,
		},
	}
}

func newTestTracker(wsMock *k8smocks.MockWorkspaceInterface) *activity.ActivityTracker {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	return activity.NewActivityTracker(k8sMock, &testLogger{}, "default")
}

func TestProxy_DeleteSession_ProxiesDELETE(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestProxy_DeleteSession_EndpointMapping(t *testing.T) {
	var capturedPath string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/session/s1", capturedPath)
}

func TestProxy_DeleteSession_InvalidSessionID(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/bad..id", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProxy_DeleteSession_WorkspaceNotActive(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseSuspended), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestProxy_DeleteSession_BypassesActiveLimit(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 0)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code, "delete should bypass active session limit")
}

func TestProxy_DeleteSession_OpencodeNotFound(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found"})
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestProxy_DeleteSession_CleansUpSessionIndex(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, si.called, "sessionIndex.DeleteSession should have been called")
	assert.Equal(t, "ws-1", si.workspaceID)
	assert.Equal(t, "s1", si.sessionID)
}

func TestProxy_DeleteSession_IndexErrorStillReturns200(t *testing.T) {
	si := &failingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code, "should still return 200 even if index delete fails")
}

// ctxRecordingStore wraps an InMemoryStore to capture the context passed to
// MarkSessionDeleted, so the tombstone's detach-from-request contract can be
// asserted (regression guard: the tombstone must survive client disconnect).
type ctxRecordingStore struct {
	wsstate.Store
	mu          sync.Mutex
	recordedCtx context.Context
	called      bool
}

func (s *ctxRecordingStore) MarkSessionDeleted(ctx context.Context, workspaceID, sessionID string) {
	s.mu.Lock()
	s.recordedCtx = ctx
	s.called = true
	s.mu.Unlock()
	s.Store.MarkSessionDeleted(ctx, workspaceID, sessionID)
}

// TestProxy_DeleteSession_TombstoneUsesDetachedContext asserts the deleted-
// session tombstone is written with a context NOT derived from the request,
// so a client disconnect mid-request cannot cancel the tombstone write and
// reopen the zombie-session window the tombstone exists to close.
//
// Method: the request is dispatched with a sentinel-bearing context. If
// MarkSessionDeleted used c.Request.Context(), the recorded ctx would carry
// the sentinel; using context.Background() (the fix) it does not. (Comparing
// to context.Background() directly is vacuous — httptest.NewRequest already
// sets the request ctx to context.Background().)
func TestProxy_DeleteSession_TombstoneUsesDetachedContext(t *testing.T) {
	store := &ctxRecordingStore{Store: wsstate.NewInMemoryStore()}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetStateStore(store)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	type sentinelKey struct{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/ws-1/sessions/s1", nil).
		WithContext(context.WithValue(context.Background(), sentinelKey{}, "request"))
	env.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	store.mu.Lock()
	called := store.called
	recorded := store.recordedCtx
	store.mu.Unlock()
	require.True(t, called, "MarkSessionDeleted must be called")
	assert.Nil(t, recorded.Value(sentinelKey{}),
		"tombstone must use a detached context (not the request ctx) so it survives client disconnect")
}

type recordingDeleteSessionIndex struct {
	mu          sync.Mutex
	called      bool
	workspaceID string
	sessionID   string
}

func (r *recordingDeleteSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (r *recordingDeleteSessionIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (r *recordingDeleteSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error {
	return nil
}
func (r *recordingDeleteSessionIndex) DeleteSession(_ context.Context, workspaceID, sessionID string) error {
	r.mu.Lock()
	r.workspaceID = workspaceID
	r.sessionID = sessionID
	r.called = true
	r.mu.Unlock()
	return nil
}

func (r *recordingDeleteSessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error {
	return nil
}
func (r *recordingDeleteSessionIndex) UpsertTitle(_ context.Context, _, _, _ string) error {
	return nil
}
func (r *recordingDeleteSessionIndex) UpsertParent(_ context.Context, _, _, _ string) error {
	return nil
}
func (r *recordingDeleteSessionIndex) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (r *recordingDeleteSessionIndex) Start() error { return nil }
func (r *recordingDeleteSessionIndex) Stop() error  { return nil }

type failingDeleteSessionIndex struct{}

func (f *failingDeleteSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (f *failingDeleteSessionIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (f *failingDeleteSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error { return nil }
func (f *failingDeleteSessionIndex) DeleteSession(_ context.Context, _, _ string) error {
	return fmt.Errorf("db connection lost")
}
func (f *failingDeleteSessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error {
	return nil
}
func (f *failingDeleteSessionIndex) UpsertTitle(_ context.Context, _, _, _ string) error  { return nil }
func (f *failingDeleteSessionIndex) UpsertParent(_ context.Context, _, _, _ string) error { return nil }
func (f *failingDeleteSessionIndex) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (f *failingDeleteSessionIndex) Start() error { return nil }
func (f *failingDeleteSessionIndex) Stop() error  { return nil }

func TestProxy_DeleteSession_RemovesActiveSession(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetActiveSessionsForTest("ws-1", []string{"s1", "s2"})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Eventually(t, func() bool {
		return !env.handler.isSessionActive(context.Background(), "ws-1", "s1")
	}, 2*time.Second, 10*time.Millisecond, "deleted session should be removed from active sessions")

	assert.True(t, env.handler.isSessionActive(context.Background(), "ws-1", "s2"), "other sessions should be unaffected")
}

func TestProxy_DeleteSession_PublishesSSEEvent(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "deleted", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE session.status deleted event")
	}
}

func TestProxy_DeleteSession_NoSSEWhenOpencodeFails(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	assert.False(t, si.called, "session index should NOT be called when opencode fails")

	select {
	case evt := <-sub.Ch:
		t.Fatalf("unexpected SSE event when opencode fails: %+v", evt)
	default:
	}
}

func TestProxy_DeleteSession_ConcurrentDeletesIdempotent(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.handler.SetActiveSessionsForTest("ws-1", []string{"s1"})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	done := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
			done <- w
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case w := <-done:
			assert.Equal(t, http.StatusOK, w.Code)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent delete response")
		}
	}

	assert.Eventually(t, func() bool {
		return !env.handler.isSessionActive(context.Background(), "ws-1", "s1")
	}, 2*time.Second, 10*time.Millisecond, "session should be removed from active set after concurrent deletes")

	assert.Eventually(t, func() bool {
		return !env.handler.HasActiveWorkspaceForTest("ws-1")
	}, 2*time.Second, 10*time.Millisecond, "workspace entry should be cleaned up when no active sessions remain")
}

func TestProxy_DeleteSession_NoSideEffectsWithoutBroker(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = nil
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, si.called)
}

func TestProxy_DeleteSession_DeepNestingEndpointMapping(t *testing.T) {
	var capturedMethod, capturedPath string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/sess_abc-123", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "DELETE", capturedMethod)
	assert.Equal(t, "/session/sess_abc-123", capturedPath)
}

func TestProxy_DeleteSession_SuppressesLateSSEUpserts(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Delete the session
	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the session is marked as deleted
	assert.True(t, env.handler.isSessionDeleted("ws-1", "s1"))

	// Simulate a late idle event — onSessionIdle should NOT call RecordMessage
	env.handler.onSessionIdle("ws-1", "s1")

	si.mu.Lock()
	assert.Empty(t, si.recorded, "RecordMessage should not be called for deleted session")
	si.mu.Unlock()
}

func TestProxy_DeleteSession_AllowsNonDeletedSessionUpserts(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"deleted": true})
	})
	env.handler.SetSessionIndex(si)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	// Delete session s1
	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusOK, w.Code)

	// Late idle for a DIFFERENT session should still work
	env.handler.onSessionIdle("ws-1", "s2")

	si.mu.Lock()
	assert.Len(t, si.recorded, 1, "RecordMessage should be called for non-deleted session")
	si.mu.Unlock()
}

func TestProxy_PersistTitleFromEvent_SkipsDeletedSession(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	env.handler.SetSessionIndex(si)

	env.handler.MarkSessionDeletedForTest("ws-1", "s1")

	rawData := `{"properties":{"sessionID":"s1","info":{"id":"s1","title":"Late Title"}}}`
	env.handler.persistTitleFromEvent("ws-1", rawData)

	si.mu.Lock()
	assert.Empty(t, si.titleUpserts, "UpsertTitle should be skipped for deleted session")
	si.mu.Unlock()
}

func TestProxy_PersistContextFromEvent_SkipsDeletedSession(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	env.handler.SetSessionIndex(si)

	env.handler.MarkSessionDeletedForTest("ws-1", "s1")

	rawData := `{"properties":{"sessionID":"s1","tokens":{"input":100,"cache":{"read":0,"write":0}}}}`
	env.handler.persistContextFromEvent("ws-1", rawData)

	si.mu.Lock()
	assert.Empty(t, si.contextUpserts, "UpsertContextUsed should be skipped for deleted session")
	si.mu.Unlock()
}

var _ interfaces.SessionIndexService = (*recordingActivitySessionIndex)(nil)

type recordingActivitySessionIndex struct {
	mu             sync.Mutex
	recorded       []activityRecordCall
	titleUpserts   []upsertTitleCall
	parentUpserts  []upsertParentCall
	contextUpserts []upsertContextCall
	deleteCalled   bool
	deleteWID      string
	deleteSID      string
}

type upsertContextCall struct {
	workspaceID string
	sessionID   string
	contextUsed int64
}

type activityRecordCall struct {
	workspaceID string
	sessionID   string
	title       string
}

type upsertTitleCall struct {
	workspaceID string
	sessionID   string
	title       string
}

type upsertParentCall struct {
	workspaceID string
	sessionID   string
	parentID    string
}

func (r *recordingActivitySessionIndex) RecordMessage(workspaceID, sessionID, title string, _ time.Time) {
	r.mu.Lock()
	r.recorded = append(r.recorded, activityRecordCall{workspaceID, sessionID, title})
	r.mu.Unlock()
}
func (r *recordingActivitySessionIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (r *recordingActivitySessionIndex) DeleteByWorkspace(_ context.Context, _ string) error {
	return nil
}
func (r *recordingActivitySessionIndex) DeleteSession(_ context.Context, workspaceID, sessionID string) error {
	r.mu.Lock()
	r.deleteCalled = true
	r.deleteWID = workspaceID
	r.deleteSID = sessionID
	r.mu.Unlock()
	return nil
}
func (r *recordingActivitySessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error {
	return nil
}
func (r *recordingActivitySessionIndex) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	r.mu.Lock()
	r.titleUpserts = append(r.titleUpserts, upsertTitleCall{workspaceID, sessionID, title})
	r.mu.Unlock()
	return nil
}
func (r *recordingActivitySessionIndex) UpsertParent(_ context.Context, workspaceID, sessionID, parentID string) error {
	r.mu.Lock()
	r.parentUpserts = append(r.parentUpserts, upsertParentCall{workspaceID, sessionID, parentID})
	r.mu.Unlock()
	return nil
}
func (r *recordingActivitySessionIndex) UpsertContextUsed(_ context.Context, workspaceID, sessionID string, contextUsed int64) error {
	r.mu.Lock()
	r.contextUpserts = append(r.contextUpserts, upsertContextCall{workspaceID, sessionID, contextUsed})
	r.mu.Unlock()
	return nil
}
func (r *recordingActivitySessionIndex) Start() error { return nil }
func (r *recordingActivitySessionIndex) Stop() error  { return nil }

func TestProxy_OnSessionIdle_RecordsSessionIndexWithoutWsConfig(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus("ws-1", "", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil).Maybe()
	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)

	tracker := activity.NewActivityTracker(k8sMock, &testLogger{}, "default")
	handler.activityTracker = tracker
	si := &recordingActivitySessionIndex{}
	handler.sessionIndex = si

	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	handler.onSessionIdle("ws-1", "s1")

	si.mu.Lock()
	assert.Len(t, si.recorded, 1, "session index RecordMessage should be called once")
	if len(si.recorded) > 0 {
		assert.Equal(t, "ws-1", si.recorded[0].workspaceID)
		assert.Equal(t, "s1", si.recorded[0].sessionID)
	}
	si.mu.Unlock()

	assert.Equal(t, 1, tracker.PendingCount(), "activity tracker should record activity")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"), "session should be removed from active set")
}

func TestProxy_OnSessionIdle_FetchAndPersistTitleWithoutWsConfig(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/s1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"title": "Test Session", "parentID": "p1"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())

	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	secret := makePasswordSecret("ws-1", "test-password")
	_, err = k8sMock.Clientset().CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	tracker := activity.NewActivityTracker(k8sMock, &testLogger{}, "default")
	handler.activityTracker = tracker
	si := &recordingActivitySessionIndex{}
	handler.sessionIndex = si

	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	handler.onSessionIdle("ws-1", "s1")

	assert.Eventually(t, func() bool {
		si.mu.Lock()
		defer si.mu.Unlock()
		return len(si.titleUpserts) > 0
	}, 2*time.Second, 10*time.Millisecond, "fetchAndPersistTitle should upsert title")

	si.mu.Lock()
	assert.Len(t, si.titleUpserts, 1)
	if len(si.titleUpserts) > 0 {
		assert.Equal(t, "ws-1", si.titleUpserts[0].workspaceID)
		assert.Equal(t, "s1", si.titleUpserts[0].sessionID)
		assert.Equal(t, "Test Session", si.titleUpserts[0].title)
	}
	si.mu.Unlock()
}

func TestProxy_IsSessionActive_ReturnsFalseForUnknownWorkspace(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	assert.False(t, handler.isSessionActive(context.Background(), "unknown-ws", "s1"),
		"isSessionActive should return false for unknown workspace")
}

func TestProxy_IsSessionActive_ReturnsTrueForActiveSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetActiveSessionsForTest("ws-1", []string{"s1", "s2"})

	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "s1"), "s1 should be active")
	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "s2"), "s2 should be active")
	assert.False(t, handler.isSessionActive(context.Background(), "ws-1", "s3"), "s3 should not be active")
}

func TestProxy_SendPromptAsync_409DoesNotAffectSendMessage(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", body)

	assert.NotEqual(t, http.StatusConflict, w.Code,
		"SendMessage (synchronous) should NOT get 409 guard")
}

func TestProxy_OnSessionIdle_SessionIndexIndependentOfActivityTracker(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus("ws-1", "", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil).Maybe()

	handler, _ := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)

	si := &recordingActivitySessionIndex{}
	handler.sessionIndex = si

	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	handler.onSessionIdle("ws-1", "s1")

	si.mu.Lock()
	assert.Len(t, si.recorded, 1, "sessionIndex.RecordMessage must fire even when activityTracker is nil")
	si.mu.Unlock()
}

func TestProxy_ProxyToWorkspace_NoDoubleReleaseOnMaxSessions(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	ws := makeWorkspaceCRD("ws-1", 1)
	env.wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(ws, nil).Maybe()
	env.setupPasswordWithT(t, "ws-1", "test-password")

	env.handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	env.handler.connMu.Lock()
	env.handler.connCount["ws-1"] = 5
	env.handler.connMu.Unlock()

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s2/message", strings.NewReader(`{"msg":"hi"}`))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	env.handler.connMu.Lock()
	count := env.handler.connCount["ws-1"]
	env.handler.connMu.Unlock()
	assert.Equal(t, 5, count, "connection count should be 5 (acquire 5→6, defer release 6→5), not underflowed to 4 by double-release")
}

func TestProxy_IsSessionActive_ConcurrentReads(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	handler.SetActiveSessionsForTest("ws-1", []string{"s1"})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "s1"))
		}()
	}
	wg.Wait()
}

func TestProxy_OnPhaseChange_RecordsLifecycleEvent(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	meteringSvc := new(mocks.MockMeteringService)
	handler.SetMeteringService(meteringSvc)

	// Set prior phase to simulate a real Creating→Active transition.
	handler.SetPriorPhaseForTest("ws-1", "Creating")

	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "user-1")
	ws.Spec.SecurityLevel = "standard"

	meteringSvc.On("RecordLifecycleEvent",
		mock.Anything,
		"ws-1",
		"user-1",
		types.OwnerTypeUser,
		"Creating",
		string(v1.WorkspacePhaseActive),
		"standard",
		mock.AnythingOfType("time.Time"),
	).Return(nil)

	handler.onPhaseChange(ws)

	meteringSvc.AssertCalled(t, "RecordLifecycleEvent",
		mock.Anything,
		"ws-1",
		"user-1",
		types.OwnerTypeUser,
		"Creating",
		string(v1.WorkspacePhaseActive),
		"standard",
		mock.AnythingOfType("time.Time"),
	)
}

// TestProxy_OnPhaseChange_CreatingToActive_AfterRestart_RecordsLifecycleEvent verifies that
// when the API restarts with a Creating workspace, and that workspace later transitions to Active,
// the lifecycle event IS recorded — even though prior=="" in the handler's priorPhase map
// (because seedResourceVersion only calls onPhaseChange for Active workspaces).
// Regression test for the billing regression introduced by the `prior != ""` guard that was
// present in an earlier version of this fix and removed.
func TestProxy_OnPhaseChange_CreatingToActive_AfterRestart_RecordsLifecycleEvent(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	meteringSvc := new(mocks.MockMeteringService)
	handler.SetMeteringService(meteringSvc)

	// Simulate API restart: workspace is Creating → priorPhase never set in handler.
	// Then a Creating→Active transition arrives from the watcher.
	ws := makeWorkspaceCRDWithStatus("ws-restart", "10.0.0.1", string(v1.WorkspacePhaseActive), "")
	ws.Spec.Owner.UserID = "user-restart"
	ws.Spec.SecurityLevel = "standard"

	meteringSvc.On("RecordLifecycleEvent",
		mock.Anything,
		"ws-restart",
		"user-restart",
		types.OwnerTypeUser,
		"", // prior is "" because this is the first handler invocation
		string(v1.WorkspacePhaseActive),
		"standard",
		mock.AnythingOfType("time.Time"),
	).Return(nil)

	handler.onPhaseChange(ws)

	meteringSvc.AssertCalled(t, "RecordLifecycleEvent",
		mock.Anything,
		"ws-restart",
		"user-restart",
		types.OwnerTypeUser,
		"",
		string(v1.WorkspacePhaseActive),
		"standard",
		mock.AnythingOfType("time.Time"),
	)
}

func TestProxy_OnPhaseChange_NoMeteringService_NoPanic(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, nil)

	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "user-1")

	assert.NotPanics(t, func() {
		handler.onPhaseChange(ws)
	})
}

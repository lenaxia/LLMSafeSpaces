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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
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
	log       pkginterfaces.LoggerInterface
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
	return newTestEnvWithBackendAndLogger(t, backendHandler, &testLogger{})
}

func newTestEnvWithBackendAndLogger(t *testing.T, backendHandler http.HandlerFunc, log pkginterfaces.LoggerInterface) *testEnv {
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

	handler, err := NewProxyHandler(k8sMock, log, "default", httpClient, newLenientMockAdapter())
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
		proxy.GET("/alerts", handler.GetWorkspaceAlerts)
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

// TestProxy_ProxiesGETRequest was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_ProxiesPOSTRequest was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_SendsBasicAuth was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_ForwardsQueryParameters was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

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

// TestProxy_RetriesOnStaleIP was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_ConnectionFailureReturns503 was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_WorkspaceNotRunning was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_WorkspaceNotFound was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_PasswordCachedAfterFirstRead was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_SecretNotFound was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_EmptyPasswordKey was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_ActiveSessionLimit was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_AlreadyActiveSessionSucceeds was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_ReadOnlyBypassesSessionLimit was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_CreateSessionBypassesLimit was ported to the adapter path
// (TestCreateSession_AdapterPath_BypassesActiveSessionLimit in
// proxy_batch1_migration_test.go) when CreateSession became adapter-only
// (#828 batch 1); the transport-level row died with the legacy branch.

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

// TestProxy_ConnectionCeiling_Returns429 was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_EndpointMapping was deleted with the raw-proxy transport (#828 final batch — the legacy seams are gone)

// TestProxy_E2E_FullFlow was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_E2E_MultipleWorkspaceIsolation was deleted with the transport (#828 final batch): workspace-scoped session accounting is pinned on the adapter paths

// TestProxy_WorkspaceNotFound_UsesDefaults was deleted with the transport default-limit arms (#828 final batch)

// TestProxy_BackendErrorPassthrough was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_Backend404Passthrough was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

func TestProxy_CacheInvalidation(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	handler.SetCachedPasswordForTest("ws-1", "password")

	// Set prior phase to Active so this is a no-op Active→Active reconcile event.
	// The seed call (prior=="") intentionally invalidates caches and restarts SSE subscriptions.
	handler.SetPriorPhaseForTest("ws-1", string(phaseActive))

	sb := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(phaseActive), "ws-1")
	handler.onPhaseChange(sb)

	_, pwOk := handler.GetCachedPasswordForTest("ws-1")
	assert.True(t, pwOk, "phase change to Running should NOT invalidate cache")
}

// TestProxy_ConcurrentRequests was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_E2E_MaxActiveSessionsCustom was deleted with the transport write-op arms (#828 final batch): session limits are enforced by checkAdapterSessionLimit on the adapter paths (pinned in the batch-2/3 rows)

func TestProxy_RemoveActiveSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	handler.SetActiveSessionsForTest("ws-1", []string{"s1", "s2"})

	handler.removeActiveSession(context.Background(), "ws-1", "s1")
	assert.Equal(t, 1, handler.activeSessionCount(context.Background(), "ws-1"))

	handler.removeActiveSession(context.Background(), "ws-1", "s2")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "ws-1"))

	assert.False(t, handler.HasActiveWorkspaceForTest("ws-1"), "empty session set should be cleaned up")
}

func TestProxy_RemoveNonexistentSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	handler.removeActiveSession(context.Background(), "sb-missing", "s1")
	assert.Equal(t, 0, handler.activeSessionCount(context.Background(), "sb-missing"))
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
			_, err := NewProxyHandler(tt.k8sClient, tt.logger, "default", nil, newLenientMockAdapter())
			if tt.expectErr != "" {
				assert.EqualError(t, err, tt.expectErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestProxy_DefaultNamespace(t *testing.T) {
	h, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "", nil, newLenientMockAdapter())
	require.NoError(t, err)
	assert.Equal(t, "default", h.namespace)
}

func TestProxy_CustomHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 10 * time.Second}
	h, err := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "ns", custom, newLenientMockAdapter())
	require.NoError(t, err)
	assert.Equal(t, custom, h.httpClient)
}

func TestProxy_ConnectionCountTracking(t *testing.T) {
	h, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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

// US-69.11: the SSE-driven lifecycle tests (tracker subscription
// arming/reset/teardown, onSessionActive/onSessionIdle callbacks) were
// deleted with the tracker. Surviving intents live elsewhere:
//   - session limits + stale-busy clearing via statusz —
//     authoritative_status_test.go, proxy_session_status_test.go
//   - gate arming/reset/teardown on phase transitions (#902) —
//     proxy_auth_cache_test.go, proxy_902_e2e_test.go
//   - phase-change cache preservation —
//     TestProxy_OnPhaseChange_SecondActiveNoManualSeed_PreservesState below.

// TestProxy_SessionLeak_NotOnConnectionCeilingReject was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_SessionLeak_CleanedUpOn503 was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// --- statuszPodIP (US-69.11: the tracker's pod-IP resolver, kept for
// the D6 sweep and the statusz reconcile pass) ---

func TestProxy_StatuszPodIP_RunningReturnsIP(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	ip := handler.statuszPodIP(context.Background(), "ws-1")
	assert.Equal(t, "10.0.0.1", ip)
}

func TestProxy_StatuszPodIP_SuspendedReturnsEmpty(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	crd := makeWorkspaceCRDWithStatus("ws-1", "", "Suspended", "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	ip := handler.statuszPodIP(context.Background(), "ws-1")
	assert.Equal(t, "", ip)
}

func TestProxy_StatuszPodIP_NotFoundReturnsEmpty(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)

	wsMock.On("Get", mock.Anything, "sb-missing", metav1.GetOptions{}).Return(nil, fmt.Errorf("not found")).Once()

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	ip := handler.statuszPodIP(context.Background(), "sb-missing")
	assert.Equal(t, "", ip)
}

// US-69.11: the OnPhaseChange tracker-lifecycle tests (SuspendingStops,
// RunningKeeps, CreatingToActive reset, ActiveToActive no-reset, seed
// arming) were deleted with the tracker. The equivalent gate semantics
// (arm on every Active event, fresh gate on transition, teardown on
// suspend, reconciler healing) are pinned in proxy_auth_cache_test.go
// and proxy_902_e2e_test.go.

// US-69.11: TestProxy_OnPhaseChange_SecondActiveNoManualSeed_PreservesState
// (the US-45.1 regression) remains below — it pins phase-change cache
// semantics that survived the tracker retirement.

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
	t.Cleanup(stubUsageStream())
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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

// TestProxy_ActivityNotRecordedOnProxyFailure was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

func TestProxy_ActivityRecordedOnSuccess(t *testing.T) {
	// Ported to the adapter path (#828 batch 2): reads record activity
	// via recordActivityIfTracked, not the legacy transport.
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.handler.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "s1"}, nil
		},
	}

	tracker := activity.NewActivityTracker(env.k8sMock, &testLogger{}, "default")
	env.handler.activityTracker = tracker

	w := env.doRequestWithT(t, "GET", "/api/v1/workspaces/ws-1/sessions/s1", nil)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, tracker.PendingCount(),
		"activity should be recorded (pending for flush) on adapter-path reads")
}

// US-69.11: TestProxy_OnSessionIdle_RecordsActivityWithoutWsConfig was
// deleted with onSessionIdle — activity recording now happens on the
// proxy request paths (TestProxy_ActivityRecordedOnSuccess above) and
// idle transitions arrive via the usage bridge
// (proxy_usagestream_test.go).

// --- Epic 25 B2: mid-stream upstream read error → SSE error event ---

// TestProxy_B2_MidStreamReadError_WritesSSEErrorEvent was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

// TestProxy_B2_CleanStreamEnd_NoSSEError was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

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
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// TestProxy_DeleteSession_EndpointMapping was deleted with the raw-proxy transport (#828 final batch — the legacy seams are gone)

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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code, "delete should bypass active session limit")
}

func TestProxy_DeleteSession_AdapterError_Returns502(t *testing.T) {
	// Port of TestProxy_DeleteSession_OpencodeNotFound: the legacy 404
	// passthrough died with the transport tail; adapter deletes surface
	// failures as a typed 502 and run NO side effects.
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("session not found")
		},
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.False(t, env.handler.isSessionDeleted("ws-1", "s1"),
		"a failed adapter delete must not tombstone")
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code, "should still return 200 even if index delete fails")
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	type sentinelKey struct{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/ws-1/sessions/s1", nil).
		WithContext(context.WithValue(context.Background(), sentinelKey{}, "request"))
	env.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)

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

func (r *recordingDeleteSessionIndex) RebuildMessageCount(_ context.Context, _, _ string, _ int) error {
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
func (f *failingDeleteSessionIndex) RebuildMessageCount(_ context.Context, _, _ string, _ int) error {
	return nil
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "deleted", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE session.status deleted event")
	}
}

func TestProxy_DeleteSession_NoSSEWhenAdapterFails(t *testing.T) {
	si := &recordingDeleteSessionIndex{}
	env := newTestEnv(t)
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("upstream 500")
		},
	}

	// Subscribe BEFORE the request (same pattern as the success row) so a
	// wrongly-published event would surface on the live stream.
	sub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)

	select {
	case evt := <-sub.Ch:
		t.Fatalf("failed delete must not publish SSE, got %+v", evt)
	case <-time.After(200 * time.Millisecond):
	}
	assert.False(t, si.called, "failed delete must not clean the session index")
	assert.False(t, env.handler.isSessionDeleted("ws-1", "s1"), "failed delete must not tombstone")
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

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
			assert.Equal(t, http.StatusNoContent, w.Code)
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
	env.handler.adapter = &mockAdapter{
		deleteSessionFn: func(_ context.Context, _, _, _ string) error { return nil },
	}

	w := env.doRequestWithT(t, "DELETE", "/api/v1/workspaces/ws-1/sessions/s1", nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, si.called)
}

// TestProxy_DeleteSession_DeepNestingEndpointMapping was deleted with the raw-proxy transport (#828 final batch — the legacy seams are gone)

// US-69.11: title/context persistence for deleted sessions moved from
// the tracker's dialect callbacks to the usage bridge — the tombstone
// guard (no zombie session_index rows from late events) survives on the
// new seam.

// US-69.11: TestProxy_DeleteSession_SuppressesLateSSEUpserts /
// AllowsNonDeletedSessionUpserts were deleted with onSessionIdle — the
// tombstone-vs-live-session split is pinned by the two bridge tests
// below plus TestBridgeContextUsed_Persists (opencode_upgrade_test.go).

func TestProxy_BridgeSessionTitle_SkipsDeletedSession(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	env.handler.SetSessionIndex(si)
	t.Cleanup(stubUsageStream())

	env.handler.MarkSessionDeletedForTest("ws-1", "s1")

	(&usageBridge{h: env.handler}).SessionTitle("ws-1", "s1", "Late Title")

	si.mu.Lock()
	assert.Empty(t, si.titleUpserts, "UpsertTitle should be skipped for deleted session")
	si.mu.Unlock()
}

func TestProxy_BridgeContextUsed_SkipsDeletedSession(t *testing.T) {
	si := &recordingActivitySessionIndex{}
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {})
	env.handler.SetSessionIndex(si)
	t.Cleanup(stubUsageStream())

	env.handler.MarkSessionDeletedForTest("ws-1", "s1")

	(&usageBridge{h: env.handler}).ContextUsed("ws-1", "s1", 100)

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
func (r *recordingActivitySessionIndex) RebuildMessageCount(_ context.Context, _, _ string, _ int) error {
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

// US-69.11: the three TestProxy_OnSessionIdle_* tests were deleted with
// onSessionIdle — idle-side activity recording, RecordMessage, and the
// idle title fetch were tracker-callback behaviors. Title and context
// persistence now flow from the usage bridge (proxy_usagestream_test.go
// and the bridge tests above); activity recording from the request
// paths (TestProxy_ActivityRecordedOnSuccess).

func TestProxy_IsSessionActive_ReturnsFalseForUnknownWorkspace(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	assert.False(t, handler.isSessionActive(context.Background(), "unknown-ws", "s1"),
		"isSessionActive should return false for unknown workspace")
}

func TestProxy_IsSessionActive_ReturnsTrueForActiveSession(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	handler.SetActiveSessionsForTest("ws-1", []string{"s1", "s2"})

	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "s1"), "s1 should be active")
	assert.True(t, handler.isSessionActive(context.Background(), "ws-1", "s2"), "s2 should be active")
	assert.False(t, handler.isSessionActive(context.Background(), "ws-1", "s3"), "s3 should not be active")
}

// Ported to the adapter path (#828 batch 1): the synchronous /message
// route takes no busy/409 guard — its only admission control is the
// active-session ceiling (429). A session already active and within
// limits must send successfully, never 409.
func TestProxy_SendPromptAsync_409DoesNotAffectSendMessage(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.SetActiveSessionsForTest("ws-1", []string{"s1"})
	env.handler.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, _ string, _ session.SendOpts) (*session.Message, error) {
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/s1/message", body)

	assert.NotEqual(t, http.StatusConflict, w.Code,
		"SendMessage (synchronous) should NOT get 409 guard")
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestProxy_ProxyToWorkspace_NoDoubleReleaseOnMaxSessions was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).

func TestProxy_IsSessionActive_ConcurrentReads(t *testing.T) {
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	t.Cleanup(stubUsageStream())
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	t.Cleanup(stubUsageStream())
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

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
	t.Cleanup(stubUsageStream())
	handler, _ := NewProxyHandler(k8smocks.NewMockKubernetesClient(), &testLogger{}, "default", nil, newLenientMockAdapter())

	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "user-1")

	assert.NotPanics(t, func() {
		handler.onPhaseChange(ws)
	})
}

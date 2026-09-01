package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- SendMessage adapter cross-cutting tests ---

func setupAdapterSendMessageEnv(t *testing.T, wsID string, maxSessions int32) (*testEnv, *mocks.MockMeteringService) {
	t.Helper()
	env := newTestEnv(t)

	env.wsMock.On("Get", mock.Anything, wsID, mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: wsID, Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:             v1.WorkspaceOwner{UserID: "user-1"},
			MaxActiveSessions: maxSessions,
		},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhaseActive,
			PodIP:   "10.0.0.1",
			PodName: "test-pod",
		},
	}, nil)
	env.setupPasswordWithT(t, wsID, "test-password")

	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)
	return env, ms
}

func TestAdapterPath_SendMessage_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			t.Fatal("adapter.Send must NOT be called when workspace is not ready")
			return nil, nil
		},
	}

	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-notready/sessions/ses_1/message", body)
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace not ready")
}

func TestAdapterPath_SendMessage_QuotaExceeded_Returns429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-quota", 5)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			t.Fatal("adapter.Send must NOT be called when quota is exceeded")
			return nil, nil
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-quota/sessions/ses_1/message", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-quota"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendMessage(c)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestAdapterPath_SendMessage_HappyPath_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-happy", 5)

	sendCalled := int32(0)
	env.handler.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, _ session.SendOpts) (*session.Message, error) {
			atomic.StoreInt32(&sendCalled, 1)
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	ms.On("Record", mock.Anything).Return()
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-happy/sessions/ses_1/message", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-happy"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendMessage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int32(1), atomic.LoadInt32(&sendCalled), "adapter.Send must be called")
	ms.AssertCalled(t, "Record", mock.Anything)
}

func TestAdapterPath_SendMessage_AdapterError_CleansActiveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-err", 5)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			return nil, errors.New("upstream error")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	wasActive := env.handler.checkAndAddActiveSession(context.Background(), "ws-err", "ses_cleanup", 5)
	assert.True(t, wasActive, "session must be active before the request")

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-err/sessions/ses_cleanup/message", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-err"}, {Key: "sessionId", Value: "ses_cleanup"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendMessage(c)

	assert.Equal(t, http.StatusBadGateway, w.Code)

	stillActive := env.handler.isSessionActive(context.Background(), "ws-err", "ses_cleanup")
	assert.False(t, stillActive, "removeActiveSession must have been called on adapter error")
}

// --- SendPromptAsync adapter cross-cutting tests ---

func TestAdapterPath_SendPromptAsync_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.handler.adapter = &mockAdapter{
		sendAsyncFn: func(context.Context, string, string, string, string, session.SendOpts) (string, error) {
			t.Fatal("adapter.SendAsync must NOT be called when workspace is not ready")
			return "", nil
		},
	}
	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-notready/sessions/ses_1/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace not ready")
}

func TestAdapterPath_SendPromptAsync_QuotaExceeded_Returns429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-quota-async", 5)

	env.handler.adapter = &mockAdapter{
		sendAsyncFn: func(context.Context, string, string, string, string, session.SendOpts) (string, error) {
			t.Fatal("adapter.SendAsync must NOT be called when quota is exceeded")
			return "", nil
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-quota-async/sessions/ses_1/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-quota-async"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestAdapterPath_SendPromptAsync_HappyPath_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-async-happy", 5)

	sendCalled := int32(0)
	env.handler.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			atomic.StoreInt32(&sendCalled, 1)
			return &session.Message{ID: "msg_123", Type: session.MessageAssistant}, nil
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	ms.On("Record", mock.Anything).Return()
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-async-happy/sessions/ses_1/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-async-happy"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusOK, w.Code, "SendPromptAsync now uses synchronous Send (#755)")
	assert.Equal(t, int32(1), atomic.LoadInt32(&sendCalled), "adapter.Send must be called")
	ms.AssertCalled(t, "Record", mock.Anything)
}

// --- #944: typed disk-full failure ---

// TestSendPromptAsync_DiskFull_ReturnsTyped507: a send failure on a
// workspace whose CRD reports disk usage at/above the critical
// threshold surfaces as 507 {"code":"disk_full",...} with the usage
// numbers, not the generic 502 the UI rendered as a bare "Failed to
// fetch".
func TestSendPromptAsync_DiskFull_ReturnsTyped507(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-diskfull", 5)

	// Replace the env default (no-disk) CRD expectation — testify
	// serves the first matching registration, so Unset first.
	env.wsMock.On("Get", mock.Anything, "ws-diskfull", mock.Anything).Unset()

	// 100% full — the incident's exact shape.
	env.setupWorkspaceWithDiskT(t, "ws-diskfull", 15_727_534_080, 15_744_311_296)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			return nil, errors.New("upstream 500: UnknownError")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-diskfull/sessions/ses_1/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-diskfull"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusInsufficientStorage, w.Code)
	var resp struct {
		Code          string `json:"code"`
		DiskUsedBytes int64  `json:"diskUsedBytes"`
		DiskTotal     int64  `json:"diskTotalBytes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "disk_full", resp.Code)
	assert.Equal(t, int64(15_727_534_080), resp.DiskUsedBytes)
	assert.Equal(t, int64(15_744_311_296), resp.DiskTotal)
}

// TestSendPromptAsync_DiskNotFull_KeepsGeneric502: below the critical
// threshold, send failures keep the generic 502 — unrelated
// provider/pod errors must not be mislabeled as disk_full.
func TestSendPromptAsync_DiskNotFull_KeepsGeneric502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-diskok", 5)

	env.wsMock.On("Get", mock.Anything, "ws-diskok", mock.Anything).Unset()
	env.setupWorkspaceWithDiskT(t, "ws-diskok", 50_000_000, 100_000_000) // 50%

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			return nil, errors.New("upstream 500: UnknownError")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-diskok/sessions/ses_1/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-diskok"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "disk_full")
}

func TestAdapterPath_SendPromptAsync_AdapterError_CleansActiveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-async-err", 5)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			return nil, errors.New("upstream not found")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	wasActive := env.handler.checkAndAddActiveSession(context.Background(), "ws-async-err", "ses_cleanup", 5)
	assert.True(t, wasActive)

	body := strings.NewReader(`{"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-async-err/sessions/ses_cleanup/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-async-err"}, {Key: "sessionId", Value: "ses_cleanup"}}
	c.Request = req
	c.Set("userID", "user-1")

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusBadGateway, w.Code,
		"adapter Send error must produce 502")

	stillActive := env.handler.isSessionActive(context.Background(), "ws-async-err", "ses_cleanup")
	assert.False(t, stillActive, "removeActiveSession must have been called on adapter error")
}

// --- SSE watch regression tests (#755 stuck-busy root cause) ---
//
// US-69.11: the read-path SSE-watch trigger tests were deleted with the
// tracker — read paths now arm the busy-gated usage stream
// (adapterEnsureSSEWatch's replacement), covered by the gate-arming
// tests in proxy_902_e2e_test.go and proxy_auth_cache_test.go.

// TestAdapterPath_SendPromptAsync_PolicyDenied_ReleasesSessionSlot pins
// round-3 finding 4: the 403 policy denial must release the active-session
// slot that checkAdapterSessionLimit reserved — the quota and adapter-error
// paths already did; a leaked slot would count against MaxActiveSessions
// with no active send.
func TestAdapterPath_SendPromptAsync_PolicyDenied_ReleasesSessionSlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-pol-403", 5)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			t.Error("denied override must not reach the adapter")
			return nil, nil
		},
	}
	env.handler.SetModelPolicyChecker(&mockPolicyChecker{policy: &types.OrgPolicyValues{
		AllowedModels: &[]string{"glm-5.3"},
	}})

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	env.handler.activityTracker = newTestTracker(env.wsMock)

	wasActive := env.handler.checkAndAddActiveSession(context.Background(), "ws-pol-403", "ses_cleanup", 5)
	assert.True(t, wasActive)

	body := strings.NewReader(`{"model":{"modelID":"gpt-5.5","providerID":"openai"},"parts":[{"type":"text","text":"hello"}]}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-pol-403/sessions/ses_cleanup/prompt", body)
	req.Header.Set("Content-Type", "application/json")
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-pol-403"}, {Key: "sessionId", Value: "ses_cleanup"}}
	c.Request = req
	c.Set("userID", "user-1")
	c.Set("workspace", &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-pol-403", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1", OrgID: "org-1"}, MaxActiveSessions: 5},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1", PodName: "p"},
	})

	env.handler.SendPromptAsync(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	stillActive := env.handler.isSessionActive(context.Background(), "ws-pol-403", "ses_cleanup")
	assert.False(t, stillActive, "403 policy denial must release the reserved session slot")
}

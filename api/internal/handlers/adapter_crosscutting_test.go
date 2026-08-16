package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(false, int64(0), nil)
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

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(true, int64(10), nil)
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
		// #817 recovery probe unavailable → falls through to the 502
		// path so the active-session cleanup still runs.
		getSessionFn: func(context.Context, string, string, string) (*session.Session, error) {
			return nil, errors.New("probe failed")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(true, int64(10), nil)
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

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(false, int64(0), nil)
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

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(true, int64(10), nil)
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

func TestAdapterPath_SendPromptAsync_AdapterError_CleansActiveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env, ms := setupAdapterSendMessageEnv(t, "ws-async-err", 5)

	env.handler.adapter = &mockAdapter{
		sendFn: func(context.Context, string, string, string, string, session.SendOpts) (*session.Message, error) {
			return nil, errors.New("upstream not found")
		},
		// #817 recovery probe unavailable → falls through to the 502
		// path so the active-session cleanup still runs.
		getSessionFn: func(context.Context, string, string, string) (*session.Session, error) {
			return nil, errors.New("probe failed")
		},
	}

	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_request").Return(true, int64(10), nil)
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
// The adapter read paths (GetHistory, GetSession, ListSessions) must
// call adapterEnsureSSEWatch so the SSE tracker starts watching the
// workspace. Without this, opening a busy session never receives the
// session.status=idle event when the LLM finishes, and the session
// appears stuck busy forever.

func TestAdapterPath_GetHistory_TriggersSSEWatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.handler.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{}, nil
		},
	}

	// Verify SSE watch is triggered by checking the tracker's internal
	// subscriptions map after the call.
	env.handler.sseTracker = sse.NewTracker(&http.Client{Timeout: 5 * time.Second}, &testLogger{}, nil)
	env.handler.sseTracker.SetPasswordGetter(env.handler)
	env.handler.sseTracker.SetPodIPResolver(func(string) string { return "" })

	env.wsMock.On("Get", mock.Anything, "ws-ready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-ready", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhaseActive,
			PodIP:   "10.0.0.1",
			PodName: "test-pod",
		},
	}, nil)
	env.setupPasswordWithT(t, "ws-ready", "test-password")

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-ready/sessions/ses_1/message", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, env.handler.sseTracker.IsWatching("ws-ready"),
		"GetHistory must trigger SSE watch")
}

func TestAdapterPath_GetSession_TriggersSSEWatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.handler.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_1"}, nil
		},
	}

	env.handler.sseTracker = sse.NewTracker(&http.Client{Timeout: 5 * time.Second}, &testLogger{}, nil)
	env.handler.sseTracker.SetPasswordGetter(env.handler)
	env.handler.sseTracker.SetPodIPResolver(func(string) string { return "" })

	env.wsMock.On("Get", mock.Anything, "ws-ready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-ready", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhaseActive,
			PodIP:   "10.0.0.1",
			PodName: "test-pod",
		},
	}, nil)
	env.setupPasswordWithT(t, "ws-ready", "test-password")

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-ready/sessions/ses_1", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.True(t, env.handler.sseTracker.IsWatching("ws-ready"),
		"GetSession must trigger SSE watch")
}

func TestAdapterPath_ListSessions_TriggersSSEWatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)

	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{}, nil
		},
	}

	env.handler.sseTracker = sse.NewTracker(&http.Client{Timeout: 5 * time.Second}, &testLogger{}, nil)
	env.handler.sseTracker.SetPasswordGetter(env.handler)
	env.handler.sseTracker.SetPodIPResolver(func(string) string { return "" })

	env.wsMock.On("Get", mock.Anything, "ws-ready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-ready", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhaseActive,
			PodIP:   "10.0.0.1",
			PodName: "test-pod",
		},
	}, nil)
	env.setupPasswordWithT(t, "ws-ready", "test-password")

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-ready/sessions", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.True(t, env.handler.sseTracker.IsWatching("ws-ready"),
		"ListSessions must trigger SSE watch")
}

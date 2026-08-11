package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAdapterPath_GetHistory_WorkspaceNotReady_Returns503 verifies
// that the adapter path now checks workspace readiness before calling
// the adapter, returning 503 + Retry-After when the workspace is not
// Active. Previously, the adapter path bypassed this check entirely.
func TestAdapterPath_GetHistory_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.adapter = &mockAdapter{
		getHistoryFn: func(context.Context, string, string, string) ([]session.Message, error) {
			t.Fatal("adapter.GetHistory must NOT be called when workspace is not ready")
			return nil, nil
		},
	}

	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-notready/sessions/ses_1/message", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace not ready")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

// TestAdapterPath_GetSession_WorkspaceNotReady_Returns503 verifies
// the same readiness check applies to GetSession.
func TestAdapterPath_GetSession_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.adapter = &mockAdapter{
		getSessionFn: func(context.Context, string, string, string) (*session.Session, error) {
			t.Fatal("adapter.GetSession must NOT be called when workspace is not ready")
			return nil, nil
		},
	}

	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-notready/sessions/ses_1", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "workspace not ready")
}

// TestAdapterPath_ListSessions_WorkspaceNotReady_Returns503 verifies
// the same readiness check applies to ListSessions.
func TestAdapterPath_ListSessions_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(context.Context, string, string) ([]session.Session, error) {
			t.Fatal("adapter.ListSessions must NOT be called when workspace is not ready")
			return nil, nil
		},
	}

	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-notready/sessions", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestAdapterPath_CreateSession_WorkspaceNotReady_Returns503 verifies
// the same readiness check applies to CreateSession.
func TestAdapterPath_CreateSession_WorkspaceNotReady_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.adapter = &mockAdapter{
		createSessionFn: func(context.Context, string, string, string) (*session.Session, error) {
			t.Fatal("adapter.CreateSession must NOT be called when workspace is not ready")
			return nil, nil
		},
	}

	env.wsMock.On("Get", mock.Anything, "ws-notready", mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-notready", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}, nil)

	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-notready/sessions", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestAdapterPath_GetHistory_WorkspaceReady_Returns200 verifies the
// happy path: workspace is Active, adapter is called, result returned.
func TestAdapterPath_GetHistory_WorkspaceReady_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)

	env := newTestEnv(t)
	env.handler.adapter = &mockAdapter{
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{}, nil
		},
	}

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
}

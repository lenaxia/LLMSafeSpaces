// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type ensureFixture struct {
	svc       *Service
	ws        *kmocks.MockWorkspaceInterface
	db        *imocks.MockDatabaseService
	clientset *k8sfake.Clientset
}

func newEnsureFixture(t *testing.T) *ensureFixture {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	ws := kmocks.NewMockWorkspaceInterface()
	db := &imocks.MockDatabaseService{}
	cache := &imocks.MockCacheService{}
	met := &imocks.MockMetricsService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(ws)

	clientset := k8sfake.NewSimpleClientset()
	k8s.On("Clientset").Return(clientset)

	svc, err := New(log, k8s, db, cache, met, &Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	return &ensureFixture{svc: svc, ws: ws, db: db, clientset: clientset}
}

func TestEnsureSession_TerminatedWorkspace_ReturnsError(t *testing.T) {
	f := newEnsureFixture(t)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseTerminated},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{UserID: "user-1"}, nil)

	_, err := f.svc.EnsureSession(context.Background(), "user-1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not usable")
}

func TestEnsureSession_FailedWorkspace_ReturnsError(t *testing.T) {
	f := newEnsureFixture(t)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseFailed},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{UserID: "user-1"}, nil)

	_, err := f.svc.EnsureSession(context.Background(), "user-1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not usable")
}

func TestEnsureSession_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newEnsureFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{UserID: "other-user"}, nil)

	_, err := f.svc.EnsureSession(context.Background(), "user-1", "ws-1")
	assert.Error(t, err)
}

func TestEnsureSession_WorkspaceNotFound_ReturnsError(t *testing.T) {
	f := newEnsureFixture(t)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(nil, fmt.Errorf("not found"))

	_, err := f.svc.EnsureSession(context.Background(), "user-1", "ws-1")
	assert.Error(t, err)
}

func TestEnsureSession_ActiveWorkspace_ReturnsSession(t *testing.T) {
	fakeBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// #1312: the MCP readiness poll precedes session creation
		if r.URL.Path == "/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{
				"llmsafespaces": {"status": "connected"},
			})
			return
		}
		assert.Equal(t, "/session", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		user, pass, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-password", pass)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "sess-abc-123"})
	}))
	defer fakeBackend.Close()

	backendURL, _ := url.Parse(fakeBackend.URL)
	_, portStr, _ := net.SplitHostPort(backendURL.Host)
	port, _ := strconv.Atoi(portStr)

	f := newEnsureFixture(t)
	ctx := context.Background()

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			PodIP: "127.0.0.1",
		},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{UserID: "user-1"}, nil)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-pw-ws-1", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("test-password")},
	}
	_, err := f.clientset.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	f.svc.config.OpencodePort = port

	resp, err := f.svc.EnsureSession(ctx, "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "ws-1", resp.WorkspaceID)
	assert.Equal(t, "sess-abc-123", resp.SessionID)
	assert.Equal(t, "Active", resp.WorkspacePhase)
	assert.False(t, resp.Resumed)
}

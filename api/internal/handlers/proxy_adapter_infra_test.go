// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// US-65.4 infrastructure tests - proxy_connections.go resolver bridges
// + SetAdapter. PR #716 review requested these.

func TestProxyPodIPResolver_ActivePodWithIP_ReturnsIP(t *testing.T) {
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.5"},
	}
	h := newProxyHandlerWithMockK8s(t, ws)
	resolver := h.AdapterPodIPResolver()

	ip, err := resolver.GetWorkspacePodIP(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.5", ip)
}

func TestProxyPodIPResolver_NonActivePhase_ReturnsEmptyIP(t *testing.T) {
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending, PodIP: "10.0.0.5"},
	}
	h := newProxyHandlerWithMockK8s(t, ws)
	resolver := h.AdapterPodIPResolver()

	ip, err := resolver.GetWorkspacePodIP(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, ip)
}

func TestProxyPodIPResolver_EmptyPodIP_ReturnsEmptyIP(t *testing.T) {
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: ""},
	}
	h := newProxyHandlerWithMockK8s(t, ws)
	resolver := h.AdapterPodIPResolver()

	ip, err := resolver.GetWorkspacePodIP(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, ip)
}

func TestProxyPodIPResolver_K8sClientError_ReturnsWrappedError(t *testing.T) {
	// When LlmsafespacesV1() returns an error, the resolver must
	// surface it wrapped with "get K8s client" context.
	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, fmt.Errorf("client unavailable"))

	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	resolver := h.AdapterPodIPResolver()
	_, err = resolver.GetWorkspacePodIP(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get K8s client")
	assert.Contains(t, err.Error(), "client unavailable", "wrapped error must preserve the root cause")
}

func TestProxyPodIPResolver_WorkspaceGetError_ReturnsWrappedError(t *testing.T) {
	// When the workspace Get returns an error, the resolver must
	// surface it wrapped with "get workspace" context.
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", context.Background(), "ws-1", metav1.GetOptions{}).
		Return((*v1.Workspace)(nil), fmt.Errorf("not found"))

	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)

	resolver := h.AdapterPodIPResolver()
	_, err = resolver.GetWorkspacePodIP(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get workspace")
	assert.Contains(t, err.Error(), "not found", "wrapped error must preserve the root cause")
}

func TestAdapterPasswordResolver_DelegatesToGetPassword(t *testing.T) {
	h, err := NewProxyHandler(
		k8smocks.NewMockKubernetesClient(),
		&testLogger{},
		"default",
		nil,
		nil,
	)
	require.NoError(t, err)
	h.state().SetCachedPassword(context.Background(), "ws-1", "test-pw-123")

	resolver := h.AdapterPasswordResolver()
	pw, err := resolver(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "test-pw-123", pw)
}

func TestSetAdapter_NilArg_IsNoOp(t *testing.T) {
	h, err := NewProxyHandler(
		k8smocks.NewMockKubernetesClient(),
		&testLogger{},
		"default",
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Nil(t, h.adapter)

	h.SetAdapter(nil)
	assert.Nil(t, h.adapter)
}

func TestSetAdapter_AfterStart_Panics(t *testing.T) {
	h, err := NewProxyHandler(
		k8smocks.NewMockKubernetesClient(),
		&testLogger{},
		"default",
		nil,
		nil,
	)
	require.NoError(t, err)
	h.started = true

	adapter := opencode.NewAdapter(
		func(_ context.Context, _ string) (string, error) { return "pw", nil },
		&stubPodIPResolver{},
		zap.NewNop(),
	)

	assert.Panics(t, func() { h.SetAdapter(adapter) })
}

func newProxyHandlerWithMockK8s(t *testing.T, ws *v1.Workspace) *ProxyHandler {
	t.Helper()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", context.Background(), ws.Name, metav1.GetOptions{}).Return(ws, nil)

	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	return h
}

type stubPodIPResolver struct{}

func (s *stubPodIPResolver) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

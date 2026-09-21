// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeRelayWorkspaceGetter resolves a fixed Workspace (the
// k8sWorkspaceGetterAdapter shape without a live cluster).
type fakeRelayWorkspaceGetter struct{}

func (fakeRelayWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	return &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "plat-ns"}}, nil
}

func handoffSecretData(t *testing.T, h *secrets.RelayHandoff) map[string][]byte {
	t.Helper()
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	return map[string][]byte{secrets.RelayHandoffDataKey: raw}
}

// TestK8sRelayTokenSource_ReadsHandoffSecret: the controller-written
// Secret (workspace-relay-<wsName>, data key `handoff`, in the
// workspace's namespace) parses into the builder's handoff shape.
func TestK8sRelayTokenSource_ReadsHandoffSecret(t *testing.T) {
	const fakeToken = "lrt_fakeAAAAadapter"
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       handoffSecretData(t, &secrets.RelayHandoff{Revision: "rADAPT01", RouterURL: "http://router.example", Providers: []secrets.RelayHandoffProvider{{ProviderSlug: "openai", Token: fakeToken, RouterPath: "/w/ws-77/openai/v1"}}}),
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)

	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, "rADAPT01", h.Revision)
	assert.Equal(t, fakeToken, h.Providers[0].Token)
	assert.Equal(t, "http://router.example/w/ws-77/openai/v1", h.RouterBase()+h.Providers[0].RouterPath)
}

// TestK8sRelayTokenSource_AbsentIsNotReady: a missing Secret returns
// (nil, nil) — the builder's staging-not-ready signal, never an error
// and never a raw fallback.
func TestK8sRelayTokenSource_AbsentIsNotReady(t *testing.T) {
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, k8sfake.NewSimpleClientset())
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	assert.Nil(t, h)
}

// TestK8sRelayTokenSource_EmptyDataIsNotReady: a Secret with an empty
// handoff key is the same not-ready class (an unusable handoff must not
// half-deliver).
func TestK8sRelayTokenSource_EmptyDataIsNotReady(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       map[string][]byte{},
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.NoError(t, err)
	assert.Nil(t, h)
}

// TestK8sRelayTokenSource_GarbageDataErrors: an unparseable handoff is
// a loud error (staging wrote something the builder cannot trust).
func TestK8sRelayTokenSource_GarbageDataErrors(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-relay-ws-77", Namespace: "plat-ns"},
		Data:       map[string][]byte{secrets.RelayHandoffDataKey: []byte("{not-json")},
	})
	src := newK8sRelayTokenSource(fakeRelayWorkspaceGetter{}, cs)
	h, err := src.RelayHandoff(context.Background(), "ws-77")
	require.Error(t, err)
	assert.Nil(t, h)
}

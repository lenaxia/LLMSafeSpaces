// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

// Re-home of the deleted TestProxy_EmptyPasswordKey (r1): the
// empty-password-key arm of ResolverHost.GetPassword — production-live
// on every adapter call — must surface a wrapped error, never an empty
// password.
func TestResolverHost_GetPassword_EmptyPasswordKey(t *testing.T) {
	fake := k8sfake.NewSimpleClientset()
	_, err := fake.CoreV1().Secrets("default").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-pw-ws-1", Namespace: "default"},
		Data:       map[string][]byte{"password": {}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("Clientset").Return(fake).Maybe()

	host := NewResolverHost(k8sMock, "default")
	_, err = host.GetPassword(context.Background(), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty password key")
}

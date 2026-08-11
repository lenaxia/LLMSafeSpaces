// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/mock"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// newMockK8sWithWorkspace creates a mock K8s client whose workspace CRD
// lookup returns an Active workspace at the given pod IP. Extracted from
// the deleted proxy_queue_test.go (US-63.7) for reuse by V2 tests.
func newMockK8sWithWorkspace(t *testing.T, workspaceID, podIP string) *k8smocks.MockKubernetesClient {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus(workspaceID, podIP, string(v1.WorkspacePhaseActive), workspaceID)
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset).Maybe()
	return k8sMock
}

// routingTransport routes HTTP requests to different backend hosts based
// on the URL path. Used by V2 integration tests that need to separate
// /event and /v1/statusz traffic from /api/session/* traffic.
// Extracted from the deleted proxy_queue_drain_miss_test.go (US-63.7).
type routingTransport struct {
	eventHost  string
	promptHost string
}

func (rt *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	if req.URL.Path == "/event" || req.URL.Path == "/v1/statusz" || req.URL.Path == "/v1/healthz" {
		r.URL.Host = rt.eventHost
	} else {
		r.URL.Host = rt.promptHost
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8swatch "k8s.io/apimachinery/pkg/watch"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestTermination_Integration_WatcherDrivesOutboxCleanup (#1119 wiring,
// integration level): the REAL CRD watcher's Terminated event drives
// onPhaseChange → the detached cleanup goroutine → Valkey — the full
// request path the review asked for, not the direct handler call.
func TestTermination_Integration_WatcherDrivesOutboxCleanup(t *testing.T) {
	const wsName = "ws-term-e2e"
	fakeWatch := k8swatch.NewFake()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	active := &v1.Workspace{}
	active.Name = wsName
	active.Spec.Owner.UserID = "user-1"
	active.Status.Phase = v1.WorkspacePhaseActive
	active.Status.PodIP = "10.0.0.1"
	terminated := active.DeepCopy()
	terminated.Status.Phase = v1.WorkspacePhaseTerminated
	list := &v1.WorkspaceList{}
	list.Items = []v1.Workspace{*active}
	wsMock.On("List", mock.Anything, mock.Anything).Return(list, nil).Maybe()
	wsMock.On("Watch", mock.Anything, mock.Anything).Return(fakeWatch, nil).Maybe()
	wsMock.On("Get", mock.Anything, wsName, metav1.GetOptions{}).Return(active, nil).Maybe()
	wsMock.On("Patch", mock.Anything, wsName, mock.Anything, mock.Anything, mock.Anything).Return(active, nil).Maybe()

	secret := makePasswordSecret(wsName, "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	logger := &e2eLogger{}
	handler, err := NewProxyHandler(k8sMock, logger, "default", nil, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.SetPriorPhaseForTest(wsName, "Active")
	t.Cleanup(stubUsageStream())

	mr, merr := miniredis.Run()
	require.NoError(t, merr)
	t.Cleanup(mr.Close)
	handler.SetOutbox(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	raw, err := json.Marshal(map[string]any{
		"id": "e1", "clientMessageID": "cm-e1", "userID": "user-1", "text": "hi",
		"acceptedAt": time.Now().UTC(), "status": "pending",
	})
	require.NoError(t, err)
	require.NoError(t, handler.outbox.SeedEntryForTest(context.Background(), wsName, "ses-1", string(raw)))
	require.NoError(t, mr.Set("outboxd:"+wsName+":ses-1", `{"id":"e1"}`))
	require.NoError(t, mr.Set("outboxdedupe:"+wsName+":ses-1:cm-e1", "1"))

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })
	t.Cleanup(func() {
		if t.Failed() {
			logger.dump(t)
		}
	})

	// The controller's terminal transition, delivered the way production
	// delivers it: through the CRD watcher's event stream.
	fakeWatch.Modify(terminated)

	require.Eventually(t, func() bool {
		return !mr.Exists("outboxq:"+wsName+":ses-1") && !mr.Exists("outboxd:"+wsName+":ses-1") &&
			!mr.Exists("outboxdedupe:"+wsName+":ses-1:cm-e1")
	}, 10*time.Second, 50*time.Millisecond,
		"the watcher-driven Terminated event must clean the workspace's outbox residue")
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

// TestPublishWorkspaceEvent_DeliversToAllWorkspaceSubscribers verifies that
// publishWorkspaceEvent delivers to every subscriber on the same workspace.
func TestPublishWorkspaceEvent_DeliversToAllWorkspaceSubscribers(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	handler.userBroker = eventbroker.NewUserEventBroker()

	sub1, err := handler.userBroker.SubscribeWorkspace("ws-bridge")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-bridge", sub1)

	sub2, err := handler.userBroker.SubscribeWorkspace("ws-bridge")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-bridge", sub2)

	evt := apitypes.WorkspaceSSEEvent{Type: "workspace.phase", WorkspaceID: "ws-bridge", Phase: "Active"}
	handler.publishWorkspaceEvent("ws-bridge", evt)

	for i, sub := range []*eventbroker.Subscriber{sub1, sub2} {
		select {
		case got := <-sub.Ch:
			assert.Equal(t, "workspace.phase", got.Type, "subscriber %d", i)
			assert.Equal(t, "Active", got.Phase, "subscriber %d", i)
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive the event", i)
		}
	}
}

// TestPublishWorkspaceEvent_NilUserBrokerDoesNotPanic verifies nil-safety.
func TestPublishWorkspaceEvent_NilUserBrokerDoesNotPanic(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	handler.userBroker = nil

	assert.NotPanics(t, func() {
		handler.publishWorkspaceEvent("ws-x", apitypes.WorkspaceSSEEvent{Type: "test"})
	})
}

// TestPublishWorkspaceEvent_UserBrokerReceivesEvent verifies publishWorkspaceEvent
// delivers events via userBroker.SubscribeWorkspace.
func TestPublishWorkspaceEvent_UserBrokerReceivesEvent(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, newLenientMockAdapter())
	require.NoError(t, err)

	handler.userBroker = eventbroker.NewUserEventBroker()

	sub, subErr := handler.userBroker.SubscribeWorkspace("ws-y")
	require.NoError(t, subErr)
	defer handler.userBroker.UnsubscribeWorkspace("ws-y", sub)

	assert.NotPanics(t, func() {
		handler.publishWorkspaceEvent("ws-y", apitypes.WorkspaceSSEEvent{Type: "test"})
	})

	select {
	case <-sub.Ch:
	case <-time.After(time.Second):
		t.Fatal("userBroker subscriber must receive event")
	}
}

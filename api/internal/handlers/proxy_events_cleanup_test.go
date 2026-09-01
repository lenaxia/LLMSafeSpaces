// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestOnPhaseChange_TerminatedCleansOutboxResidue (#1119 wiring): the
// Terminating/Terminated transition must fire the outbox's workspace
// cleanup — the handler-level proof behind the F2 fix, not just the
// service unit.
func TestOnPhaseChange_TerminatedCleansOutboxResidue(t *testing.T) {
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(&v1.Workspace{}, nil).Maybe()
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())
	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	h.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(stubUsageStream())

	mr, merr := miniredis.Run()
	require.NoError(t, merr)
	t.Cleanup(mr.Close)
	h.SetOutbox(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	// Residue for the dying workspace (queue + staging + dedupe + lock)
	// and for a living one.
	raw, err := json.Marshal(map[string]any{
		"id": "e1", "clientMessageID": "cm-e1", "userID": "u1", "text": "hi",
		"acceptedAt": time.Now().UTC(), "status": "pending",
	})
	require.NoError(t, err)
	require.NoError(t, h.outbox.SeedEntryForTest(context.Background(), "ws-dying", "ses-1", string(raw)))
	require.NoError(t, mr.Set("outboxd:ws-dying:ses-1", `{"id":"e1"}`))
	require.NoError(t, mr.Set("outboxdedupe:ws-dying:ses-1:cm-e1", "1"))
	require.NoError(t, mr.Set("outboxlock:ws-dying:ses-1", "tok"))
	require.NoError(t, h.outbox.SeedEntryForTest(context.Background(), "ws-alive", "ses-1", string(raw)))

	for _, phase := range []v1.WorkspacePhase{v1.WorkspacePhaseTerminating, v1.WorkspacePhaseTerminated} {
		ws := &v1.Workspace{}
		ws.Name = "ws-dying"
		ws.Status.Phase = phase
		h.onPhaseChange(ws)
	}

	require.Eventually(t, func() bool {
		for _, k := range []string{
			"outboxq:ws-dying:ses-1", "outboxd:ws-dying:ses-1",
			"outboxdedupe:ws-dying:ses-1:cm-e1", "outboxlock:ws-dying:ses-1",
		} {
			if mr.Exists(k) {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "the Terminated transition must clean the dying workspace's outbox keys")

	assert.True(t, mr.Exists("outboxq:ws-alive:ses-1"), "living workspaces keep their queues")
}

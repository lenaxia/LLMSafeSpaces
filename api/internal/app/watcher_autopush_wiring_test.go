// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestWatcherAutopushWiring_CallbackToPusherChain exercises the
// load-bearing wiring seam a unit test alone cannot cover: the real
// secretautopush.Service, constructed the way app.New constructs it,
// driving a fake pusher through the full "callback → bindings check →
// push" chain at the interface boundaries. US-70.2 removed the DEK
// priming and auth-ctx threading entirely — the pusher builds the
// batch session-free — so the chain is now exactly these two hops.
func TestWatcherAutopushWiring_CallbackToPusherChain(t *testing.T) {
	bindings := &wiringFakeBindingsChecker{returnExists: true}
	pusher := &wiringFakePusher{}

	svc := secretautopush.New(bindings, pusher)

	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-wiring", Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner: v1.WorkspaceOwner{UserID: "user-wiring"},
		},
		Status: v1.WorkspaceStatus{
			Phase:            v1.WorkspacePhaseActive,
			UserCredsPresent: wiringBoolPtr(false),
		},
	}

	svc.OnWorkspaceUpdate(ws)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pusher.calls.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.EqualValues(t, 1, pusher.calls.Load(), "push MUST fire")
	require.NotNil(t, pusher.lastCtx.Load(), "pusher MUST have received a ctx")
}

// --- test fixtures ---

func wiringBoolPtr(b bool) *bool { return &b }

type wiringFakeBindingsChecker struct{ returnExists bool }

func (f *wiringFakeBindingsChecker) UserHasBoundSecrets(_ context.Context, _ string) (bool, error) {
	return f.returnExists, nil
}

type wiringFakePusher struct {
	calls   atomic.Int64
	lastCtx atomic.Value
}

func (f *wiringFakePusher) Push(ctx context.Context, _, _ string) error {
	f.calls.Add(1)
	f.lastCtx.Store(ctx)
	return nil
}

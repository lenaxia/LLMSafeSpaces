// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestWatcherAutopushWiring_AuthCtxThreadedFromDEKToPusher exercises
// the load-bearing wiring seam that a unit test alone cannot cover:
// the real secretautopush.Service, wired to the PRODUCTION
// agentpushAuthCtxBuilder (not a test double), producing a ctx that
// the fake pusher can inspect via agentpush.AuthFromContext.
//
// This proves that:
//   - GetDEKForUser's returned jti actually reaches the pusher's
//     downstream ctx via WithAuth.
//   - agentpush.AuthFromContext successfully recovers the sessionID.
//   - The full "callback → DEK fetch → auth ctx build → push" chain
//     works end-to-end at the interface boundaries.
//
// Uses fakes for DEKRetriever + BindingsChecker + SecretPusher (which
// unit tests already cover) but the AuthContexter is the real
// production type from this package.
func TestWatcherAutopushWiring_AuthCtxThreadedFromDEKToPusher(t *testing.T) {
	const wantJTI = "jti-e2e-wiring-test"

	dek := &wiringFakeDEKRetriever{returnDEK: []byte("dek"), returnJTI: wantJTI}
	bindings := &wiringFakeBindingsChecker{returnExists: true}
	pusher := &wiringFakePusher{}

	svc := secretautopush.New(dek, bindings, pusher,
		// PRODUCTION adapter — this is what app.New wires in.
		secretautopush.WithAuthContexter(agentpushAuthCtxBuilder{}),
	)

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

	// Wait for the fire-and-forget goroutine to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pusher.calls.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.EqualValues(t, 1, pusher.calls.Load(), "push MUST fire")

	// Load the ctx the pusher received and verify auth was threaded.
	gotCtx := pusher.lastCtx.Load()
	require.NotNil(t, gotCtx, "pusher MUST have received a ctx")
	sessionID, matchedKey := agentpush.AuthFromContext(gotCtx.(context.Context))
	assert.Equal(t, wantJTI, sessionID,
		"agentpush.AuthFromContext MUST recover the jti that "+
			"GetDEKForUser returned — the auth-ctx wiring is what "+
			"lets the pusher's downstream GetDEK(sessionID) hit the "+
			"Redis cache populated by GetDEKForUser")
	assert.Nil(t, matchedKey,
		"matchedSigningKey MUST be nil — the auth-ctx builder passes "+
			"nil intentionally; the DEK is cached in Redis under the "+
			"jti, no signing-key rehydrate needed")
}

// --- test fixtures ---

func wiringBoolPtr(b bool) *bool { return &b }

type wiringFakeDEKRetriever struct {
	returnDEK []byte
	returnJTI string
}

func (f *wiringFakeDEKRetriever) GetDEKForUser(_ context.Context, _ string) ([]byte, string, error) {
	return f.returnDEK, f.returnJTI, nil
}

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

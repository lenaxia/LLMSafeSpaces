// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/secretautopush"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"k8s.io/apimachinery/pkg/watch"
)

// TestChainedWiring_WatcherToSecretautopush is the end-to-end
// wiring test the bot's review pass 3 requested: drive a real K8s
// watch event through the real Watcher, into the real
// secretautopush.Service (installed via SetWorkspaceUpdateCallback),
// through the real agentpush.WithAuth-equivalent adapter, into a
// probe pusher. This closes the last observability gap: a future
// refactor that wires the callback incorrectly at the watcher level
// (or breaks the through-chain) would fail this test.
//
// This is the "connected chain" test. Every hop is real EXCEPT:
//   - DEKRetriever (fake — real one queries jwt_sessions + KeyService)
//   - BindingsChecker (fake — real one queries SecretStore)
//   - SecretPusher (fake — real one HTTP-POSTs to agentd)
//
// The tested chain: watch event → Watcher.handleWatchEvent →
// onWorkspaceUpdate → secretautopush.Service.OnWorkspaceUpdate →
// filter → DEKRetriever.GetDEKForUser (fake) → AuthContexter.WithAuth
// (real agentpush.WithAuth via a mini-adapter) → SecretPusher.Push
// (fake) → agentpush.AuthFromContext round-trip.
func TestChainedWiring_WatcherToSecretautopush(t *testing.T) {
	// --- Set up the real Watcher.
	k8s := k8smocks.NewMockKubernetesClient()
	llm := k8smocks.NewMockLLMSafespacesV1Interface()
	wsIface := k8smocks.NewMockWorkspaceInterface()
	fakeWatch := watch.NewFake()

	k8s.On("LlmsafespacesV1").Return(llm, nil)
	llm.On("Workspaces", "default").Return(wsIface)
	wsIface.On("Watch", mock.Anything, mock.Anything).Return(fakeWatch, nil).Maybe()
	wsIface.On("List", mock.Anything, mock.Anything).
		Return(&v1.WorkspaceList{}, nil).Maybe()

	w, err := workspace.NewWatcher(k8s, testLogger{}, "default", func(*v1.Workspace) {})
	require.NoError(t, err)

	// --- Set up the real secretautopush.Service.
	const wantJTI = "jti-chained-wiring"
	dek := &chainedFakeDEK{returnDEK: []byte("dek"), returnJTI: wantJTI}
	bindings := &chainedFakeBindings{returnExists: true}
	pusher := &chainedFakePusher{}
	svc := secretautopush.New(dek, bindings, pusher,
		secretautopush.WithAuthContexter(chainedAgentpushAuther{}),
	)

	// Wire secretautopush's OnWorkspaceUpdate as the watcher's
	// per-CRD-event callback. This is the exact wiring app.New does.
	w.SetWorkspaceUpdateCallback(svc.OnWorkspaceUpdate)

	require.NoError(t, w.Start())
	defer w.Stop()

	// --- Drive a Modified event through the fakeWatch. The Watcher's
	// watch goroutine should invoke onWorkspaceUpdate → svc.OnWorkspaceUpdate
	// → filter passes (Active + UserCredsPresent=false + owner set) →
	// fake DEK returns → chainedAgentpushAuther builds ctx → fake pusher
	// records it.
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-chain", Namespace: "default", ResourceVersion: "1"},
		Spec: v1.WorkspaceSpec{
			Owner: v1.WorkspaceOwner{UserID: "user-chain"},
		},
		Status: v1.WorkspaceStatus{
			Phase:            v1.WorkspacePhaseActive,
			UserCredsPresent: chainedBoolPtr(false),
		},
	}
	fakeWatch.Add(ws)

	// Poll for the pusher to receive its call. The chain is fully async
	// (watch goroutine → secretautopush.Service.run goroutine).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pusher.calls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.EqualValues(t, 1, pusher.calls.Load(),
		"pusher MUST have been called exactly once through the full chain "+
			"(watcher event → callback → filter → DEK fetch → push)")

	// The auth ctx must carry the jti as sessionID (via agentpush.WithAuth
	// invoked inside chainedAgentpushAuther, verified via
	// agentpush.AuthFromContext).
	gotCtx := pusher.lastCtx.Load()
	require.NotNil(t, gotCtx)
	sessionID, _ := agentpush.AuthFromContext(gotCtx.(context.Context))
	assert.Equal(t, wantJTI, sessionID,
		"end-to-end chain MUST thread the DEKRetriever's jti through "+
			"AuthContexter into the pusher's ctx — this is the exact "+
			"integration point the bot's review pass 3 requested coverage for")

	// Also confirm the pusher received the correct user/workspace IDs.
	assert.Equal(t, "user-chain", pusher.sawUserID.Load())
	assert.Equal(t, "ws-chain", pusher.sawWorkspaceID.Load())
}

// --- fixtures ---

func chainedBoolPtr(b bool) *bool { return &b }

type chainedFakeDEK struct {
	returnDEK []byte
	returnJTI string
}

func (f *chainedFakeDEK) GetDEKForUser(_ context.Context, _ string) ([]byte, string, error) {
	return f.returnDEK, f.returnJTI, nil
}

type chainedFakeBindings struct{ returnExists bool }

func (f *chainedFakeBindings) UserHasBoundSecrets(_ context.Context, _ string) (bool, error) {
	return f.returnExists, nil
}

type chainedFakePusher struct {
	calls          atomic.Int64
	sawUserID      atomic.Value
	sawWorkspaceID atomic.Value
	lastCtx        atomic.Value
}

func (f *chainedFakePusher) Push(ctx context.Context, userID, workspaceID string) error {
	f.calls.Add(1)
	f.sawUserID.Store(userID)
	f.sawWorkspaceID.Store(workspaceID)
	f.lastCtx.Store(ctx)
	return nil
}

// chainedAgentpushAuther mirrors the production adapter
// (agentpushAuthCtxBuilder in api/internal/app/secrets_adapters.go).
// Kept local to this test file to avoid an app-package import cycle.
type chainedAgentpushAuther struct{}

func (chainedAgentpushAuther) WithAuth(ctx context.Context, sessionID string, _ []byte) context.Context {
	return agentpush.WithAuth(ctx, sessionID, nil)
}

// testLogger is a minimal no-op implementation of
// pkginterfaces.LoggerInterface. Duplicated from watcher_test.go
// because that's in package workspace (not workspace_test).
type testLogger struct{}

func (testLogger) Debug(string, ...interface{})                      {}
func (testLogger) Info(string, ...interface{})                       {}
func (testLogger) Warn(string, ...interface{})                       {}
func (testLogger) Error(string, error, ...interface{})               {}
func (testLogger) Fatal(string, error, ...interface{})               {}
func (testLogger) With(...interface{}) pkginterfaces.LoggerInterface { return testLogger{} }
func (testLogger) Sync() error                                       { return nil }

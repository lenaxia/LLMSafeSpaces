// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// state_reconciler_rearm_1732_test.go — #1732 leg B pins: after an API
// roll (the incident's wedge leg), the NEW api pod starts gate-less and
// — for sessions already in flight — nothing re-arms the persistent
// usage-stream gates the activity way (the halted sessions route no new
// outbox deliveries / adapter writes). The re-arm reconciler must
// re-establish the pod stream from the ROLL-SURVIVING ws_state truth
// within one reconcile pass, while D1-B holds: idle workspaces (no
// active-session records) still open NOTHING.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// start1732RollRig builds the full-wiring environment of
// proxy_902_e2e_test.go (real ProxyHandler.Start: activity tracker, CRD
// watcher seed, state reconciler) against mock k8s, with the
// usage-stream singleton swapped for a recording consumer and — the
// #1732 addition — a REAL wsstate store shared across BOTH handler
// instances (Redis's role in production: state that survives the roll).
type rollRig1732 struct {
	handler *ProxyHandler
	fc      *gateTestClient
	store   wsstate.Store
}

func start1732RollRig(t *testing.T, wsName, podIP string, interval time.Duration, store wsstate.Store) *rollRig1732 {
	t.Helper()
	orig := stateReconcileInterval
	stateReconcileInterval = interval
	t.Cleanup(func() { stateReconcileInterval = orig })

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // statusz probe target (silent)
	}))
	t.Cleanup(backend.Close)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	wsCR := &v1.Workspace{}
	wsCR.Name = wsName
	wsCR.Spec.Owner.UserID = "user-1"
	wsCR.Status.Phase = v1.WorkspacePhaseActive
	wsCR.Status.PodIP = podIP
	list := &v1.WorkspaceList{}
	list.Items = []v1.Workspace{*wsCR}
	wsMock.On("List", mock.Anything, mock.Anything).Return(list, nil).Maybe()
	wsMock.On("Watch", mock.Anything, mock.Anything).Return(watch.NewFake(), nil).Maybe()
	wsMock.On("Get", mock.Anything, wsName, metav1.GetOptions{}).Return(wsCR, nil).Maybe()
	wsMock.On("Patch", mock.Anything, wsName, mock.Anything, mock.Anything, mock.Anything).Return(wsCR, nil).Maybe()

	secret := makePasswordSecret(wsName, "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	logger := &e2eLogger{}
	handler, err := NewProxyHandler(k8sMock, logger, "default", backend.Client(), newLenientMockAdapter())
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(func() {
		if t.Failed() {
			logger.dump(t)
		}
	})
	handler.SetStateStore(store)
	// Steady-state precondition (the #902 shape): the roll-surviving
	// prior phase already says Active, so the watcher's seed fires NO
	// transition (a transition into Active legitimately cancels gates
	// for a fresh connection — not the phenomenon under test here).
	handler.SetPriorPhaseForTest(wsName, "Active")

	fc := &gateTestClient{connected: make(chan struct{}, 16)}
	consumer := usagestream.New(usagestream.Config{
		Resolve: func(context.Context, string) (string, string, error) {
			return "http://pod", "test-pw", nil
		},
		NewClient: func(baseURL, password string) usagestream.Client { return fc },
		Bridge:    &usageBridge{h: handler},
		Logger:    logger,
		IdleDrop:  time.Hour,
		Retry:     50 * time.Millisecond,
	})
	t.Cleanup(injectUsageStream(consumer))

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })
	return &rollRig1732{handler: handler, fc: fc, store: store}
}

// Test1732_ApiRoll_ReArmsGateForLiveSessionWithinOnePass is the issue's
// Leg-B acceptance at the fullest in-repo layer: a session is LIVE
// (active-session record in the roll-surviving store, gate armed on the
// OLD pod) when the api rolls — old instance gone entirely, new instance
// booting with NO new routed activity. The new pod's reconciler must
// re-arm the gate from the store within ONE pass (startup pass or first
// tick — bounded by reconnect-backoff + replay semantics, NOT by a
// human poking the workspace).
func Test1732_ApiRoll_ReArmsGateForLiveSessionWithinOnePass(t *testing.T) {
	store := wsstate.NewInMemoryStore()
	require.True(t, store.CheckAndAddActiveSession(context.Background(), "ws-1732", "ses-live", 4),
		"sanity: session recorded live in the roll-surviving store")

	// The OLD api pod: activity-armed gate, connected.
	old := start1732RollRig(t, "ws-1732", "10.0.0.1", time.Hour, store)
	old.handler.SetAgentdTerminus(true)
	old.handler.outboxOnDelivered("ws-1732", "ses-live", outboxEntry1732("e1"))
	select {
	case <-old.fc.connected:
	case <-time.After(10 * time.Second):
		t.Fatal("old pod: gate never connected (rig sanity)")
	}

	// THE ROLL: the old pod is GONE (consumer torn down with it — the
	// singleton swap models the new pod's fresh process state).
	old.handler.UsageStream().CloseAll()
	require.NoError(t, old.handler.Stop())

	// The NEW api pod: same store (Redis in production), no activity.
	interval := 250 * time.Millisecond // one fast tick; the startup pass may fire first
	fc := &gateTestClient{connected: make(chan struct{}, 16)}
	orig := stateReconcileInterval
	stateReconcileInterval = interval
	t.Cleanup(func() { stateReconcileInterval = orig })
	newHandler, newFC := start1732NewPod(t, "ws-1732", "10.0.0.1", store, fc)

	_ = newHandler
	select {
	case <-newFC.connected:
		// The persistent pod stream re-established: the gate connected,
		// replay/snapshot reconcile re-seeds bridge truth (#1666 seed
		// diff), usage-gate state returns.
	case <-time.After(5 * time.Second):
		t.Fatal("#1732: after the api roll the usage gate never re-armed — the platform stays blind to the live session (the wedge)")
	}
	require.True(t, newHandler.UsageStream().HasGate("ws-1732"), "usage-gate state must return after the roll")
}

// start1732NewPod boots the rolled-in api instance: a fresh handler +
// fresh consumer (new process state) sharing ONLY the store.
func start1732NewPod(t *testing.T, wsName, podIP string, store wsstate.Store, fc *gateTestClient) (*ProxyHandler, *gateTestClient) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	wsCR := &v1.Workspace{}
	wsCR.Name = wsName
	wsCR.Spec.Owner.UserID = "user-1"
	wsCR.Status.Phase = v1.WorkspacePhaseActive
	wsCR.Status.PodIP = podIP
	list := &v1.WorkspaceList{}
	list.Items = []v1.Workspace{*wsCR}
	wsMock.On("List", mock.Anything, mock.Anything).Return(list, nil).Maybe()
	wsMock.On("Watch", mock.Anything, mock.Anything).Return(watch.NewFake(), nil).Maybe()
	wsMock.On("Get", mock.Anything, wsName, metav1.GetOptions{}).Return(wsCR, nil).Maybe()
	wsMock.On("Patch", mock.Anything, wsName, mock.Anything, mock.Anything, mock.Anything).Return(wsCR, nil).Maybe()

	secret := makePasswordSecret(wsName, "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	logger := &e2eLogger{}
	handler, err := NewProxyHandler(k8sMock, logger, "default", backend.Client(), newLenientMockAdapter())
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	handler.SetStateStore(store)
	handler.SetPriorPhaseForTest(wsName, "Active")
	handler.SetAgentdTerminus(true)
	t.Cleanup(func() {
		if t.Failed() {
			logger.dump(t)
		}
	})

	consumer := usagestream.New(usagestream.Config{
		Resolve: func(context.Context, string) (string, string, error) {
			return "http://pod", "test-pw", nil
		},
		NewClient: func(baseURL, password string) usagestream.Client { return fc },
		Bridge:    &usageBridge{h: handler},
		Logger:    logger,
		IdleDrop:  time.Hour,
		Retry:     50 * time.Millisecond,
	})
	t.Cleanup(injectUsageStream(consumer))

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })
	return handler, fc
}

func outboxEntry1732(id string) outbox.Entry {
	return outbox.Entry{ID: id, ClientMessageID: "cmid-" + id, UserID: "user-1", Text: "hi"}
}

// Test1732_ReArmSparesIdleWorkspaces_D1BHolds pins the D1-B boundary:
// an Active workspace with NO live sessions (idle fleet) opens nothing —
// the re-arm is a live-session recovery path, never a blanket (an API
// deploy with idle pods must not fan in pod streams).
func Test1732_ReArmSparesIdleWorkspaces_D1BHolds(t *testing.T) {
	store := wsstate.NewInMemoryStore()
	rig := start1732RollRig(t, "ws-idle", "10.0.0.2", 150*time.Millisecond, store)
	rig.handler.SetAgentdTerminus(true)

	// No active sessions recorded; let the reconciler run several passes.
	requireGateSilence(t, rig.fc, "idle workspace (no live sessions) must not gain a gate — D1-B")
	assert.Equal(t, 0, rig.handler.UsageStream().Gates())
}

// Test1732_ReArmIsIdempotentForArmedGates: an already-armed gate is
// left alone (Open is idempotent, but the re-arm must not churn).
func Test1732_ReArmIsIdempotentForArmedGates(t *testing.T) {
	store := wsstate.NewInMemoryStore()
	require.True(t, store.CheckAndAddActiveSession(context.Background(), "ws-armed", "ses-1", 4))
	rig := start1732RollRig(t, "ws-armed", "10.0.0.3", 150*time.Millisecond, store)
	rig.handler.SetAgentdTerminus(true)
	rig.handler.outboxOnDelivered("ws-armed", "ses-1", outboxEntry1732("e1"))
	select {
	case <-rig.fc.connected:
	case <-time.After(10 * time.Second):
		t.Fatal("activity-armed gate never connected (rig sanity)")
	}
	// Reconciler passes run; exactly one gate must remain, no churn.
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 1, rig.handler.UsageStream().Gates())
	// gateTestClient records each (re)connection; no second connect.
	select {
	case <-rig.fc.connected:
		t.Fatal("the armed gate must not be torn down and re-opened by the re-arm pass")
	default:
	}
}

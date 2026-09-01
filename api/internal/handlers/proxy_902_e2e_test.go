// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #902/#903 full-wiring tests, ported to the US-69.11 usage-gate seam:
// real ProxyHandler.Start() → real CRD watcher seed → onPhaseChange →
// UsageStream().Open → the gate's (fake) ABI client connects → derived
// state reaches the user stream through the PRODUCTION usageBridge.
// These catch wiring deletions the direct unit calls cannot (review
// round 1 on #903): removing the seed path, the phaseSource assignment,
// or the reconciler launch breaks them.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// e2eLogger captures log lines so e2e failures self-diagnose
// (testLogger swallows everything; the reconciler's Warn output is the
// fastest route to why a gate did or didn't connect).
type e2eLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *e2eLogger) logf(level, msg string, kv ...interface{}) string {
	line := level + " " + msg
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	return line
}

func (l *e2eLogger) Debug(msg string, kv ...interface{}) {}
func (l *e2eLogger) Info(msg string, kv ...interface{})  { l.logf("INFO", msg, kv...) }
func (l *e2eLogger) Warn(msg string, kv ...interface{})  { l.logf("WARN", msg, kv...) }
func (l *e2eLogger) Error(msg string, err error, kv ...interface{}) {
	l.logf("ERROR", msg+": "+err.Error(), kv...)
}
func (l *e2eLogger) Fatal(msg string, err error, kv ...interface{}) {
	l.logf("FATAL", msg+": "+err.Error(), kv...)
}
func (l *e2eLogger) With(kv ...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *e2eLogger) Sync() error                                          { return nil }

func (l *e2eLogger) dump(t *testing.T) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		t.Log(line)
	}
}

// startFullWiring builds a ProxyHandler wired exactly like production
// (NewProxyHandler + Start: activity tracker, CRD watcher with seed,
// state reconciler) against mock k8s, with the usage-stream singleton
// swapped for a consumer whose client is the recording fake and whose
// bridge is the PRODUCTION usageBridge. priorPhaseActive simulates the
// Redis-persisted prior that made post-restart seeds skip arming (#902).
// ipReady != nil models the resume race: resolve fails until the flag
// flips (the pod-IP-empty unhappy path).
func startFullWiring(t *testing.T, wsName, podIP, priorPhase string, reconcileInterval time.Duration, ipReady <-chan struct{}) (*ProxyHandler, *gateTestClient) {
	t.Helper()
	orig := stateReconcileInterval
	stateReconcileInterval = reconcileInterval
	t.Cleanup(func() { stateReconcileInterval = orig })

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // statusz reconcile probe target (silent)
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
	// Activity tracker flush patches lastActivityAt on active workspaces.
	wsMock.On("Patch", mock.Anything, wsName, mock.Anything, mock.Anything, mock.Anything).Return(wsCR, nil).Maybe()

	// Password via the fake clientset secret (production path).
	secret := makePasswordSecret(wsName, "test-pw")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	logger := &e2eLogger{}
	handler, err := NewProxyHandler(k8sMock, logger, "default", backend.Client(), nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(func() {
		if t.Failed() {
			logger.dump(t)
		}
	})

	// The usage-stream swap: recording client, production bridge. The
	// resolve seam models agentdEndpoint — instant success, or (for the
	// resume-race test) failure until the pod IP appears.
	fc := &gateTestClient{connected: make(chan struct{}, 16)}
	consumer := usagestream.New(usagestream.Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			if ipReady != nil {
				select {
				case <-ipReady:
				case <-ctx.Done():
					return "", "", ctx.Err()
				default:
					return "", "", fmt.Errorf("no pod IP yet (resume race) for %s", workspaceID)
				}
			}
			return "http://pod", "test-pw", nil
		},
		NewClient: func(baseURL, password string) usagestream.Client { return fc },
		Bridge:    &usageBridge{h: handler},
		Logger:    logger,
		IdleDrop:  time.Hour,
		Retry:     50 * time.Millisecond,
	})
	t.Cleanup(injectUsageStream(consumer))

	// The #902 precondition: Redis-persisted prior phase already says
	// Active, so the seed's onPhaseChange sees a no-transition update.
	handler.SetPriorPhaseForTest(wsName, priorPhase)

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })
	return handler, fc
}

func requireGateConnect(t *testing.T, fc *gateTestClient, what string) {
	t.Helper()
	select {
	case <-fc.connected:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: usage gate never connected", what)
	}
}

// Test902_E2E_SeedWithPersistedPriorArmsGateAndBridgesEvents is the
// full-wiring reproduction of the incident: real Start(), prior phase
// Active (Redis-persisted shape), seed fires onPhaseChange — the usage
// gate must arm and CONNECT despite prior == Active, and a pod ABI event
// must reach a user subscriber through the production bridge. Pre-fix,
// the seed took the else-branch and none of this happened: users halted
// while sends succeeded.
func Test902_E2E_SeedWithPersistedPriorArmsGateAndBridgesEvents(t *testing.T) {
	handler, fc := startFullWiring(t, "ws-902e2e", "10.0.0.1", "Active", time.Hour, nil)

	requireGateConnect(t, fc, "seed with Redis-persisted prior=Active must still arm and connect the gate (#902)")

	// A pod-side session.status event bridges onto the user stream.
	sub, err := handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeUser("user-1", sub)

	fc.apply(2, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: "ses-x",
		Status:    abiv1.SessionStatus_SESSION_STATUS_IDLE,
	})

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "ws-902e2e", evt.WorkspaceID)
		assert.Equal(t, "ses-x", evt.SessionID)
		assert.Equal(t, "idle", evt.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("derived session.status never reached the user subscriber — gate connected but the bridge is not relaying")
	}
}

// Test902_E2E_ReconcilerHealsDeletedGate proves the Start()-launched
// reconciler wiring: after a gate is torn down (simulating a dropped
// gate with no phase transition), a NEW connection appears within one
// reconcile interval. Deleting the phaseSource assignment or the
// goroutine launch in Start() fails this test.
func Test902_E2E_ReconcilerHealsDeletedGate(t *testing.T) {
	handler, fc := startFullWiring(t, "ws-902heal", "10.0.0.1", "Active", 100*time.Millisecond, nil)

	requireGateConnect(t, fc, "initial gate")

	// Kill the gate exactly as a connection death does: no transition,
	// no user stream — nothing but the reconciler can bring it back.
	handler.UsageStream().Close("ws-902heal")
	require.Equal(t, 0, handler.UsageStream().Gates(), "sanity: gate torn down")

	requireGateConnect(t, fc, "reconciler must re-arm the deleted gate within one interval")
	require.Equal(t, 1, handler.UsageStream().Gates())
}

// Test902_E2E_ArmWhileUnresolvableThenRecovers: the unhappy path — the
// workspace is Active but the pod is not reachable yet (resume race).
// The gate arms (retrying resolve), and once the pod resolves the gate
// connects and events flow.
func Test902_E2E_ArmWhileUnresolvableThenRecovers(t *testing.T) {
	ipReady := make(chan struct{})
	handler, fc := startFullWiring(t, "ws-902ip", "", "Active", time.Hour, ipReady)

	// Armed but cannot connect yet.
	require.Eventually(t, func() bool { return handler.UsageStream().Gates() == 1 },
		5*time.Second, 20*time.Millisecond, "gate must arm while the pod is unresolvable")
	select {
	case <-fc.connected:
		t.Fatal("sanity: no pod yet, no connection possible")
	default:
	}

	// Pod appears (resume completes); the retry loop connects.
	close(ipReady)
	requireGateConnect(t, fc, "gate retry loop must connect once the pod resolves")
}

// Test902_TransitionCancelsExistingGate pins the fresh-connection
// semantics with a REAL cancel observation: a transition into Active
// must close the previous gate (cancel its stream) before arming a new
// one — the old stream targets the previous pod.
func Test902_TransitionCancelsExistingGate(t *testing.T) {
	env := newTestEnv(t)
	fc := &gateTestClient{
		connected:    make(chan struct{}, 4),
		disconnected: make(chan struct{}, 4),
	}
	consumer := usagestream.New(usagestream.Config{
		Resolve:   func(context.Context, string) (string, string, error) { return "http://pod", "pw", nil },
		NewClient: func(string, string) usagestream.Client { return fc },
		Logger:    &testLogger{},
		IdleDrop:  time.Hour,
		Retry:     10 * time.Millisecond,
	})
	t.Cleanup(injectUsageStream(consumer))

	env.handler.UsageStream().Open("ws-902t")
	requireGateConnect(t, fc, "pre-transition gate")

	env.handler.SetPriorPhaseForTest("ws-902t", "Resuming")
	ws := &v1.Workspace{}
	ws.Name = "ws-902t"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	select {
	case <-fc.disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("transition into Active must cancel the previous gate's stream (fresh connection to the new pod)")
	}
	requireGateConnect(t, fc, "transition must leave a (new) gate armed")
	require.Equal(t, 1, env.handler.UsageStream().Gates())
}

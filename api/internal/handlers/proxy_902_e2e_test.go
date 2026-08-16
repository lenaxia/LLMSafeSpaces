// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #902/#903 full-wiring tests: real ProxyHandler.Start() → real CRD
// watcher seed → onPhaseChange → tracker connect → agent event relayed
// to a workspace subscriber. These catch wiring deletions the direct
// unit calls cannot (review round 1 on #903): removing the seed path,
// the phaseSource assignment, or the reconciler launch breaks them.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// fakeSSEBackend serves opencode's /event stream: signals every
// connection, and lets tests push events to all live listeners.
type fakeSSEBackend struct {
	*httptest.Server
	mu        sync.Mutex
	conns     int
	listeners map[chan string]struct{}
}

func newFakeSSEBackend() *fakeSSEBackend {
	b := &fakeSSEBackend{listeners: make(map[chan string]struct{})}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			ch := make(chan string, 8)
			b.mu.Lock()
			b.conns++
			b.listeners[ch] = struct{}{}
			b.mu.Unlock()
			defer func() {
				b.mu.Lock()
				b.conns--
				delete(b.listeners, ch)
				b.mu.Unlock()
			}()
			flusher := w.(http.Flusher)
			flusher.Flush()
			for {
				select {
				case evt := <-ch:
					fmt.Fprintf(w, "data: %s\n\n", evt)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	return b
}

func (b *fakeSSEBackend) connectionCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conns
}

func (b *fakeSSEBackend) push(event string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.listeners {
		select {
		case ch <- event:
		default:
		}
	}
}

// e2eLogger captures log lines so e2e failures self-diagnose
// (testLogger swallows everything; the tracker's Warn output is the
// fastest route to why a connection did or didn't happen).
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
// tracker, reconciler) against mock k8s and the fake SSE backend.
// priorPhaseActive simulates the Redis-persisted prior that made
// post-restart seeds skip arming (#902).
// flipGet wraps MockWorkspaceInterface so Get flips to an IP-carrying CR
// once ready flips — race-free dynamic mock behavior (testify re-On
// appends rather than replaces).
type flipGet struct {
	*k8smocks.MockWorkspaceInterface
	ready  *atomic.Bool
	withIP *v1.Workspace
}

func (f *flipGet) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.Workspace, error) {
	if f.ready != nil && f.ready.Load() {
		return f.withIP, nil
	}
	return f.MockWorkspaceInterface.Get(ctx, name, opts)
}

func startFullWiring(t *testing.T, backend *fakeSSEBackend, wsName, podIP, priorPhase string, reconcileInterval time.Duration, ipReady *atomic.Bool) (*ProxyHandler, *k8smocks.MockWorkspaceInterface) {
	t.Helper()
	orig := sseWatchReconcileInterval
	sseWatchReconcileInterval = reconcileInterval
	t.Cleanup(func() { sseWatchReconcileInterval = orig })

	// No client.Timeout — production wires the default client (transport
	// timeouts only); a client.Timeout would kill the long-lived /event
	// stream mid-test, mirroring nothing real.
	transport := &redirectTransport{server: backend.Server}
	httpClient := &http.Client{Transport: transport}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	// The workspace interface the API talks to: the plain mock, or (for
	// the pod-IP-empty unhappy path) a wrapper whose Get flips to an
	// IP-carrying CR atomically.
	var wsIface = interface{}(wsMock)
	if ipReady != nil {
		ipWS := &v1.Workspace{}
		ipWS.Name = wsName
		ipWS.Status.Phase = v1.WorkspacePhaseActive
		ipWS.Status.PodIP = "10.0.0.1"
		wsIface = &flipGet{MockWorkspaceInterface: wsMock, ready: ipReady, withIP: ipWS}
	}
	llmMock.On("Workspaces", "default").Return(wsIface)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	// One mutable CR shared by List/Get: the unhappy-path test flips
	// PodIP on it after the seed (simulating the resume completing).
	wsCR := &v1.Workspace{}
	wsCR.Name = wsName
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
	handler, err := NewProxyHandler(k8sMock, logger, "default", httpClient, nil)
	require.NoError(t, err)
	handler.userBroker = eventbroker.NewUserEventBroker()
	t.Cleanup(func() {
		if t.Failed() {
			logger.dump(t)
		}
	})

	// The #902 precondition: Redis-persisted prior phase already says
	// Active, so the seed's onPhaseChange sees a no-transition update.
	handler.SetPriorPhaseForTest(wsName, priorPhase)

	require.NoError(t, handler.Start())
	t.Cleanup(func() { _ = handler.Stop() })
	return handler, wsMock
}

// Test902_E2E_SeedWithPersistedPriorArmsWatchAndRelaysEvents is the
// full-wiring reproduction of the incident: real Start(), prior phase
// Active (Redis-persisted shape), seed fires onPhaseChange — the watch
// must arm, CONNECT to the backend, and relay an agent event to a
// workspace subscriber. Pre-fix, the seed took the else-branch and none
// of this happened: users halted while sends succeeded.
func Test902_E2E_SeedWithPersistedPriorArmsWatchAndRelaysEvents(t *testing.T) {
	backend := newFakeSSEBackend()
	t.Cleanup(backend.Close) // LIFO: runs after handler.Stop (registered below)

	handler, _ := startFullWiring(t, backend, "ws-902e2e", "10.0.0.1", "Active", time.Hour, nil)

	// The watch armed AND connected despite prior == Active.
	require.Eventually(t, func() bool { return backend.connectionCount() >= 1 },
		10*time.Second, 20*time.Millisecond,
		"seed with Redis-persisted prior=Active must still arm and connect the tracker (#902)")
	assert.True(t, handler.GetSSETracker().IsWatching("ws-902e2e"))

	// A workspace subscriber receives a relayed agent event end-to-end.
	sub, err := handler.userBroker.SubscribeWorkspace("ws-902e2e")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-902e2e", sub)
	backend.push(`{"type":"session.status","properties":{"sessionID":"ses-x","status":{"type":"idle"}}}`)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
		assert.Equal(t, "session.status", evt.EventType)
	case <-time.After(5 * time.Second):
		t.Fatal("agent event never reached the workspace subscriber — tracker connected but not relaying")
	}
}

// Test902_E2E_ReconcilerHealsDeletedWatch proves the Start()-launched
// reconciler wiring: after the watch is torn down (simulating connection
// death with no phase transition), a NEW connection appears within one
// reconcile interval and events flow again. Deleting the phaseSource
// assignment or the goroutine launch in Start() fails this test.
func Test902_E2E_ReconcilerHealsDeletedWatch(t *testing.T) {
	backend := newFakeSSEBackend()
	t.Cleanup(backend.Close) // LIFO: runs after handler.Stop (registered below)

	handler, _ := startFullWiring(t, backend, "ws-902heal", "10.0.0.1", "Active", 100*time.Millisecond, nil)

	require.Eventually(t, func() bool { return backend.connectionCount() >= 1 },
		10*time.Second, 20*time.Millisecond)

	// Kill the watch exactly as a connection death does: no transition,
	// no user stream — nothing but the reconciler can bring it back.
	handler.GetSSETracker().StopWatching("ws-902heal")
	require.Eventually(t, func() bool { return backend.connectionCount() == 0 },
		5*time.Second, 20*time.Millisecond, "sanity: watch torn down, connection closed")

	require.Eventually(t, func() bool { return backend.connectionCount() >= 1 },
		3*time.Second, 20*time.Millisecond,
		"reconciler must re-arm the deleted watch within one interval")

	sub, err := handler.userBroker.SubscribeWorkspace("ws-902heal")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-902heal", sub)
	backend.push(`{"type":"session.status","properties":{"sessionID":"ses-y","status":{"type":"idle"}}}`)
	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("healed watch must relay events")
	}
}

// Test902_E2E_ArmWhilePodIPEmptyThenRecovers: the unhappy path — the
// workspace is Active but podIP is not yet populated (resume race). The
// watch arms, connectAndRead fails (Warn + backoff), and once the IP
// appears the retry loop connects and events flow.
func Test902_E2E_ArmWhilePodIPEmptyThenRecovers(t *testing.T) {
	backend := newFakeSSEBackend()
	t.Cleanup(backend.Close) // LIFO: runs after handler.Stop (registered below)

	ipReady := &atomic.Bool{}
	handler, _ := startFullWiring(t, backend, "ws-902ip", "", "Creating", time.Hour, ipReady)

	// Armed but cannot connect yet.
	require.Eventually(t, func() bool { return handler.GetSSETracker().IsWatching("ws-902ip") },
		5*time.Second, 20*time.Millisecond)
	assert.Zero(t, backend.connectionCount(), "sanity: no pod IP yet, no connection possible")

	// Pod IP appears (resume completes). The wrapper's Get flips
	// atomically (re-On-ing the k8s mock APPENDS an expectation — the old
	// IP-less one keeps winning; mutating the shared CR races the
	// tracker's reads). The subscribe loop's retry (2s initial backoff,
	// growing) connects once the IP is visible.
	ipReady.Store(true)

	require.Eventually(t, func() bool { return backend.connectionCount() >= 1 },
		15*time.Second, 50*time.Millisecond,
		"tracker retry loop must connect once the pod IP becomes available")

	sub, err := handler.userBroker.SubscribeWorkspace("ws-902ip")
	require.NoError(t, err)
	defer handler.userBroker.UnsubscribeWorkspace("ws-902ip", sub)
	backend.push(`{"type":"session.status","properties":{"sessionID":"ses-z","status":{"type":"idle"}}}`)
	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("recovered watch must relay events")
	}
}

// Test902_TransitionCancelsExistingWatch pins the fresh-connection
// semantics with a REAL cancel observation: a transition into Active
// must StopWatching (cancel) the previous subscription before arming a
// new one. Uses ForceWatchingWithCancelForTest so the cancel is
// observable (review round 1 case 5).
func Test902_TransitionCancelsExistingWatch(t *testing.T) {
	env := newTestEnv(t)
	env.handler.sseTracker = sse.NewTracker(nil, &testLogger{}, env.handler.onSessionIdle)

	canceled := make(chan struct{}, 1)
	env.handler.GetSSETracker().ForceWatchingWithCancelForTest("ws-902t", func() {
		select {
		case canceled <- struct{}{}:
		default:
		}
	})
	env.handler.SetPriorPhaseForTest("ws-902t", "Resuming")

	ws := &v1.Workspace{}
	ws.Name = "ws-902t"
	ws.Status.Phase = v1.WorkspacePhaseActive
	env.handler.onPhaseChange(ws)

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("transition into Active must cancel the previous watch (fresh connection to the new pod)")
	}
	assert.True(t, env.handler.GetSSETracker().IsWatching("ws-902t"),
		"and leave a (new) watch armed")
}

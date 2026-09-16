// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- #761 test fixtures -----------------------------------------------------
//
// The drain consults agentd's /v1/statusz on the admin mux. Tests wire a
// local httptest server by overriding agentdAdminPort (same pattern as
// setupHealthTest in health_test.go) and pointing Status.PodIP at
// 127.0.0.1. statuszStub lets each test script the busy/idle/unhealthy
// sequence and count statusz hits (asserting which paths consult the drain).

type statuszStub struct {
	mu     sync.Mutex
	resp   agentd.StatuszResponse
	calls  int  // /v1/statusz hits only
	gate   bool // require Bearer auth on statusz (models gated agentd)
	unwell bool // healthz reports unhealthy (drives the restart path)
}

func (s *statuszStub) set(resp agentd.StatuszResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resp = resp
}

// gateAuth makes the stub reject unauthenticated statusz scrapes with 401,
// modeling a production gated agentd (the password-secret-missing path).
func (s *statuszStub) gateAuth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = true
}

// makeHealthzUnhealthy scripts the healthz endpoint to report an unhealthy
// agent (drives the health-check restart path).
func (s *statuszStub) makeHealthzUnhealthy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unwell = true
}

func (s *statuszStub) statuszCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *statuszStub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/statusz" {
		s.mu.Lock()
		s.calls++
		resp := s.resp
		gate := s.gate
		s.mu.Unlock()
		if gate && r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	// Non-statusz endpoints (healthz in the drain tests) — scriptable
	// healthy/unhealthy so the health-restart path can be driven.
	s.mu.Lock()
	unwell := s.unwell
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{Healthy: !unwell})
}

// startStatuszAgent starts a stub agentd admin mux on 127.0.0.1 and points
// the package's agentdAdminPort at it for the duration of the test.
func startStatuszAgent(t *testing.T, stub *statuszStub) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: http.HandlerFunc(stub.serve)},
	}
	server.Start()
	t.Cleanup(server.Close)

	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	origAdmin := agentdAdminPort
	var ok bool
	agentdAdminPort, ok = atoi(portStr)
	require.True(t, ok, "bad stub port %q", portStr)
	t.Cleanup(func() { agentdAdminPort = origAdmin })
}

func atoi(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// startClosedPort grabs then closes a TCP port so dialing it fails fast
// (connection refused) — models an unreachable agentd.
func startClosedPort(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	portStr := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	origAdmin := agentdAdminPort
	agentdAdminPort = portStr
	t.Cleanup(func() { agentdAdminPort = origAdmin })
}

// reconcilerForDrain builds a reconciler with a FakeRecorder for event
// assertions. The variadic objects mirror reconcilerFor's runtime.Object
// list; kept separate so the drain tests always have a Recorder.
func reconcilerForDrain(t *testing.T, objs ...runtime.Object) (*WorkspaceReconciler, *record.FakeRecorder) {
	t.Helper()
	r := reconcilerFor(t, objs...)
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec
	return r, rec
}

// eventsFrom drains the FakeRecorder channel without blocking.
func eventsFrom(rec *record.FakeRecorder) []string {
	out := []string{}
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	for _, e := range events {
		if strings.Contains(e, reason) {
			return true
		}
	}
	return false
}

// makeSuspendingWorkspace builds a Suspending workspace plus its running pod.
func makeSuspendingWorkspace(name string) (*v1.Workspace, *corev1.Pod) {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseSuspending)
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.PodName = podName(name, string(ws.UID))
	ws.Status.PodNamespace = "default"
	pod := makeRunningPod(podName(name, string(ws.UID)), "default", "127.0.0.1")
	return ws, pod
}

func busyStatusz(contextUsed int64) agentd.StatuszResponse {
	return agentd.StatuszResponse{
		Healthy:             true,
		Ready:               true,
		Connected:           []string{"opencode"},
		ProvidersConfigured: 1,
		SessionsActive:      1,
		Sessions: []agentd.SessionInfo{
			{ID: "ses-busy", Status: "busy", ContextUsed: contextUsed},
		},
	}
}

func idleStatusz() agentd.StatuszResponse {
	return agentd.StatuszResponse{
		Healthy:             true,
		Ready:               true,
		Connected:           []string{"opencode"},
		ProvidersConfigured: 1,
		SessionsActive:      0,
		Sessions:            []agentd.SessionInfo{{ID: "ses-idle", Status: "idle"}},
	}
}

func podExists(t *testing.T, r *WorkspaceReconciler, name string) bool {
	t.Helper()
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &corev1.Pod{})
	if err == nil {
		return true
	}
	require.True(t, apierrors.IsNotFound(err), "unexpected error: %v", err)
	return false
}

// --- Drain decision matrix ---

// TestDrain_IdleAgentProceedsImmediately: no busy sessions → the deletion
// path runs unchanged (pod deleted, phase transition committed) and no
// drain state survives.
func TestDrain_IdleAgentProceedsImmediately(t *testing.T) {
	stub := &statuszStub{resp: idleStatusz()}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-idle")
	r, rec := reconcilerForDrain(t, ws, pod)

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-idle", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "idle agent must not requeue for drain")

	assert.False(t, podExists(t, r, pod.Name), "pod must be deleted when sessions are idle")
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-drain-idle", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)

	events := eventsFrom(rec)
	assert.False(t, hasEvent(events, "SessionDrainDeferred"), "no defer event expected")
	assert.False(t, hasEvent(events, "SessionDrainForced"), "no force event expected")

	r.drainStatesMu.Lock()
	assert.Empty(t, r.drainStates, "drain state must be cleared after proceeding")
	r.drainStatesMu.Unlock()
}

// TestDrain_BusyAgentDefersDeletion: busy sessions → pod survives, phase
// stays Suspending, requeue is the drain poll interval, one defer event and
// one deferred metric increment are emitted.
func TestDrain_BusyAgentDefersDeletion(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-busy")
	r, rec := reconcilerForDrain(t, ws, pod)

	before := counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonSuspend)

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-busy", "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "busy agent must requeue at the drain poll interval")

	assert.True(t, podExists(t, r, pod.Name), "pod must survive while sessions are busy")
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-drain-busy", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspending, updated.Status.Phase, "phase must stay Suspending while draining")

	events := eventsFrom(rec)
	assert.True(t, hasEvent(events, "SessionDrainDeferred"), "defer event expected, got %v", events)

	// Second poll still busy → still defers (no duplicate event, no duplicate metric).
	result, err = r.Reconcile(context.Background(), reqFor("ws-drain-busy", "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter)
	assert.True(t, podExists(t, r, pod.Name))

	after := counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonSuspend)
	assert.Equal(t, before+1, after, "deferred metric increments once per drain window, not per poll")
	assert.Len(t, eventsFrom(rec), 0, "subsequent polls must not re-emit the defer event")
}

// TestDrain_BusyThenIdle_ProceedsOnNextPoll: the drain resolves as soon as
// the busy session goes idle.
func TestDrain_BusyThenIdle_ProceedsOnNextPoll(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-flip")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-flip", "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, pod.Name))

	stub.set(idleStatusz())
	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-flip", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, podExists(t, r, pod.Name), "pod must be deleted once sessions are idle")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-drain-flip", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
}

// TestDrain_ProgressExtendsWindow: observable progress (growing ContextUsed
// — the statusz-visible proxy for #1342's part-level progress signal) resets
// the stall clock, so a long but active turn is never force-killed by
// wall-clock alone.
func TestDrain_ProgressExtendsWindow(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-progress")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-progress", "default"))
	require.NoError(t, err)

	// Simulate the turn having run (and last progressed) long ago — the
	// window is far beyond the stall bound already.
	r.drainStatesMu.Lock()
	st := r.drainStates[drainKey(ws)]
	require.NotNil(t, st)
	st.lastProgressAt = time.Now().Add(-2 * drainStallBound)
	st.startedAt = time.Now().Add(-3 * drainStallBound)
	r.drainStatesMu.Unlock()

	// New tokens streamed → progress observed → window resets → defer.
	stub.set(busyStatusz(5000))
	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-progress", "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "progress must extend the drain window")
	assert.True(t, podExists(t, r, pod.Name))

	r.drainStatesMu.Lock()
	assert.Less(t, time.Since(st.lastProgressAt), drainStallBound, "progress must reset lastProgressAt")
	r.drainStatesMu.Unlock()
}

// TestDrain_StalledBeyondBoundForces: busy sessions with no observable
// progress beyond the stall bound → deletion proceeds with a warning event
// and a forced metric.
func TestDrain_StalledBeyondBoundForces(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-stall")
	r, rec := reconcilerForDrain(t, ws, pod)

	before := counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonSuspend)

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-stall", "default"))
	require.NoError(t, err)
	require.True(t, podExists(t, r, pod.Name))

	// Freeze the snapshot (same statusz response) and time-travel the last
	// progress marker past the bound.
	r.drainStatesMu.Lock()
	st := r.drainStates[drainKey(ws)]
	require.NotNil(t, st)
	st.lastProgressAt = time.Now().Add(-2 * drainStallBound)
	r.drainStatesMu.Unlock()

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-stall", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "stalled sessions must not requeue")
	assert.False(t, podExists(t, r, pod.Name), "stalled drain must force the deletion")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-drain-stall", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)

	assert.True(t, hasEvent(eventsFrom(rec), "SessionDrainForced"), "force event expected")
	after := counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonSuspend)
	assert.Equal(t, before+1, after)
}

// TestDrain_UnreachableAgentFailsOpen: statusz transport failure must never
// block a deletion — the drain fails open with an event and metric.
func TestDrain_UnreachableAgentFailsOpen(t *testing.T) {
	startClosedPort(t)
	ws, pod := makeSuspendingWorkspace("ws-drain-dead")
	r, rec := reconcilerForDrain(t, ws, pod)

	before := counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonSuspend)

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-dead", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, podExists(t, r, pod.Name), "unreachable agentd must fail open (delete proceeds)")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-drain-dead", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)

	assert.True(t, hasEvent(eventsFrom(rec), "SessionDrainFailedOpen"), "fail-open event expected")
	after := counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonSuspend)
	assert.Equal(t, before+1, after)
}

// TestDrain_UnhealthyStatuszFailsOpen: an agentd that responds but reports
// itself unhealthy must not be trusted for busy state — proceed.
func TestDrain_UnhealthyStatuszFailsOpen(t *testing.T) {
	stub := &statuszStub{resp: agentd.StatuszResponse{Healthy: false, SessionsActive: 1}}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-unhealthy")
	r, _ := reconcilerForDrain(t, ws, pod)

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-unhealthy", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, podExists(t, r, pod.Name), "unhealthy agentd reports must fail open")
}

// TestDrain_EmptyPodIPSkipsConsult: without a PodIP there is no agent to
// consult — proceed without touching statusz.
func TestDrain_EmptyPodIPSkipsConsult(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-noip")
	ws.Status.PodIP = ""
	r, _ := reconcilerForDrain(t, ws, pod)

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-noip", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, podExists(t, r, pod.Name))
	assert.Zero(t, stub.statuszCalls(), "statusz must not be consulted without a PodIP")
}

// --- Per-path integration ---

// TestDrain_SuspendRequestFlowDefers: a full user-suspend flow (Spec.Suspend
// → Suspending → drain) keeps the pod alive while busy and completes once
// idle.
func TestDrain_SuspendRequestFlowDefers(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-suspend-flow", "default", v1.WorkspacePhaseActive)
	trueVal := true
	ws.Spec.Suspend = &trueVal
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	// First reconcile: transition to Suspending (pod untouched).
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	// Second reconcile: handleSuspending drains — busy → defer.
	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter)
	assert.True(t, podExists(t, r, pod.Name), "suspend must not delete the pod mid-turn")

	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name))

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Nil(t, updated.Spec.Suspend, "suspend request must be acknowledged")
}

// TestDrain_RestartGenerationDefers: the restart-generation bump defers
// while busy — the workspace stays Active and ObservedRestartGeneration is
// NOT advanced (the bump is re-consumed after the drain).
func TestDrain_RestartGenerationDefers(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-restartgen", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "restart-gen bump must drain busy sessions")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase, "workspace stays Active while draining")
	assert.Equal(t, int64(1), updated.Status.ObservedRestartGeneration, "generation must not be observed mid-drain")
	assert.True(t, podExists(t, r, pod.Name), "pod must survive the deferred restart-gen bump")

	// Turn finishes → restart proceeds.
	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name))

	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.ObservedRestartGeneration)
}

// TestDrain_ArchDriftDefers: the architecture-drift recycle defers while
// sessions are busy.
func TestDrain_ArchDriftDefers(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-arch", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pod.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "arch-drift recycle must drain busy sessions")
	assert.True(t, podExists(t, r, pod.Name))

	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name))

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
}

// TestDrain_PasswordSecretMissingFailsOpen: with the password Secret gone
// the statusz consult cannot authenticate — the drain must fail open and
// the recycle proceeds (self-heal path, not a user-visible restart).
func TestDrain_PasswordSecretMissingFailsOpen(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	stub.gateAuth() // no password Secret → unauthenticated scrape → 401
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-pwmissing", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	// No password secret in the fixture.
	r, _ := reconcilerForDrain(t, ws, pod)

	before := counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonPasswordSecretMissing)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "missing-secret recycle must fail open, not defer")
	assert.False(t, podExists(t, r, pod.Name))

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)

	after := counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonPasswordSecretMissing)
	assert.Equal(t, before+1, after)
}

// TestDrain_TerminateDoesNotConsultDrain: explicit user destroy is immediate
// — no statusz consult, no deferral.
func TestDrain_TerminateDoesNotConsultDrain(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-terminate", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PVCName = "workspace-ws-drain-terminate"
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	r, _ := reconcilerForDrain(t, ws, pod)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "terminate must never drain")
	assert.False(t, podExists(t, r, pod.Name))
	assert.Zero(t, stub.statuszCalls(), "terminate must not consult statusz")

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase)
}

// TestDrain_HealthRestartSkipsDrain: the health-check restart fires without
// consulting statusz — by the time agentd has failed healthz three times the
// drain could at best fail open and at worst delay recovery.
func TestDrain_HealthRestartSkipsDrain(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-health", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.ConsecutiveHealthFailures = 2
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	// Force the healthz endpoint to report unhealthy so the third
	// consecutive failure triggers restartAgentPod inside this reconcile.
	stub.makeHealthzUnhealthy()
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	assert.Zero(t, stub.statuszCalls(), "health restart must not consult the drain")

	assert.False(t, podExists(t, r, pod.Name), "health restart must delete the pod")
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)

	r.drainStatesMu.Lock()
	_, hasState := r.drainStates[drainKey(ws)]
	r.drainStatesMu.Unlock()
	assert.False(t, hasState, "health restart must not create drain state")
}

// TestDrain_TerminatingClearsDrainState: the in-memory drain window does not
// outlive the workspace.
func TestDrain_TerminatingClearsDrainState(t *testing.T) {
	stub := &statuszStub{resp: idleStatusz()}
	startStatuszAgent(t, stub)
	ws := makeWorkspace("ws-drain-cleanup", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PVCName = "workspace-ws-drain-cleanup"
	r, _ := reconcilerForDrain(t, ws)

	r.drainStatesMu.Lock()
	if r.drainStates == nil {
		r.drainStates = make(map[string]*podDrainState)
	}
	r.drainStates[drainKey(ws)] = &podDrainState{
		startedAt:      time.Now(),
		lastProgressAt: time.Now(),
	}
	r.drainStatesMu.Unlock()

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-cleanup", "default"))
	require.NoError(t, err)

	r.drainStatesMu.Lock()
	_, hasState := r.drainStates[drainKey(ws)]
	r.drainStatesMu.Unlock()
	assert.False(t, hasState, "terminate must clean up drain state")
}

// TestDrain_BusySetChangeIsProgress: a session going idle while another goes
// busy (turn churn) counts as observable progress — the window resets.
func TestDrain_BusySetChangeIsProgress(t *testing.T) {
	first := busyStatusz(100)
	first.Sessions = []agentd.SessionInfo{{ID: "ses-a", Status: "busy", ContextUsed: 100}}
	second := busyStatusz(100)
	second.Sessions = []agentd.SessionInfo{{ID: "ses-b", Status: "busy", ContextUsed: 100}}

	stub := &statuszStub{resp: first}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-churn")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-churn", "default"))
	require.NoError(t, err)

	r.drainStatesMu.Lock()
	st := r.drainStates[drainKey(ws)]
	require.NotNil(t, st)
	st.lastProgressAt = time.Now().Add(-2 * drainStallBound)
	r.drainStatesMu.Unlock()

	stub.set(second)
	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-churn", "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter, "busy-set churn must count as progress")
	assert.True(t, podExists(t, r, pod.Name))
}

// TestDrain_PodRecreationResetsWindow: opencode session IDs survive pod
// recreation (the session DB lives on the PVC), so a new pod's busy set may
// match the old pod's snapshot byte-for-byte. The drain window must reset on
// pod identity change — never inherit the dead pod's progress age and
// force-kill the new turn.
func TestDrain_PodRecreationResetsWindow(t *testing.T) {
	snap := busyStatusz(100) // same session ID + same ContextUsed on both pods

	stub := &statuszStub{resp: snap}
	startStatuszAgent(t, stub)
	ws, pod := makeSuspendingWorkspace("ws-drain-recreated")
	r, _ := reconcilerForDrain(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-drain-recreated", "default"))
	require.NoError(t, err)

	// Age the old pod's window far past the stall bound and mark it as
	// belonging to the OLD pod's IP — the workspace now runs a new pod
	// (same session IDs: the session DB lives on the PVC) whose busy
	// snapshot is byte-for-byte identical.
	r.drainStatesMu.Lock()
	st := r.drainStates[drainKey(ws)]
	require.NotNil(t, st)
	st.lastProgressAt = time.Now().Add(-2 * drainStallBound)
	st.startedAt = time.Now().Add(-3 * drainStallBound)
	st.podIP = "192.0.2.1" // the dead pod's address
	r.drainStatesMu.Unlock()

	result, err := r.Reconcile(context.Background(), reqFor("ws-drain-recreated", "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"a fresh pod's identical busy snapshot must start a fresh window, not inherit the old pod's progress age")
	assert.True(t, podExists(t, r, pod.Name))

	r.drainStatesMu.Lock()
	st = r.drainStates[drainKey(ws)]
	require.NotNil(t, st)
	assert.Equal(t, "127.0.0.1", st.podIP, "window must be pinned to the new pod identity")
	assert.Less(t, time.Since(st.lastProgressAt), drainStallBound)
	r.drainStatesMu.Unlock()
}

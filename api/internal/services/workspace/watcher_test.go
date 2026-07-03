// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	testTimeout      = 2 * time.Second
	testPollInterval = 50 * time.Millisecond
)

func setupWatcherMocks(t *testing.T) (*k8smocks.MockKubernetesClient, *k8smocks.MockWorkspaceInterface, *watch.FakeWatcher) {
	k8s := k8smocks.NewMockKubernetesClient()
	llm := k8smocks.NewMockLLMSafespacesV1Interface()
	ws := k8smocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(llm, nil)
	llm.On("Workspaces", "default").Return(ws)
	fakeWatch := watch.NewFake()
	ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil).Maybe()
	ws.On("Watch", mock.Anything, mock.Anything).Return(fakeWatch, nil).Maybe()
	return k8s, ws, fakeWatch
}

func TestWorkspaceWatcher_NilCallback_ReturnsError(t *testing.T) {
	k8s, _, _ := setupWatcherMocks(t)
	_, err := NewWatcher(k8s, &testLogger{}, "default", nil)
	assert.Error(t, err)
}

func TestWorkspaceWatcher_GetKnownPhase_Empty(t *testing.T) {
	k8s, _, _ := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)

	_, ok := w.GetKnownPhase("nonexistent")
	assert.False(t, ok)
}

func TestWorkspaceWatcher_PhaseChangeCallback(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)

	var callbackCalled atomic.Bool
	callback := func(workspace *v1.Workspace) {
		callbackCalled.Store(true)
	}

	w, err := NewWatcher(k8s, &testLogger{}, "default", callback)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	defer w.Stop()

	// Send a workspace event
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	fakeWatch.Add(ws)

	// Then modify it to trigger phase change
	ws2 := ws.DeepCopy()
	ws2.Status.Phase = v1.WorkspacePhaseSuspending
	ws2.ResourceVersion = "2"
	fakeWatch.Modify(ws2)

	assert.Eventually(t, func() bool { return callbackCalled.Load() }, testTimeout, testPollInterval)
}

func TestWorkspaceWatcher_SeedResourceVersion_PopulatesKnownPhases(t *testing.T) {
	k8s := k8smocks.NewMockKubernetesClient()
	llm := k8smocks.NewMockLLMSafespacesV1Interface()
	ws := k8smocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(llm, nil)
	llm.On("Workspaces", "default").Return(ws)

	ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		ListMeta: metav1.ListMeta{ResourceVersion: "100"},
		Items: []v1.Workspace{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-1"},
				Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1"}},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-2"},
				Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-2"}},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
		},
	}, nil)

	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	w.SetUserBroker(broker)

	err = w.seedResourceVersion()
	require.NoError(t, err)

	// Verify knownPhases populated
	phase, ok := w.GetKnownPhase("ws-1")
	assert.True(t, ok)
	assert.Equal(t, "Active", phase)

	phase, ok = w.GetKnownPhase("ws-2")
	assert.True(t, ok)
	assert.Equal(t, "Suspended", phase)

	// Verify broker ownership recorded
	assert.Equal(t, "user-1", broker.WorkspaceOwner("ws-1"))
	assert.Equal(t, "user-2", broker.WorkspaceOwner("ws-2"))
}

func TestWorkspaceWatcher_HandleEvent_Deleted(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)

	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)

	broker := eventbroker.NewUserEventBroker()
	w.SetUserBroker(broker)

	require.NoError(t, w.Start())
	defer w.Stop()

	// Add a workspace so it's known
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-del", ResourceVersion: "1"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-del"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	fakeWatch.Add(ws)

	assert.Eventually(t, func() bool {
		_, ok := w.GetKnownPhase("ws-del")
		return ok
	}, testTimeout, testPollInterval)

	// Manually record ownership (normally done by seedResourceVersion)
	broker.RecordWorkspaceOwner("ws-del", "user-del")

	// Delete the workspace
	fakeWatch.Delete(ws)

	assert.Eventually(t, func() bool {
		_, ok := w.GetKnownPhase("ws-del")
		return !ok
	}, testTimeout, testPollInterval)

	// Verify broker ownership cleaned up. The delete handler clears
	// knownPhases first, then calls CleanupWorkspace after releasing the
	// mutex (watcher.go:306-316), so the broker cleanup can lag the phase
	// cleanup — poll for it rather than asserting immediately.
	assert.Eventually(t, func() bool {
		return broker.WorkspaceOwner("ws-del") == ""
	}, testTimeout, testPollInterval)
}

func TestWorkspaceWatcher_GetAllKnownPhases(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)

	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	defer w.Stop()

	// Add two workspaces
	fakeWatch.Add(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-a", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	})
	fakeWatch.Add(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-b", ResourceVersion: "2"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
	})

	assert.Eventually(t, func() bool {
		phases := w.GetAllKnownPhases()
		return len(phases) >= 2
	}, testTimeout, testPollInterval)

	phases := w.GetAllKnownPhases()
	assert.Equal(t, "Active", phases["ws-a"])
	assert.Equal(t, "Suspended", phases["ws-b"])

	// Verify it's a copy — mutating doesn't affect watcher
	phases["ws-a"] = "Terminated"
	realPhase, _ := w.GetKnownPhase("ws-a")
	assert.Equal(t, "Active", realPhase)
}

func TestWorkspaceWatcher_HandleEvent_PhaseTransitionMetricRecorded(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}

	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	defer w.Stop()

	// Seed initial phase via a first Add event.
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-metric", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
	}
	fakeWatch.Add(ws)
	assert.Eventually(t, func() bool {
		_, ok := w.GetKnownPhase("ws-metric")
		return ok
	}, testTimeout, testPollInterval)

	before := gatherPhaseTransitionCount(t, "Creating", "Active")

	// Modify to Active — should fire the metric.
	ws2 := ws.DeepCopy()
	ws2.Status.Phase = v1.WorkspacePhaseActive
	ws2.ResourceVersion = "2"
	fakeWatch.Modify(ws2)

	assert.Eventually(t, func() bool {
		return gatherPhaseTransitionCount(t, "Creating", "Active") > before
	}, testTimeout, testPollInterval)
}

func TestWorkspaceWatcher_HandleEvent_SamePhase_NoMetric(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}

	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	defer w.Stop()

	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-same", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	fakeWatch.Add(ws)
	assert.Eventually(t, func() bool {
		_, ok := w.GetKnownPhase("ws-same")
		return ok
	}, testTimeout, testPollInterval)

	before := gatherPhaseTransitionCount(t, "Active", "Active")

	// Modify with the same phase — must not increment the metric.
	ws2 := ws.DeepCopy()
	ws2.ResourceVersion = "2" // different RV, same phase
	fakeWatch.Modify(ws2)
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, before, gatherPhaseTransitionCount(t, "Active", "Active"))
}

// TestWorkspaceWatcher_SeedResourceVersion_CallsOnPhaseChangeForActiveWorkspaces verifies that
// seedResourceVersion calls onPhaseChange for every Active workspace discovered during the
// initial List. This is the fix for the SSE tracker startup race: without this call, the
// EnsureWatching loop in proxy_lifecycle.Start() runs against an empty knownPhases map (because
// watcher.Start() is async) and the SSE tracker never connects to already-Active workspaces.
func TestWorkspaceWatcher_SeedResourceVersion_CallsOnPhaseChangeForActiveWorkspaces(t *testing.T) {
	k8s := k8smocks.NewMockKubernetesClient()
	llm := k8smocks.NewMockLLMSafespacesV1Interface()
	wsi := k8smocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(llm, nil)
	llm.On("Workspaces", "default").Return(wsi)

	wsi.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-active-1"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-active-2"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-suspended"},
				Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended},
			},
		},
	}, nil)

	var called []string
	callback := func(ws *v1.Workspace) {
		called = append(called, ws.Name)
	}

	w, err := NewWatcher(k8s, &testLogger{}, "default", callback)
	require.NoError(t, err)

	require.NoError(t, w.seedResourceVersion())

	// Only Active workspaces must have triggered onPhaseChange.
	assert.Len(t, called, 2)
	assert.Contains(t, called, "ws-active-1")
	assert.Contains(t, called, "ws-active-2")
	assert.NotContains(t, called, "ws-suspended")
}

// TestWorkspaceWatcher_SeedResourceVersion_NonActiveNoCallback verifies that non-Active
// workspaces discovered during seeding do not trigger onPhaseChange.
func TestWorkspaceWatcher_SeedResourceVersion_NonActiveNoCallback(t *testing.T) {
	k8s := k8smocks.NewMockKubernetesClient()
	llm := k8smocks.NewMockLLMSafespacesV1Interface()
	wsi := k8smocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(llm, nil)
	llm.On("Workspaces", "default").Return(wsi)

	wsi.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-suspended"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-creating"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating}},
			{ObjectMeta: metav1.ObjectMeta{Name: "ws-failed"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseFailed}},
		},
	}, nil)

	var called []string
	w, err := NewWatcher(k8s, &testLogger{}, "default", func(ws *v1.Workspace) {
		called = append(called, ws.Name)
	})
	require.NoError(t, err)
	require.NoError(t, w.seedResourceVersion())

	assert.Empty(t, called, "no onPhaseChange for non-Active workspaces during seed")
}

func gatherPhaseTransitionCount(t *testing.T, from, to string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "llmsafespaces_workspace_phase_transitions_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make(map[string]string)
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["from_phase"] == from && labels["to_phase"] == to {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestWorkspaceWatcher_WorkspaceUpdateCallback_FiresOnModify verifies
// the callback wiring for the watcher-driven auto-push (worklog 0591).
// A Modified event MUST invoke onWorkspaceUpdate with the current
// workspace object. Without this test, a future refactor that
// accidentally skipped the callback (or reordered event handling in
// a way that hid it) would silently break the auto-push feature.
func TestWorkspaceWatcher_WorkspaceUpdateCallback_FiresOnModify(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)

	var callbackCount int32
	var lastSeen *v1.Workspace
	var mu sync.Mutex

	w, err := NewWatcher(k8s, &testLogger{}, "default", func(*v1.Workspace) {})
	require.NoError(t, err)
	w.SetWorkspaceUpdateCallback(func(ws *v1.Workspace) {
		mu.Lock()
		defer mu.Unlock()
		atomic.AddInt32(&callbackCount, 1)
		lastSeen = ws
	})
	require.NoError(t, w.Start())
	defer w.Stop()

	// Seed with an Add.
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-update-cb", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	fakeWatch.Add(ws)
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&callbackCount) >= 1
	}, testTimeout, testPollInterval, "callback MUST fire on Added event")

	// Modify with a status change (UserCredsPresent set).
	ws2 := ws.DeepCopy()
	ucp := false
	ws2.Status.UserCredsPresent = &ucp
	ws2.ResourceVersion = "2"
	fakeWatch.Modify(ws2)

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&callbackCount) >= 2
	}, testTimeout, testPollInterval, "callback MUST fire on Modified event")

	mu.Lock()
	got := lastSeen
	mu.Unlock()
	require.NotNil(t, got)
	require.NotNil(t, got.Status.UserCredsPresent,
		"callback MUST receive the workspace with its full status "+
			"including UserCredsPresent (auto-push consumer filters on it)")
	assert.Equal(t, false, *got.Status.UserCredsPresent)
}

// TestWorkspaceWatcher_WorkspaceUpdateCallback_NotInvokedForDelete
// pins the Deleted-event contract: onWorkspaceUpdate is for extant
// workspaces only. Firing on Delete would surface a stale workspace
// object to the consumer whose downstream lookups (bindings check,
// DEK fetch) would race the actual cleanup.
func TestWorkspaceWatcher_WorkspaceUpdateCallback_NotInvokedForDelete(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)

	var callbackCount int32

	w, err := NewWatcher(k8s, &testLogger{}, "default", func(*v1.Workspace) {})
	require.NoError(t, err)
	w.SetWorkspaceUpdateCallback(func(_ *v1.Workspace) {
		atomic.AddInt32(&callbackCount, 1)
	})
	require.NoError(t, w.Start())
	defer w.Stop()

	// Seed with Add so the callback fires once.
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-delete", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	fakeWatch.Add(ws)
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&callbackCount) >= 1
	}, testTimeout, testPollInterval)

	before := atomic.LoadInt32(&callbackCount)

	// Delete event MUST NOT invoke the callback.
	ws2 := ws.DeepCopy()
	ws2.ResourceVersion = "2"
	fakeWatch.Delete(ws2)

	// Give the watch goroutine time to process the Delete + verify
	// callback count did not increase.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, atomic.LoadInt32(&callbackCount),
		"Delete event MUST NOT invoke onWorkspaceUpdate — "+
			"consumers filter on Active phase and expect the workspace to "+
			"exist for downstream lookups")
}

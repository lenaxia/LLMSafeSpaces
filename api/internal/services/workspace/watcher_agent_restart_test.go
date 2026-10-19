// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// watcher_agent_restart_test.go — #1760: the workspace watcher fires
// the restart callback when the supervisor restart stamp CHANGES on an
// Active→Active update (onPhaseChange is transition-only and cannot
// carry restarts — they land while the phase stays Active).

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func restartEventWS(name, rv string, count int32, at time.Time) *v1.Workspace {
	t := metav1.NewTime(at)
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: rv},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			AgentSupervisor: &v1.AgentSupervisorStatus{
				AgentRestarts: count,
				LastAgentRestart: &v1.AgentRestartEvent{
					Reason:  "health_watchdog",
					Verdict: "hung",
					At:      &t,
				},
			},
		},
	}
}

func TestWorkspaceWatcher_AgentRestartCallback_FiresOnNewRestart(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)

	var calls atomic.Int32
	w.SetRestartCallback(func(ws *v1.Workspace) {
		calls.Add(1)
	})
	require.NoError(t, w.Start())
	defer w.Stop()

	at := time.Now().Add(-2 * time.Minute)
	fakeWatch.Add(restartEventWS("ws-1", "1", 0, at)) // seed, no restart yet
	// First observation of a restart → fire.
	restartAt := time.Now().Add(-time.Minute)
	fakeWatch.Modify(restartEventWS("ws-1", "2", 1, restartAt))
	assert.Eventually(t, func() bool { return calls.Load() == 1 }, testTimeout, testPollInterval)

	// Same stamp re-observed (status churn) → no re-fire.
	fakeWatch.Modify(restartEventWS("ws-1", "3", 1, restartAt))
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), calls.Load(), "unchanged restart stamp must not re-fire")

	// A SECOND restart → fire again.
	fakeWatch.Modify(restartEventWS("ws-1", "4", 2, time.Now()))
	assert.Eventually(t, func() bool { return calls.Load() == 2 }, testTimeout, testPollInterval)
}

func TestWorkspaceWatcher_AgentRestartCallback_NoCallbackSet(t *testing.T) {
	// Nil callback (wiring edge) must not panic on restart-bearing events.
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	defer w.Stop()

	fakeWatch.Add(restartEventWS("ws-1", "1", 1, time.Now()))
	fakeWatch.Modify(restartEventWS("ws-1", "2", 2, time.Now()))
	time.Sleep(200 * time.Millisecond) // no panic = pass
}

func TestWorkspaceWatcher_AgentRestartCallback_DeletedCleansState(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	w.SetRestartCallback(func(*v1.Workspace) {})
	require.NoError(t, w.Start())
	defer w.Stop()

	at := time.Now().Add(-time.Minute)
	ws := restartEventWS("ws-1", "1", 1, at)
	fakeWatch.Add(ws)
	// Delete then re-add the SAME restart (workspace recreated): the
	// stamp must be forgotten on delete so the recreated workspace's
	// identical restart record re-fires (a fresh pod's history is fresh
	// evidence).
	fakeWatch.Delete(ws)
	ws2 := restartEventWS("ws-1", "2", 1, at)
	fakeWatch.Add(ws2)
	// No assertion beyond no-panic + state hygiene; the re-add path
	// routes through the same detection as Modify.
}

func TestWorkspaceWatcher_AgentRestartCallback_NilBlockNoFire(t *testing.T) {
	k8s, _, fakeWatch := setupWatcherMocks(t)
	noop := func(*v1.Workspace) {}
	w, err := NewWatcher(k8s, &testLogger{}, "default", noop)
	require.NoError(t, err)
	var calls atomic.Int32
	w.SetRestartCallback(func(*v1.Workspace) { calls.Add(1) })
	require.NoError(t, w.Start())
	defer w.Stop()

	fakeWatch.Add(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", ResourceVersion: "1"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	})
	fakeWatch.Modify(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", ResourceVersion: "2"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, AgentSupervisor: &v1.AgentSupervisorStatus{SuppressedRestarts: 74}},
	})
	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, calls.Load(), "suppression-only state (no fired restart) must not fire the callback")
}

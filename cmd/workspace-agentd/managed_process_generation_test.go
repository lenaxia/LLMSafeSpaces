// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Supervisor generation-hook tests (design 0050 D2): onChildStarted must
// fire exactly once per child process — first boot, operator restart(),
// and crash recovery — using the fake-opencode harness from
// managed_process_test.go.

func TestManagedProcess_OnChildStarted_FiresPerGeneration(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)

	var calls atomic.Int64
	var mu sync.Mutex
	generations := &sessionStatusTracker{}
	p.onChildStarted = func() {
		mu.Lock()
		defer mu.Unlock()
		calls.Add(1)
		// Same wiring as production: heal orphaned busy flags.
		generations.onOpencodeGenerationStart()
	}

	p.start()
	defer p.stop()

	requireFakeReachable(t, port, 2*time.Second)
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		2*time.Second, 10*time.Millisecond, "hook must fire for first boot")

	p.restart()
	requireFakeReachable(t, port, 2*time.Second)
	require.Eventually(t, func() bool { return calls.Load() == 2 },
		2*time.Second, 10*time.Millisecond, "hook must fire again for the restart generation")
}

func TestManagedProcess_OnChildStarted_HealsOrphanedBusy(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)

	tr := newSessionStatusTracker()
	// Simulate an orphan: busy flag set, then the owning process is gone.
	tr.set("ses-orphan", "busy")
	tr.set("ses-live", "idle")
	p.onChildStarted = tr.onOpencodeGenerationStart

	p.start()
	defer p.stop()

	requireFakeReachable(t, port, 2*time.Second)
	require.Eventually(t, func() bool { return tr.get("ses-orphan") == "idle" },
		2*time.Second, 10*time.Millisecond,
		"orphaned busy flag must clear at the generation boundary")
	require.Equal(t, "idle", tr.get("ses-live"))
}

func TestManagedProcess_OnChildStarted_NilIsSafe(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0) // onChildStarted nil
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)
}

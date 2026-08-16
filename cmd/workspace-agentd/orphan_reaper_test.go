// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSubreaper makes the test process a child subreaper so orphaned
// grandchildren reparent to IT (not the container init), letting the
// reaper's /proc scan observe them as zombie children of this process.
// Production agentd is PID 1 and needs no prctl; this mirrors it for
// the test binary.
func withSubreaper(t *testing.T) {
	t.Helper()
	require.NoError(t, becomeSubreaper(), "test process must be able to become a subreaper")
}

// spawnOrphanedGrandchild starts a grandchild that outlives its parent:
// the outer sh (direct child of the caller, reaped by Run) exits
// immediately while the inner sh is still in its sleep. The inner
// process is reparented to the nearest subreaper (the caller, after
// withSubreaper) and exits ~100ms later as an unwaited zombie.
func spawnOrphanedGrandchild(t *testing.T) {
	t.Helper()
	err := exec.Command("sh", "-c", `sh -c 'sleep 0.1; exit 9' & exit 0`).Run()
	require.NoError(t, err)
}

// awaitZombie polls the /proc scan until the given pid appears as a
// zombie child of this process, or fails after timeout.
func awaitZombie(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, z := range scanZombieChildren() {
			if z == pid {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never appeared as a zombie child within %s", pid, timeout)
}

// firstPidInOutput extracts the last positive integer the command
// printed (used to capture a background grandchild's pid).
func firstPidInOutput(t *testing.T, out []byte) int {
	t.Helper()
	pid := 0
	for _, f := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(f); err == nil && p > 0 {
			pid = p
		}
	}
	require.NotZero(t, pid, "must capture the grandchild pid")
	return pid
}

// TestOrphanReaper_ZombiePersistsWithoutReaper pins the kernel behavior
// the fix targets: an orphaned grandchild that exits unwaited stays a
// zombie child of the subreaper indefinitely when nothing reaps it.
// This is the #904 bug, demonstrated in-process.
func TestOrphanReaper_ZombiePersistsWithoutReaper(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = 100 * time.Millisecond

	// Find the orphan's pid before it exits: spawn a variant that prints
	// the grandchild's pid.
	out, err := exec.Command("sh", "-c", `sh -c 'sleep 0.4' & echo $!; exit 0`).Output()
	require.NoError(t, err)
	orphanPid := firstPidInOutput(t, out)

	awaitZombie(t, orphanPid, 3*time.Second)

	// Well past any grace the reaper would use: the zombie must STILL be
	// there — nothing is reaping it.
	time.Sleep(500 * time.Millisecond)
	found := false
	for _, z := range scanZombieChildren() {
		if z == orphanPid {
			found = true
		}
	}
	assert.True(t, found, "unwaited orphan must stay zombie without a reaper (bug #904 baseline)")
}

// TestOrphanReaper_ReapsAdoptedOrphan is the core fix: with the reaper
// running, an orphaned grandchild that exits unwaited is reaped within
// grace + one scan, counted in the metric, and leaves no zombie.
func TestOrphanReaper_ReapsAdoptedOrphan(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = 200 * time.Millisecond
	reaper.interval = 50 * time.Millisecond
	reaper.workspaceID = "ws-reap-test"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); reaper.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	spawnOrphanedGrandchild(t)

	// The zombie must appear (adoption worked) and then disappear
	// (reaped) within grace + generous scan slack.
	deadline := time.Now().Add(5 * time.Second)
	sawZombie := false
	for time.Now().Before(deadline) {
		if len(scanZombieChildren()) > 0 {
			sawZombie = true
		}
		if sawZombie && len(scanZombieChildren()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, sawZombie, "orphan should be observed as a zombie before reaping")
	assert.Empty(t, scanZombieChildren(), "reaper must clear the zombie within grace+scan")

	assert.GreaterOrEqual(t, testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues("ws-reap-test")), 1.0,
		"each reaped orphan must be counted in workspace_orphans_reaped_total")
}

// TestOrphanReaper_TrackedZombieNeverReaped proves the reaper never
// steals a child whose exit status an os/exec waiter still owns — even
// when that child stays zombie far past the grace period (the starved
// supervisor window). The late Wait() must still see the real exit code.
func TestOrphanReaper_TrackedZombieNeverReaped(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = 150 * time.Millisecond
	reaper.interval = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); reaper.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	cmd := exec.Command("sh", "-c", "sleep 0.2; exit 3")
	require.NoError(t, cmd.Start())
	reaper.track(cmd.Process.Pid)

	awaitZombie(t, cmd.Process.Pid, 3*time.Second)
	time.Sleep(600 * time.Millisecond) // >> grace while owned

	stillZombie := false
	for _, z := range scanZombieChildren() {
		if z == cmd.Process.Pid {
			stillZombie = true
		}
	}
	assert.True(t, stillZombie, "tracked (owned) zombie must never be reaped, regardless of age")

	waitErr := cmd.Wait()
	reaper.untrack(cmd.Process.Pid)
	require.Error(t, waitErr, "child exits 3 on purpose")
	ee, ok := waitErr.(*exec.ExitError)
	require.True(t, ok)
	assert.Equal(t, 3, ee.ExitCode(), "owner must observe the true exit status")
}

// TestOrphanReaper_ConcurrentUntrackedWaitsNotStolen guards the
// transient-exec race: plain os/exec Start+Wait (no registration, the
// shape of every Output() call) must keep working while the reaper
// runs — the grace period plus kernel-level handoff to the blocking
// waiter mean the reaper never observes these pids as aged zombies.
func TestOrphanReaper_ConcurrentUntrackedWaitsNotStolen(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = 100 * time.Millisecond
	reaper.interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); reaper.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	const n = 40
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			cmd := exec.Command("sh", "-c", "exit 0")
			if err := cmd.Start(); err != nil {
				errs <- err
				return
			}
			errs <- cmd.Wait()
		}(i)
	}
	for i := 0; i < n; i++ {
		assert.NoError(t, <-errs, "untracked actively-waited children must never be stolen")
	}
}

// TestTrackedOutput verifies the tracked exec helper used by callers
// that need Output() semantics with reaper registration — including
// stderr capture into ExitError.Stderr (parity with cmd.Output()).
func TestTrackedOutput(t *testing.T) {
	out, err := trackedOutput(exec.Command("sh", "-c", "printf hello"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(out))

	_, err = trackedOutput(exec.Command("sh", "-c", "echo boom >&2; exit 4"))
	require.Error(t, err)
	ee, ok := err.(*exec.ExitError)
	require.True(t, ok)
	assert.Equal(t, 4, ee.ExitCode())
	assert.Equal(t, "boom\n", string(ee.Stderr), "stderr must be captured like cmd.Output()")
}

// TestOrphanReaper_PendingPrunedForVanishedZombies pins the pending-map
// prune: a zombie sighted once and then reaped by its own waiter must
// not leave a pending entry (leak + pid-reuse grace bypass).
func TestOrphanReaper_PendingPrunedForVanishedZombies(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = time.Hour // never reap in this test via aging

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	awaitZombie(t, pid, 3*time.Second)

	// First pass sees it: pending entry added.
	reaper.pass()
	reaper.mu.Lock()
	_, inPending := reaper.pending[pid]
	reaper.mu.Unlock()
	assert.True(t, inPending, "first pass records the unowned zombie")

	// Its waiter reaps it; second pass must drop the stale entry.
	require.NoError(t, cmd.Wait())
	reaper.pass()
	reaper.mu.Lock()
	_, inPending = reaper.pending[pid]
	reaper.mu.Unlock()
	assert.False(t, inPending, "pending entry must be pruned once the zombie is gone")
}

// TestOrphanReaper_Wait4Echo covers the syscall plumbing on this
// kernel: a reaped child's exit status is readable via Wait4.
func TestOrphanReaper_Wait4Echo(t *testing.T) {
	withSubreaper(t)
	reaper := newOrphanReaper()
	reaper.grace = 50 * time.Millisecond

	out, err := exec.Command("sh", "-c", `sh -c 'sleep 0.05; exit 7' & echo $!; exit 0`).Output()
	require.NoError(t, err)
	pid := firstPidInOutput(t, out)

	awaitZombie(t, pid, 3*time.Second)
	var ws syscall.WaitStatus
	wpid, werr := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	require.NoError(t, werr)
	assert.Equal(t, pid, wpid)
	assert.True(t, ws.Exited())
	assert.Equal(t, 7, ws.ExitStatus())
}

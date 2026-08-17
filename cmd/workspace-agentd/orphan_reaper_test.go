// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
		zs, _ := scanZombieChildren()
		for _, z := range zs {
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
	zs, _ := scanZombieChildren()
	found := false
	for _, z := range zs {
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

	// Metric-only assertion (review round 3): a counted reap proves a
	// zombie existed and was reaped — immune to a CI stall spanning the
	// exit→reap window, which could previously hide the zombie from
	// every poll while the reaper worked correctly. The zombie-becomes-
	// zombie baseline is pinned separately by ZombiePersistsWithoutReaper.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues("ws-reap-test")) >= 1.0
	}, 10*time.Second, 50*time.Millisecond,
		"reaper must reap the adopted orphan within grace+scan and count it in workspace_orphans_reaped_total")
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
	zs, _ := scanZombieChildren()
	for _, z := range zs {
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

// TestOrphanReaper_SupervisorRegistration_Wiring pins the production
// wiring in managedProcess.supervise: while a real supervised child is
// alive the reaper must own its pid, and after stop() the pid must be
// released. Deleting the track/untrack calls in managed_process.go
// fails this test — the mutation the review verified ships undetected.
func TestOrphanReaper_SupervisorRegistration_Wiring(t *testing.T) {
	withSubreaper(t)
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	defer p.stop()

	requireFakeReachable(t, port, 5*time.Second)
	p.mu.Lock()
	pid := p.cmd.Process.Pid
	p.mu.Unlock()
	require.NotZero(t, pid)

	assert.True(t, pkgOrphanReaper.owns(pid),
		"supervisor must register its live child with the reaper")

	p.stop()
	assert.False(t, pkgOrphanReaper.owns(pid),
		"registration must be released after the child exits and is waited")
}

// TestOrphanReaper_StartupWiring_ReapsAndDoesNotSteal pins the
// production wiring in startBackgroundLoops: the reaper loop that
// serves a real agentd startup (a) reaps an adopted orphan and counts
// the metric, and (b) never steals a supervised child's exit — the
// supervisor still observes the true status while the loop runs.
// Deleting the reaper goroutine in server.go fails (a); deleting the
// supervisor registration fails (b).
func TestOrphanReaper_StartupWiring_ReapsAndDoesNotSteal(t *testing.T) {
	withSubreaper(t)
	withTestLogger(t)

	// Real startBackgroundLoops wiring, minimal deps: the loops we do
	// not care about (SSE subscribe, memory pressure, metrics, fillGaps,
	// health cache) tolerate a nil-ish client because this test never
	// exercises them past their idle paths; the reaper loop is the
	// target. The supervised child comes from the standard fake factory.
	deps := serverDeps{
		client:          &OpenCodeClient{password: "t", client: httpTimeoutClient(t)},
		cache:           &providerCache{},
		sseTracker:      newSessionStatusTracker(),
		pressureMonitor: newMemoryPressureMonitor(),
		healthCache:     newHealthzCache(),
		gr:              newGateRecorder(time.Now(), agentdGateDurationSeconds, log),
		startedAt:       time.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var bgWg sync.WaitGroup
	startBackgroundLoops(ctx, &bgWg, deps)
	defer func() {
		cancel()
		done := make(chan struct{})
		go func() { bgWg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	// RecordOrphanReap normalizes an empty workspace ID to "unknown";
	// mirror that here (pkgOrphanReaper's label is "" in tests).
	reapLabel := pkgOrphanReaper.workspaceID
	if reapLabel == "" {
		reapLabel = "unknown"
	}
	before := testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues(reapLabel))

	// (a) orphaned grandchild must be reaped by the loop.
	spawnOrphanedGrandchild(t)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues(reapLabel)) > before
	}, 30*time.Second, 200*time.Millisecond, "startup-wired reaper must reap the adopted orphan and count the metric (grace 5s + ticker 5s + slack)")

	// (b) ownership discipline through the real startup wiring: the
	// supervised child is owned while alive and released after exit.
	// (The status-preservation property itself — an owned child's exit
	// status always reaching its waiter — is pinned by
	// TrackedZombieNeverReaped; here we assert the registry
	// transitions, i.e. that this wiring stays registered.)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	requireFakeReachable(t, port, 5*time.Second)
	p.mu.Lock()
	pid := p.cmd.Process.Pid
	p.mu.Unlock()
	require.True(t, pkgOrphanReaper.owns(pid), "supervised child must be owned while the loop runs")
	p.stop()
	assert.False(t, pkgOrphanReaper.owns(pid), "ownership released after exit")
}

func httpTimeoutClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Timeout: 2 * time.Second}
}

// TestReadProcStat table-drives the /proc/<pid>/stat parser against
// hostile comm fields (spaces, parens) — a naive strings.Fields
// refactor must fail here, not in prod.
func TestReadProcStat(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		state byte
		ppid  int
		ok    bool
	}{
		{"plain zombie", "41 (sh) Z 1 0 0", 'Z', 1, true},
		{"spaces in comm", "42 (a b c) S 7 0 0", 'S', 7, true},
		{"parens in comm", "43 (a) b (c)) Z 7 0 0", 'Z', 7, true},
		{"no close paren", "44 (sh Z 1 0 0", 0, 0, false},
		{"empty after comm", "45 (sh)", 0, 0, false},
		{"missing ppid field", "46 (sh) Z", 0, 0, false},
		{"non-numeric ppid", "47 (sh) Z x 0 0", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, ppid, ok := parseProcStat(tc.raw)
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.state, state)
				assert.Equal(t, tc.ppid, ppid)
			}
		})
	}
}

// TestOrphanReaper_ScanErrorPreservesPendingClocks pins the round-3
// robustness fix: a failed /proc walk aborts the pass before touching
// state — pending grace clocks survive (a transient scan failure must
// not delay in-flight reaps by resetting their aging) and nothing is
// reaped.
func TestOrphanReaper_ScanErrorPreservesPendingClocks(t *testing.T) {
	reaper := newOrphanReaper()
	reaper.grace = 10 * time.Millisecond

	calls := 0
	reaper.scan = func() ([]int, error) {
		calls++
		if calls == 1 {
			return []int{4242}, nil // healthy scan: pending clock starts
		}
		return nil, errors.New("transient /proc failure")
	}

	// Never actually reap anything: Wait4 on 4242 would ECHILD anyway;
	// the point is that the error pass must not reach Wait4 or prune.
	reaper.pass() // first pass records pending[4242]
	reaper.mu.Lock()
	firstSeen := reaper.pending[4242]
	reaper.mu.Unlock()
	require.False(t, firstSeen.IsZero(), "healthy scan must start the grace clock")

	time.Sleep(30 * time.Millisecond) // past grace
	reaper.pass()                     // scan error pass

	reaper.mu.Lock()
	still, kept := reaper.pending[4242]
	reaper.mu.Unlock()
	assert.True(t, kept, "scan error must not prune pending entries")
	assert.Equal(t, firstSeen, still, "scan error must not reset the grace clock")

	// Recovery: after the error pass, a healthy scan still reaps.
	withSubreaper(t)
	real := newOrphanReaper()
	real.grace = 50 * time.Millisecond
	reapLabel := real.workspaceID
	if reapLabel == "" {
		reapLabel = "unknown"
	}
	before := testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues(reapLabel))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); real.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	spawnOrphanedGrandchild(t)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(pkgOpsMetrics.orphansReaped.WithLabelValues(reapLabel)) > before
	}, 10*time.Second, 50*time.Millisecond, "recovery scan after error must still reap and count the metric delta")
}

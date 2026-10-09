// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_reaper_test.go — #1632 fix #4: the supervise-opencode
// supervisor (PID 1 of the workspace container in sidecar topology) must
// RUN the orphan reaper loop, not only becomeSubreaper(). The 2026-10-08
// incident pod carried [python] <defunct> (after 3:30 of CPU) and five
// [esbuild] <defunct> entries: orphans reparented to PID 1 and nothing
// reaped them, because pkgOrphanReaper.run() was wired only in the
// mux-serving agentd modes (server.go), never in the supervisor.
//
// Red-first shape: launch the REAL subcommand with a stub opencode that
// double-forks a short-lived sleeper. The sleeper orphans → reparents to
// the supervisor (subreaper) → exits → is a zombie child of the
// supervisor. Without the reaper loop the zombie is permanent (the Go
// runtime only reaps children its own os/exec waiters block on); with it,
// the zombie is reaped within grace (5s) + scan interval (5s).

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startSupervisorSubprocessStub launches the real supervise-opencode
// subcommand with a CALLER-SUPPLIED stub opencode script (the shared
// helper hardcodes `exec sleep 3600`). Same env discipline as
// startSupervisorSubprocessEnv: PATH limited to the stub dir, ephemeral
// control socket, WaitDelay so stray child I/O cannot wedge go test.
func startSupervisorSubprocessStub(t *testing.T, stubScript string) *supervisorProc {
	t.Helper()
	withTestLogger(t)

	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "opencode")
	require.NoError(t, os.WriteFile(stub, []byte(stubScript), 0o755))

	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	//nolint:gosec // os.Args[0] is the trusted test binary path
	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorHelperProcess", "-test.v")
	cmd.Env = []string{
		"GO_TEST_SUPERVISOR=1",
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=" + addr,
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"WORKSPACE_ID=supervisor-reaper-test",
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())

	sp := &supervisorProc{cmd: cmd, addr: addr, stubDir: stubDir}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Cleanup(sp.stop)
			return sp
		}
		if sp.exited() {
			t.Fatalf("supervisor subprocess exited before serving (see output above)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("supervisor socket %s never accepted within 10s", addr)
	return nil
}

// procStateOf reads (state, ppid) from /proc/<pid>/stat, tolerating a
// vanished entry ((0,0,false)).
func procStateOf(pid int) (state byte, ppid int, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(raw)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 > len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 2 {
		return 0, 0, false
	}
	p, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], p, true
}

// supervisorChildren returns the pids whose /proc stat names the
// supervisor as parent (direct children AND reparented orphans).
func supervisorChildren(superPid int) map[int]byte {
	out := map[int]byte{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == superPid {
			continue
		}
		state, ppid, ok := procStateOf(pid)
		if ok && ppid == superPid {
			out[pid] = state
		}
	}
	return out
}

// TestSupervisorSubprocess_ReapsOrphanedToolChildren: the incident's
// exact shape — a tool child orphaned below opencode reparents to the
// supervisor, exits, and must be reaped (vanish from /proc), not linger
// as <defunct>. Bound: orphan grace (5s) + scan interval (5s) + slack.
func TestSupervisorSubprocess_ReapsOrphanedToolChildren(t *testing.T) {
	if os.Getenv("GO_TEST_SUPERVISOR") == "1" {
		// Guard: never run the outer test in the helper re-exec.
		t.Skip("helper process")
	}
	// Stub opencode: fork a sleeper that outlives its subshell parent
	// (orphaning it → reparents to the supervisor's subreaper), then
	// exec long-lived sleep as the supervised child.
	sp := startSupervisorSubprocessStub(t, "#!/bin/sh\n(sleep 2 &)\nexec sleep 3600\n")
	cc := newControlClient(sp.addr)
	superPid := sp.cmd.Process.Pid

	// Wait for the supervised child, then for the orphan to appear as a
	// supervisor child that is NOT the supervised pid.
	var childPID int
	require.Eventually(t, func() bool {
		childPID = sp.childPIDOf(t, cc)
		return childPID > 0
	}, 10*time.Second, 100*time.Millisecond, "supervised child never started")

	orphanPID := 0
	require.Eventually(t, func() bool {
		for pid := range supervisorChildren(superPid) {
			if pid != childPID {
				orphanPID = pid
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond,
		"orphaned sleeper never reparented to the supervisor (subreaper broken?)")
	t.Logf("orphan pid %d (supervised child %d, supervisor %d)", orphanPID, childPID, superPid)

	// Wait for the orphan to exit and turn zombie while parented by the
	// supervisor (2s sleep + slack). This is the incident's <defunct>.
	require.Eventually(t, func() bool {
		state, ppid, ok := procStateOf(orphanPID)
		return ok && state == 'Z' && ppid == superPid
	}, 10*time.Second, 100*time.Millisecond, "orphan never became a supervisor zombie")

	// THE PIN: the zombie must be reaped — /proc entry gone — within
	// grace + scan interval + slack. Without the reaper loop wired in
	// runSuperviseOpencodeCommand this times out with the zombie still
	// present (the 36-hour <defunct> accumulation shape).
	require.Eventually(t, func() bool {
		_, _, ok := procStateOf(orphanPID)
		return !ok
	}, 20*time.Second, 200*time.Millisecond,
		"orphan zombie was never reaped — supervisor PID 1 is not running the reaper loop (#1632)")

	// The reaper must not have stolen the SUPERVISED child: it is
	// tracked (os/exec waiter) and still reported live by status.
	nowPID := sp.childPIDOf(t, cc)
	require.Greater(t, nowPID, 0, "supervised child must survive orphan reaping")
}

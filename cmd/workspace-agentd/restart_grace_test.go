// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// restart_grace_test.go — US-2 (design 0051): grace_seconds wiring.
//
// Appendix A.2's restart params carry grace_seconds; US-1 shipped the
// socket contract but the supervisor's restart() hardcoded the 5s
// SIGTERM→SIGKILL window. These tests pin the parameterization:
//
//   - restartWithGrace honors a SHORTER window than the 5s default
//     (proving the timer is parameterized, not constant);
//   - managedProcAdapter.Restart forwards the socket's grace_seconds
//     to restartWithGrace.

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestManagedProcess_RestartWithGrace_ShortKillWindow runs a fake child
// that IGNORES SIGTERM (only SIGKILL terminates it) and asks for a 700ms
// grace. With the pre-US-2 hardcoded 5s kill timer this restart takes
// ~5s; parameterized it must complete in well under that. The 3.5s
// assert boundary sits between the two regimes with CI-slack margin on
// both sides.
func TestManagedProcess_RestartWithGrace_ShortKillWindow(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := &managedProcess{}
	p.cmdFactory = func() *exec.Cmd {
		//nolint:gosec // os.Args[0] is the trusted test binary path
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = []string{
			"GO_TEST_FAKE_OPENCODE=1",
			"FAKE_PORT=" + strconv.Itoa(port),
			"IGNORE_SIGTERM=1",
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	p.healthCheckURL = "" // skip post-restart probe; not under test
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	begin := time.Now()
	p.restartWithGrace(700 * time.Millisecond)
	elapsed := time.Since(begin)

	// The new child must be up (restart contract unchanged)…
	requireFakeReachable(t, port, 2*time.Second)
	// …and the restart must NOT have waited out a 5s kill window.
	require.Less(t, elapsed, 3*time.Second,
		"restartWithGrace(700ms) completed in %v — the kill timer is not honoring the grace parameter", elapsed)
	require.GreaterOrEqual(t, elapsed, 600*time.Millisecond,
		"the SIGKILL fallback fired suspiciously fast; the child ignores SIGTERM, so nothing but the timer can end it")
}

// TestManagedProcess_Restart_DefaultGraceUnchanged pins that the plain
// restart() keeps the historical 5s window — single-container callers
// (watchdog, session-aware reload, relay injector) see no behavior
// change from the parameterization.
func TestManagedProcess_Restart_DefaultGraceUnchanged(t *testing.T) {
	require.Equal(t, 5*time.Second, defaultRestartGrace,
		"restart() must keep the historical 5s SIGTERM→SIGKILL window")
}

// TestManagedProcAdapter_RestartForwardsGrace proves the adapter passes
// the socket's grace_seconds through to the supervisor's kill timer: a
// 1s grace against a SIGTERM-ignoring child finishes near 1s, not 5s.
func TestManagedProcAdapter_RestartForwardsGrace(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := &managedProcess{}
	p.cmdFactory = func() *exec.Cmd {
		//nolint:gosec // os.Args[0] is the trusted test binary path
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = []string{
			"GO_TEST_FAKE_OPENCODE=1",
			"FAKE_PORT=" + strconv.Itoa(port),
			"IGNORE_SIGTERM=1",
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	p.healthCheckURL = ""
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	adapter := &managedProcAdapter{p: p}
	begin := time.Now()
	restarted, inProgress := adapter.Restart("manual", 1)
	elapsed := time.Since(begin)

	require.True(t, restarted)
	require.False(t, inProgress)
	requireFakeReachable(t, port, 2*time.Second)
	require.Less(t, elapsed, 3*time.Second,
		"adapter.Restart grace_seconds=1 took %v — grace is not reaching the kill timer", elapsed)
}

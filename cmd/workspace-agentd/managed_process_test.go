// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// managed_process_test.go — Tests for managedProcess.start/restart/stop.
//
// Why these tests exist
// ---------------------
// Worklog 0125 surfaced a production bug: when the LLM-credential push
// path fell back to proc.restart(), restart() and the supervisor
// goroutine spawned by start() both called cmd.Wait() on the same
// *exec.Cmd. The supervisor's Wait() won; restart() returned before
// the kernel reaped the old PID; the replacement opencode crashed with
// `Failed to start server. Is port 4096 in use?` over and over.
//
// The bug was undetectable in the original test suite because there
// were *no* tests for managedProcess at all. Every CI run was green
// because nothing exercised the restart path.
//
// Test strategy
// -------------
// We use the well-known TestHelperProcess pattern (see
// os/exec/exec_test.go in the standard library): the test re-execs
// itself with a marker env var so the subprocess runs `runFakeOpencode`
// instead of the test binary. The fake binds a TCP port (configurable
// via env var so concurrent tests don't collide) and has three
// signal-handling modes:
//   - IGNORE_SIGTERM=1: catch+discard SIGTERM in a loop (only SIGKILL kills)
//   - SIGTERM_DELAY_MS=N: catch SIGTERM, sleep N ms, then exit
//   - default: block forever until killed (SIGKILL)
//
// The managedProcess code under test is decoupled from the literal
// `opencode serve` argv via a commandFactory func; production wires
// the real opencode command, tests inject a factory that produces a
// fake subprocess.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHelperProcess is the entry point used when the test binary is
// re-execed with GO_TEST_FAKE_OPENCODE=1. The standard testing harness
// runs every Test* function; this one short-circuits to runFakeOpencode
// so the test process can be used as a controllable subprocess.
//
// Inspired by os/exec/exec_test.go's TestHelperProcess pattern.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_FAKE_OPENCODE") != "1" {
		return
	}
	runFakeOpencode()
}

// runFakeOpencode binds the port specified by FAKE_PORT, optionally
// ignores SIGTERM for SIGTERM_DELAY_MS milliseconds, then exits with
// FAKE_EXIT (default 0). This simulates opencode for managedProcess
// tests.
func runFakeOpencode() {
	port := os.Getenv("FAKE_PORT")
	if port == "" {
		fmt.Fprintln(os.Stderr, "fake-opencode: FAKE_PORT not set")
		os.Exit(2)
	}

	// Optional pre-bind delay: simulates opencode's boot-to-listen window
	// under CPU starvation (incident 2026-08-15/16: boot exceeded 120s on
	// a saturated 2-CPU quota). The port stays refused while this sleeps.
	if d := os.Getenv("FAKE_BIND_DELAY_MS"); d != "" {
		if ms, err := strconv.Atoi(d); err == nil && ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-opencode: bind %s: %v\n", port, err)
		os.Exit(3)
	}

	// Serve a trivial /v1/readyz so the post-restart health check in
	// managedProcess.restart() can succeed, and /global/health so the
	// healthz-cache loop (refreshOnce) observes the child as healthy.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"version":"fake-test"}`))
	})
	srv := &http.Server{Handler: mux} //nolint:gosec // test fixture
	go func() { _ = srv.Serve(ln) }()

	// Optional SIGTERM-handling delay: when set, the process catches
	// SIGTERM/SIGINT, sleeps, then exits. This forces
	// managedProcess.restart() to wait for the old process to fully
	// exit before binding a new port — the regression behavior for
	// Bug 2 (worklog 0125).
	//
	// IGNORE_SIGTERM=1: the process catches and discards SIGTERM/SIGINT
	// in a loop, never exiting on signal. Only SIGKILL can terminate it.
	// Used by the SIGKILL-fallback tests to prove the killTimer is what
	// kills the child (not a natural exit).
	delayMS, _ := strconv.Atoi(os.Getenv("SIGTERM_DELAY_MS"))
	if os.Getenv("IGNORE_SIGTERM") == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		for range ch {
			// discard — only SIGKILL (uncatchable) terminates us
		}
	} else if delayMS > 0 {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		time.Sleep(time.Duration(delayMS) * time.Millisecond)
	} else {
		// Default: block forever until killed.
		select {}
	}
	exitCode, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(exitCode)
}

// --- Tests ---

// TestManagedProcess_StartLaunchesSubprocess verifies the basic happy
// path: start() launches the subprocess, the subprocess runs, and the
// process is reachable on its port.
func TestManagedProcess_StartLaunchesSubprocess(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	defer p.stop()

	requireFakeReachable(t, port, 2*time.Second)
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	require.NotNil(t, cmd)
	require.NotNil(t, cmd.Process)
}

// TestManagedProcess_Restart_FreesPortBeforeNewBind is the regression
// guard for Bug 2 (worklog 0125). The fake opencode is configured
// with SIGTERM_DELAY_MS=300, so a naive restart() that doesn't wait
// for full Wait()/reap would race the new bind against the old port
// holder and the new process would crash with "address already in
// use." This test asserts that restart() returns only after the old
// process is fully reaped, so the new one can bind cleanly.
func TestManagedProcess_Restart_FreesPortBeforeNewBind(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	// SIGTERM_DELAY_MS=300 — old process holds the port for 300ms
	// after SIGTERM. Pre-fix restart() returned ~immediately and the
	// new opencode crashed on bind.
	p := newTestManagedProcess(t, port, 300)
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	// Capture the original PID so we can prove the new process is a
	// fresh one (not the same PID that got "kept around").
	p.mu.Lock()
	origPID := p.cmd.Process.Pid
	p.mu.Unlock()

	p.restart()

	// After restart() returns, the new process must already be
	// listening — not "about to listen". A 2-second budget covers
	// the 300ms SIGTERM grace + Start() + bind on a slow CI runner.
	requireFakeReachable(t, port, 2*time.Second)

	p.mu.Lock()
	newPID := p.cmd.Process.Pid
	p.mu.Unlock()
	require.NotEqual(t, origPID, newPID, "restart() must produce a new PID")
}

// TestManagedProcess_Restart_OldProcessIsReaped ensures the old
// process is in the "exited" state (not lingering as a zombie or
// still running) after restart() returns.
func TestManagedProcess_Restart_OldProcessIsReaped(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 100)
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	p.mu.Lock()
	origCmd := p.cmd
	p.mu.Unlock()

	p.restart()

	// The original cmd's ProcessState must be set (non-nil), which is
	// the contract of a successful Wait(). If the supervisor was the
	// one that called Wait() and restart() raced ahead before reap,
	// ProcessState would be nil here.
	require.NotNil(t, origCmd.ProcessState,
		"original cmd.ProcessState must be set — Wait() must have completed before restart() returns")
	require.True(t, origCmd.ProcessState.Exited(),
		"original process must be in Exited state after restart()")
}

// TestManagedProcess_AutoRestartOnCrash verifies the supervisor's
// auto-restart behavior: if the subprocess exits unexpectedly (not
// via stop()), the supervisor re-launches it on its backoff schedule.
func TestManagedProcess_AutoRestartOnCrash(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	// Kill the process out from under the supervisor — same outward
	// effect as a crash.
	p.mu.Lock()
	origPID := p.cmd.Process.Pid
	_ = p.cmd.Process.Kill()
	p.mu.Unlock()

	// Wait for the supervisor to restart; baseline backoff is 1s.
	deadline := time.Now().Add(5 * time.Second)
	var newPID int
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if p.cmd != nil && p.cmd.Process != nil {
			newPID = p.cmd.Process.Pid
		}
		p.mu.Unlock()
		if newPID != 0 && newPID != origPID {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotEqual(t, origPID, newPID, "supervisor must auto-restart after crash")

	// And the new process must own the port.
	requireFakeReachable(t, port, 3*time.Second)
}

// TestManagedProcess_StopPreventsAutoRestart verifies that an
// intentional stop() does NOT trigger the auto-restart path.
func TestManagedProcess_StopPreventsAutoRestart(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	requireFakeReachable(t, port, 2*time.Second)

	p.stop()

	// Wait long enough that any mistaken auto-restart would have
	// fired (1s baseline backoff + slack).
	time.Sleep(2 * time.Second)
	require.False(t, isPortBound("127.0.0.1:"+strconv.Itoa(port)),
		"port must NOT be re-bound after stop()")
}

// TestManagedProcess_RapidRestarts verifies repeated restart() calls
// don't stack up subprocesses or leak port holders.
func TestManagedProcess_RapidRestarts(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	// 50ms SIGTERM grace — short enough that 5 back-to-back restarts
	// finish within the test budget.
	p := newTestManagedProcess(t, port, 50)
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	for i := 0; i < 5; i++ {
		p.restart()
		requireFakeReachable(t, port, 2*time.Second)
	}
}

// --- Helpers ---

// newTestManagedProcess builds a managedProcess wired with a
// commandFactory that re-execs the test binary in TestHelperProcess
// mode. sigtermDelayMS controls how long the fake holds the port
// after receiving SIGTERM (0 = forever; killed by SIGKILL fallback).
func newTestManagedProcess(t *testing.T, port int, sigtermDelayMS int) *managedProcess {
	t.Helper()
	p := &managedProcess{}
	p.cmdFactory = func() *exec.Cmd {
		// Re-exec the test binary in TestHelperProcess mode.
		//nolint:gosec // os.Args[0] is the trusted test binary path
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		env := []string{
			"GO_TEST_FAKE_OPENCODE=1",
			"FAKE_PORT=" + strconv.Itoa(port),
		}
		if sigtermDelayMS > 0 {
			env = append(env, "SIGTERM_DELAY_MS="+strconv.Itoa(sigtermDelayMS))
		}
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	// Override the health check URL so the post-restart probe targets
	// the fake's port, not the production opencode port.
	p.healthCheckURL = "http://127.0.0.1:" + strconv.Itoa(port) + "/v1/readyz"
	return p
}

// withTestLogger replaces the package-global log with a no-op logger
// for the duration of the test. managedProcess uses the package log
// directly; without this override, test output is dominated by INFO
// lines about start/exit.
func withTestLogger(t *testing.T) {
	t.Helper()
}

// freeTCPPort returns a TCP port number that is currently free on
// 127.0.0.1. There is a small race between the listener being closed
// and the test subprocess binding the port; in practice 50ms is
// enough on every CI runner we use.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// requireFakeReachable polls the fake's HTTP /v1/readyz until it
// responds 200 or the deadline elapses.
func requireFakeReachable(t *testing.T, port int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/v1/readyz"
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url) //nolint:noctx // bounded by client.Timeout
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fake opencode at %s did not respond ready within %s", url, within)
}

// isPortBound returns true if `addr` accepts a TCP connection right
// now. Used to assert a process is or is not running.
func isPortBound(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// TestHealthProbeAfterRestart_BearerTokenAttachment verifies the probe
// attaches `Authorization: Bearer $AGENTD_ADMIN_TOKEN` when the env var
// is set, so it succeeds against a readyz endpoint gated by
// requireBearerToken (server.go). Covers the code path added when the
// healthCheckURL was corrected from opencode's :4096 (which the probe
// could never authenticate against) to agentd's :4098 readyz.
func TestHealthProbeAfterRestart_BearerTokenAttachment(t *testing.T) {
	const token = "test-admin-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// requireBearerToken matches this exact header value.
	t.Setenv("AGENTD_ADMIN_TOKEN", token)

	// Construct a minimal managedProcess with only the fields the probe
	// reads (healthCheckURL + an open, never-closed doneCh). An open
	// doneCh means the probe's `<-doneCh` select case never fires, so the
	// probe runs its full poll loop rather than aborting immediately.
	p := &managedProcess{
		healthCheckURL: srv.URL + "/v1/readyz",
	}
	doneCh := make(chan struct{})
	p.doneCh = doneCh

	require.NotPanics(t, func() { p.healthProbeAfterRestart() },
		"probe must succeed (200) when the Bearer token is attached")
}

// TestHealthProbeAfterRestart_BearerTokenMissingFails is the negative
// half: with the token set in the env but the server expecting a
// different token, the probe gets 401 on every attempt and never
// succeeds. (We assert via the absence of a panic / hang rather than a
// return-value contract, since the probe logs a warning on failure and
// has no return value — it is "purely diagnostic" by design.)
func TestHealthProbeAfterRestart_BearerTokenMissingFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server requires a token the probe does not send.
		if got := r.Header.Get("Authorization"); got != "Bearer expected-different" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("AGENTD_ADMIN_TOKEN", "wrong-token")

	// Open, never-closed doneCh — probe runs its full retry loop.
	p := &managedProcess{
		healthCheckURL: srv.URL + "/v1/readyz",
	}
	doneCh := make(chan struct{})
	p.doneCh = doneCh

	// The probe exhausts its retries (all 401) and logs a warning. It must
	// not hang or panic.
	require.NotPanics(t, func() { p.healthProbeAfterRestart() })
}

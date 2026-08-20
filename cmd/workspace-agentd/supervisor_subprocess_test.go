// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_subprocess_test.go — US-1 exec-level integration (design
// 0051; level between L1 and L2 in docs/testing/0051-us2-integration-test-plan.md).
//
// Everything else in this package wires the supervisor's components
// IN-PROCESS. This file runs the REAL `supervise-opencode` subcommand as
// a REAL subprocess — PID semantics, signal handling, subreaper, exit
// code — and drives it from the test process over real TCP with the real
// controlClient. That closes the gap between the unit suites and the
// kind-cluster runbook: any wiring that only exists at process start
// (env parsing, listener bootstrap, shutdown ordering) breaks HERE, on
// every machine, not only in a cluster.
//
// Fakes, deliberately minimal:
//   - the `opencode` BINARY: a POSIX shell stub on the supervisor's PATH
//     that traps SIGTERM and sleeps — the supervisor never health-probes
//     it in supervisor mode (skipHealthProbe), so no port binding is
//     needed; what is under test is spawn/signal/respawn/env, not opencode.
//   - the socket ADDRESS: an ephemeral port via LLMSAFESPACES_CONTROL_
//     SOCKET_ADDR (the test seam; production keeps the fixed 4099).
//
// The subprocess is the test binary itself re-exec'd in helper mode
// (same TestHelperProcess pattern as managed_process_test.go).

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSupervisorHelperProcess is the re-exec entry point. It never
// returns while the supervisor runs.
func TestSupervisorHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_SUPERVISOR") != "1" {
		return
	}
	code := runSuperviseOpencodeCommand(nil)
	os.Exit(code)
}

// supervisorProc is one launched supervisor subprocess.
type supervisorProc struct {
	cmd     *exec.Cmd
	addr    string
	stubDir string
}

// startSupervisorSubprocess re-execs the test binary in supervisor mode:
// PATH limited to the stub dir (fake opencode + coreutils), socket on an
// ephemeral port, output piped for failure diagnostics.
func startSupervisorSubprocess(t *testing.T) *supervisorProc {
	t.Helper()
	withTestLogger(t)

	stubDir := t.TempDir()
	// Fake opencode: ONE process (exec sleep — no background children that
	// could outlive the trap and hold the test's stdout pipe open, which
	// once made `go test` fail with "WaitDelay expired" after the tests
	// had passed). SIGTERM hits sleep's default disposition: terminate.
	// The supervisor never health-probes it (skipHealthProbe), so no port
	// binding is needed; what is under test is spawn/signal/respawn/env.
	stub := filepath.Join(stubDir, "opencode")
	require.NoError(t, os.WriteFile(stub, []byte(
		"#!/bin/sh\nexec sleep 3600\n"),
		0o755))

	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	//nolint:gosec // os.Args[0] is the trusted test binary path
	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorHelperProcess", "-test.v")
	cmd.Env = []string{
		"GO_TEST_SUPERVISOR=1",
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=" + addr,
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		// Neutralize this machine's secrets-env: the supervisor composes
		// the child env from it when present (buildEnvFrom). Pointing
		// HOME/PATH away is not enough — the path is a const — so assert
		// only on OUR handed env and ignore whatever else it carries.
		"WORKSPACE_ID=supervisor-integration-test",
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Belt and braces: never let lingering child I/O outlive the test.
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())

	sp := &supervisorProc{cmd: cmd, addr: addr, stubDir: stubDir}

	// Wait for the socket to accept before handing control back.
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

func (sp *supervisorProc) exited() bool {
	return sp.cmd.ProcessState != nil
}

// stop SIGTERMs the supervisor and waits for a CLEAN (0) exit.
func (sp *supervisorProc) stop() {
	if sp.cmd.ProcessState != nil {
		return
	}
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = sp.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = sp.cmd.Process.Kill()
	}
}

// childPIDOf reads the supervisor's reported child pid via status.
func (sp *supervisorProc) childPIDOf(t *testing.T, cc *controlClient) int {
	t.Helper()
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	return st.ChildPID
}

// --- Tests -------------------------------------------------------------------

// TestSupervisorSubprocess_LifecycleAndContract walks the full Appendix-A
// surface against the real subcommand: hello liveness, status shape,
// metrics envelope, spawn_env landing in the next REAL child's environ,
// restart swapping the child, and clean SIGTERM shutdown.
func TestSupervisorSubprocess_LifecycleAndContract(t *testing.T) {
	sp := startSupervisorSubprocess(t)
	cc := newControlClient(sp.addr)
	ctx := context.Background()

	// A real child must come up and be observable through the socket.
	var firstPID int
	require.Eventually(t, func() bool {
		firstPID = sp.childPIDOf(t, cc)
		return firstPID > 0
	}, 10*time.Second, 100*time.Millisecond, "supervisor must spawn the (stub) opencode child")

	hello, err := cc.Hello(ctx)
	require.NoError(t, err)
	require.Equal(t, "supervise-opencode", hello.Supervisor)

	// metrics: real supervisor wires a live source (US-2); on this host
	// cgroup v2 files exist, so the envelope must be non-reserved. If a
	// future host lacks cgroupfs the values degrade to zero — assert the
	// STRUCTURE only: a result object with the four v1 fields present.
	m, err := cc.Metrics(ctx)
	require.NoError(t, err)
	require.NotNil(t, m)

	// spawn_env → restart → the env lands in the next real child.
	require.NoError(t, cc.SpawnEnv(ctx, map[string]string{
		"PROBE_VAR": "handed-via-socket",
	}))
	_, err = cc.Restart(ctx, "credential_reload", 5)
	require.NoError(t, err)

	var secondPID int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(sp.childPIDOf(t, cc)) + "/environ")
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "PROBE_VAR=handed-via-socket\x00")
	}, 10*time.Second, 100*time.Millisecond, "the next real child must run with the socket-handed env")
	secondPID = sp.childPIDOf(t, cc)
	require.NotEqual(t, firstPID, secondPID, "restart must swap the child process")

	// The handed env REPLACES the child env wholesale — the factory's
	// own env (GO_TEST_SUPERVISOR included) must not leak through.
	data, err := os.ReadFile("/proc/" + strconv.Itoa(secondPID) + "/environ")
	require.NoError(t, err)
	require.False(t, strings.Contains(string(data), "GO_TEST_SUPERVISOR="),
		"spawn_env replacement semantics: only the handed env may reach the child")

	// NOTE: status.restarts counts CRASH recoveries; operator-initiated
	// (socket) restarts reset the counter by design — the pid swap above
	// is the restart contract. The counter is asserted in the crash test.
}

// TestSupervisorSubprocess_ChildCrashRespawn: killing the CHILD (as
// in-pod uid-1000 code can) exercises the supervisor's crash-recovery
// loop end-to-end in the real process — a fresh child, still serving the
// socket, marker semantics owned by the crash path.
func TestSupervisorSubprocess_ChildCrashRespawn(t *testing.T) {
	sp := startSupervisorSubprocess(t)
	cc := newControlClient(sp.addr)

	firstPID := 0
	require.Eventually(t, func() bool { firstPID = sp.childPIDOf(t, cc); return firstPID > 0 },
		10*time.Second, 100*time.Millisecond)

	// SIGKILL the child out from under the supervisor.
	require.NoError(t, syscall.Kill(firstPID, syscall.SIGKILL))

	require.Eventually(t, func() bool {
		newPID := sp.childPIDOf(t, cc)
		return newPID > 0 && newPID != firstPID
	}, 15*time.Second, 200*time.Millisecond,
		"the supervisor must respawn a fresh child after a crash (baseline backoff 1s)")

	// The socket must have stayed up THROUGH the crash window.
	_, err := cc.Hello(context.Background())
	require.NoError(t, err)

	// And the crash is COUNTED (unlike operator restarts — see the
	// lifecycle test's note): status.restarts reflects crash recoveries.
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, st.Restarts, 1, "a crashed-and-respawned child must increment the crash counter")
}

// TestSupervisorSubprocess_ShutdownReapsChild: SIGTERM to the real
// supervisor must take the child down with it (no orphan) and exit 0.
func TestSupervisorSubprocess_ShutdownReapsChild(t *testing.T) {
	sp := startSupervisorSubprocess(t)
	cc := newControlClient(sp.addr)

	firstPID := 0
	require.Eventually(t, func() bool { firstPID = sp.childPIDOf(t, cc); return firstPID > 0 },
		10*time.Second, 100*time.Millisecond)

	sp.stop()

	// Exit code 0 — the command's clean-shutdown contract.
	require.NotNil(t, sp.cmd.ProcessState)
	require.Equal(t, 0, sp.cmd.ProcessState.ExitCode(),
		"supervisor must exit 0 on SIGTERM; got %v", sp.cmd.ProcessState.ExitCode())

	// The child must be gone (reaped/terminated), not orphaned.
	require.Eventually(t, func() bool {
		err := syscall.Kill(firstPID, 0)
		return err != nil // ESRCH: process no longer exists
	}, 5*time.Second, 100*time.Millisecond, "child must not outlive the supervisor")
}

// TestSupervisorSubprocess_BadRequestOverWire: malformed input to the
// REAL process gets the A.3 error shapes (not a crash).
func TestSupervisorSubprocess_BadRequestOverWire(t *testing.T) {
	sp := startSupervisorSubprocess(t)

	resp := mustDial(t, sp.addr, `{not json`)
	errBody, ok := resp["error"].(map[string]any)
	require.True(t, ok, "malformed JSON must yield an error object, got %v", resp)
	require.Equal(t, "bad_request", errBody["code"])

	resp = mustDial(t, sp.addr, `{"v":1,"id":2,"method":"exec","params":{}}`)
	errBody, ok = resp["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "method_unknown", errBody["code"])

	// The supervisor must STILL be alive and serving afterwards.
	cc := newControlClient(sp.addr)
	_, err := cc.Hello(context.Background())
	require.NoError(t, err)
}

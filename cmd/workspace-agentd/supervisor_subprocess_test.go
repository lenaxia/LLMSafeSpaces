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
	return startSupervisorSubprocessEnv(t)
}

// startSupervisorSubprocessEnv additionally appends extra env vars to
// the supervisor's environment (the US-70.1 exec tests wire the
// spawn-env pull address and credential this way).
func startSupervisorSubprocessEnv(t *testing.T, extraEnv ...string) *supervisorProc {
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
	cmd.Env = append([]string{
		"GO_TEST_SUPERVISOR=1",
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=" + addr,
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		// Neutralize this machine's secrets-env: the supervisor composes
		// the child env from it when present (buildEnvFrom). Pointing
		// HOME/PATH away is not enough — the path is a const — so assert
		// only on OUR handed env and ignore whatever else it carries.
		"WORKSPACE_ID=supervisor-integration-test",
	}, extraEnv...)
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

// verifiedChildEnviron (#1543) captures the supervisor's CURRENT stub
// child's environ through the pid-identity guard: /proc/<pid>/stat's
// ppid must name the supervisor and a re-Status must still report the
// same pid — a recycled or moved-on pid is retried, never trusted.
func (sp *supervisorProc) verifiedChildEnviron(t *testing.T, cc *controlClient) string {
	t.Helper()
	obs := childEnvironObserver{
		status: func() (int, int, error) {
			st, err := cc.Status(context.Background())
			if err != nil {
				return 0, 0, err
			}
			return st.ChildPID, sp.cmd.Process.Pid, nil
		},
		environ: func(pid int) ([]byte, error) {
			return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		},
		statPPID: func(pid int) (int, error) {
			return procPPID(pid)
		},
		deadline: 10 * time.Second,
		interval: 50 * time.Millisecond,
	}
	data, err := obs.observeVerified()
	require.NoError(t, err, "verified child-environ observation failed")
	return string(data)
}

// procPPID reads field 4 of /proc/<pid>/stat (the parent pid). The comm
// field (2nd, parenthesized) may contain spaces and parens — parse from
// the LAST ')' so the split can never misalign on a weird process name.
func procPPID(pid int) (int, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(raw)
	at := strings.LastIndex(s, ")")
	if at < 0 || at+2 > len(s) {
		return 0, fmt.Errorf("procPPID: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[at+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("procPPID: short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("procPPID: bad ppid field in /proc/%d/stat: %w", pid, err)
	}
	return ppid, nil
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

	// Degradation steady state (review round 1): the FIRST child boots
	// with the PLATFORM env only. This test env wires no pull credential
	// (OPENCODE_SERVER_PASSWORD unset), so the US-70.1 spawn-time pull
	// fast-fails with spawn_env_no_credential — the loud degraded boot.
	// The healthy pulled-delta first spawn is pinned in
	// spawn_env_pull_exec_test.go.
	//
	// #1543: the environ read is PID-IDENTITY-GUARDED — under the race
	// CI leg's helper-process churn, an unguarded read can catch a
	// recycled pid (readable, alive, a stranger's env — the observed
	// flake). The guard verifies parentage + currency before accepting.
	{
		data := sp.verifiedChildEnviron(t, cc)
		require.True(t, strings.Contains(data, "GO_TEST_SUPERVISOR=1"),
			"platform env present on the pre-push child")
		require.False(t, strings.Contains(data, "PROBE_VAR="),
			"no delta on the pre-push child — platform-env-only is the degraded boot state")
	}

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

	// US-4a merge semantics, with pid self-consistency: read pid → read
	// environ → re-read pid; a changed pid means the supervisor moved on
	// (crash-backoff churn) and the read may have hit a transient/recycled
	// pid — retry. The delta+parent checks are INSIDE the Eventually
	// window: on loaded CI runners the first post-restart child can be a
	// churn respawn without the handed delta (kind-era flake, 2026-08-26);
	// the window must keep searching for the child that carries BOTH.
	// Nothing is weakened — the FINAL read must still satisfy pid
	// stability, the platform env, and the handed delta together.
	var data []byte
	var secondPID int
	require.Eventually(t, func() bool {
		pid := sp.childPIDOf(t, cc)
		if pid <= 0 || pid == firstPID {
			return false
		}
		d, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			return false
		}
		if sp.childPIDOf(t, cc) != pid {
			return false // transient pid — the supervisor replaced it mid-read
		}
		if !strings.Contains(string(d), "PROBE_VAR=handed-via-socket\x00") {
			return false // churn respawn without the delta — keep waiting
		}
		if !strings.Contains(string(d), "GO_TEST_SUPERVISOR=1") {
			return false // merge semantics: parent env must ride along
		}
		data = d
		secondPID = pid
		return true
	}, 15*time.Second, 100*time.Millisecond, "the next real child must run parent+delta")
	require.NotEqual(t, firstPID, secondPID, "restart must swap the child process")
	require.True(t, strings.Contains(string(data), "PROBE_VAR=handed-via-socket\x00"),
		"the socket-handed delta reached the next spawn")
	require.True(t, strings.Contains(string(data), "GO_TEST_SUPERVISOR=1"),
		"merge semantics: the parent env is retained alongside the handed delta (firstPID=%d secondPID=%d envLen=%d)",
		firstPID, secondPID, len(data))

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

// The US-4a boot-ordering tests (sidecar push precedes mux serving) were
// removed with the push itself: under native-sidecar startup gating the
// push dialed a control socket in a container that could not have
// started yet — structurally always "connection refused" (US-70.1 /
// design 0057 R2). The successor invariants — the FIRST spawn pulls the
// delta from the sidecar mux, a dead mux degrades loudly, spawn never
// blocks — are pinned in spawn_env_pull_exec_test.go against the REAL
// subcommand.

// --- #1543: pid-identity-guarded child-environ observation ---------------
//
// The lifecycle test's platform-env assertion reads
// /proc/<ChildPID>/environ. That read has an unguarded identity window:
// if the stub child dies between Status and the read, the supervisor's
// crash recovery has not necessarily run yet (Status still reports the
// dead pid) while the runner's parallel test helpers churn pids fast
// enough for the SAME number to be reallocated — the read then returns
// a stranger's environ: readable, alive, and missing GO_TEST_SUPERVISOR
// (the observed occurrence-2 failure: "Should be true" with a healthy
// Read). The guard below verifies the observed pid is still the
// supervisor's OWN child (by /proc/<pid>/stat ppid) and still the
// CURRENT child (by re-Status) before accepting the reading, retrying
// through the churn otherwise.

// childEnvironObserver is the injectable seam for the guard's tests.
type childEnvironObserver struct {
	status   func() (childPID int, supervisorPID int, err error)
	environ  func(pid int) ([]byte, error)
	statPPID func(pid int) (int, error)
	sleep    func(time.Duration)
	deadline time.Duration
	interval time.Duration
}

// observeVerified returns the environ of the supervisor's current stub
// child, accepting a reading only when BOTH identity checks hold at
// read time: the pid's parent is the supervisor, and a re-Status still
// names the same pid (crash recovery has not moved on). Retries until
// the deadline; the returned error names the pid-confusion mechanism so
// a genuine env bug is never misread as flake churn.
func (o childEnvironObserver) observeVerified() ([]byte, error) {
	start := time.Now()
	for {
		pid, supPID, err := o.status()
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		ppid, statErr := o.statPPID(pid)
		if statErr == nil && ppid == supPID {
			data, readErr := o.environ(pid)
			if readErr == nil {
				// Re-status: the child must STILL be current — a read
				// accepted against a pid crash-recovery has already
				// replaced is a stale-env reading.
				if pidNow, _, err2 := o.status(); err2 == nil && pidNow == pid {
					return data, nil
				}
			}
		}
		if time.Since(start) > o.deadline {
			return nil, fmt.Errorf("could not capture a verified child-environ reading within %s — the child pid churned (died + recycled by parallel test helpers) on every attempt; if this repeats with a stable pid, the env composition itself is broken (#1543)", o.deadline)
		}
		if o.sleep != nil {
			o.sleep(o.interval) // the injected sleep models wait + world-advance
		} else {
			time.Sleep(o.interval)
		}
	}
}

// TestChildEnvironObserver_AcceptsStableChild: identity + currency both
// hold on the first attempt — the reading returns without retry.
func TestChildEnvironObserver_AcceptsStableChild(t *testing.T) {
	calls := 0
	obs := childEnvironObserver{
		status: func() (int, int, error) { calls++; return 4242, 7, nil },
		environ: func(pid int) ([]byte, error) {
			return []byte("GO_TEST_SUPERVISOR=1\x00PATH=/stub"), nil
		},
		statPPID: func(pid int) (int, error) { return 7, nil },
		sleep:    func(time.Duration) {},
		deadline: 2 * time.Second,
		interval: time.Millisecond,
	}
	data, err := obs.observeVerified()
	require.NoError(t, err)
	require.Contains(t, string(data), "GO_TEST_SUPERVISOR=1")
	require.Equal(t, 2, calls, "exactly the initial + currency re-status calls — no retry churn on a stable child")
}

// TestChildEnvironObserver_RetriesOnPPidMismatch: a recycled pid (wrong
// parent) is rejected and the retry captures the supervisor's real
// child — the occurrence-2 shape cannot pass a stranger's environ
// through.
func TestChildEnvironObserver_RetriesOnPPidMismatch(t *testing.T) {
	var statusCalls int
	pid := 4242
	obs := childEnvironObserver{
		status: func() (int, int, error) {
			statusCalls++
			return pid, 7, nil
		},
		environ: func(int) ([]byte, error) { return []byte("STRANGER_ENV=1"), nil },
		statPPID: func(p int) (int, error) {
			if p == 4242 {
				return 99, nil // recycled: parent is NOT the supervisor
			}
			return 7, nil
		},
		sleep:    func(time.Duration) { pid = 5151 }, // churn resolves on the first retry
		deadline: 2 * time.Second,
		interval: time.Millisecond,
	}
	data, err := obs.observeVerified()
	require.NoError(t, err)
	require.Contains(t, string(data), "STRANGER_ENV=1", "the second attempt's (legitimate) reading is returned")
	require.Greater(t, statusCalls, 2, "the mismatch must have caused at least one retry cycle")
}

// TestChildEnvironObserver_RetriesOnCurrencyFailure: ppid checks out but
// the child died and crash recovery moved on (re-Status names a new pid)
// — the stale reading is discarded and the new child captured.
func TestChildEnvironObserver_RetriesOnCurrencyFailure(t *testing.T) {
	var statusCalls int
	pid := 4242
	obs := childEnvironObserver{
		status: func() (int, int, error) {
			statusCalls++
			// Every OTHER call reports the moved-on child: the FIRST
			// status sees 4242, the currency re-status sees 5151, the
			// next full attempt sees 5151 consistently.
			if statusCalls%2 == 1 && pid == 4242 {
				return 4242, 7, nil
			}
			return 5151, 7, nil
		},
		environ: func(p int) ([]byte, error) {
			return []byte(fmt.Sprintf("PID_%d=1", p)), nil
		},
		statPPID: func(int) (int, error) { return 7, nil },
		sleep:    func(time.Duration) { pid = 5151 },
		deadline: 2 * time.Second,
		interval: time.Millisecond,
	}
	data, err := obs.observeVerified()
	require.NoError(t, err)
	require.Contains(t, string(data), "PID_5151", "only the CURRENT child's reading may be returned")
}

// TestChildEnvironObserver_FailsLoudlyOnPersistentChurn: never-verifying
// readings exhaust the deadline with the mechanism named — a bounded,
// honest failure instead of an unguarded stranger's environ.
func TestChildEnvironObserver_FailsLoudlyOnPersistentChurn(t *testing.T) {
	obs := childEnvironObserver{
		status:   func() (int, int, error) { return 4242, 7, nil },
		environ:  func(int) ([]byte, error) { return []byte("STRANGER=1"), nil },
		statPPID: func(int) (int, error) { return 99, nil }, // never ours
		sleep:    func(d time.Duration) {},
		deadline: 5 * time.Millisecond,
		interval: time.Millisecond,
	}
	_, err := obs.observeVerified()
	require.Error(t, err)
	require.Contains(t, err.Error(), "pid churned")
	require.Contains(t, err.Error(), "#1543")
}

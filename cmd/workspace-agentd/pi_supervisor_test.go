// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_supervisor_test.go — exec-level integration: the REAL
// `workspace-agentd supervise-pi` binary as a REAL subprocess (the
// supervisor_subprocess_test.go pattern), driving a REAL fake-pi child
// via the boot-attach knob. Pins what only exists at process start:
// env resolution, verify ordering, boot attach, and the SIGNAL
// contract — first signal runs the rpc.md graceful ladder (waits for
// pending work), a second signal exits at once.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// piSubprocessRun is one launched supervise-pi subprocess. The wait
// goroutine reaps exactly once; reaped closes and code holds the
// verdict, so waitExit and the cleanup can BOTH wait without
// double-consuming a one-shot channel.
type piSubprocessRun struct {
	cmd     *exec.Cmd
	fixture string
	reaped  chan struct{}
	code    int
}

// startPiSupervisorSubprocess builds the real agentd binary and runs
// `supervise-pi` with the given fixture env (fake pi binary wired via
// LLMSAFESPACES_PI_BINARY + the fake's file knobs).
func startPiSupervisorSubprocess(t *testing.T, extraEnv ...string) *piSubprocessRun {
	t.Helper()
	withTestLogger(t)
	bin := buildAgentdBinary(t)

	fixture := t.TempDir()
	for _, sub := range []string{"state", "stats", "script", "persist", "boot", "cmds"} {
		require.NoError(t, os.MkdirAll(filepath.Join(fixture, sub), 0o755))
	}
	fakePi := piFakeBinary(t)

	//nolint:gosec // the compiled agentd test binary + fixture paths
	cmd := exec.Command(bin, "supervise-pi")
	cmd.Env = append([]string{
		"HOME=" + t.TempDir(),
		"LLMSAFESPACES_PI_BINARY=" + fakePi,
		"LLMSAFESPACES_PI_SESSION_DIR=" + filepath.Join(fixture, "sessions"),
		"LLMSAFESPACES_PI_AGENT_DIR=" + filepath.Join(fixture, "agent"),
		"LLMSAFESPACES_PI_POLL_INTERVAL_SECONDS=0.2",
		"PI_FAKE=1",
		"PI_FAKE_STATE_DIR=" + filepath.Join(fixture, "state"),
		"PI_FAKE_STATS_DIR=" + filepath.Join(fixture, "stats"),
		"PI_FAKE_SCRIPT_DIR=" + filepath.Join(fixture, "script"),
		"PI_FAKE_PERSIST_DIR=" + filepath.Join(fixture, "persist"),
		"PI_FAKE_BOOTLOG_DIR=" + filepath.Join(fixture, "boot"),
		"PI_FAKE_CMDLOG_DIR=" + filepath.Join(fixture, "cmds"),
	}, extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())

	p := &piSubprocessRun{cmd: cmd, fixture: fixture, reaped: make(chan struct{})}
	go func() {
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if asExitError(err, &exitErr) {
				p.code = exitErr.ExitCode()
			} else {
				p.code = -1
			}
		}
		close(p.reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-p.reaped
	})
	return p
}

// waitExit waits for the subprocess exit with a deadline.
func (p *piSubprocessRun) waitExit(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case <-p.reaped:
		return p.code
	case <-time.After(d):
		t.Fatalf("supervise-pi did not exit within %s", d)
		return -1
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

func TestSupervisePiSubprocess_GracefulShutdown(t *testing.T) {
	p := startPiSupervisorSubprocess(t, "LLMSAFESPACES_PI_ATTACH_ON_BOOT=s-grace")

	// The child booted (boot log proves the boot-attach path spawned
	// the fake pi with the pinned session id).
	bootPath := filepath.Join(p.fixture, "boot", "s-grace.boot")
	require.Eventually(t, func() bool {
		lines := piFakeLines(bootPath)
		return len(lines) >= 1 &&
			strings.Contains(lines[0], "--session-id s-grace") &&
			strings.Contains(lines[0], "--mode rpc")
	}, 15*time.Second, 50*time.Millisecond, "boot attach must spawn the session child")

	// ONE SIGTERM: the graceful rpc.md ladder — stdin EOF first, the
	// child's default disposition exits cleanly, exit code 0.
	require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
	require.Equal(t, 0, p.waitExit(t, 20*time.Second), "graceful shutdown exits 0")
}

func TestSupervisePiSubprocess_RepeatSignalExitsAtOnce(t *testing.T) {
	// The child ignores EOF AND SIGTERM, and the grace window is long:
	// the first signal starts the "wait for pending work" posture, and
	// the SECOND must not wait out the grace — it collapses the ladder
	// to immediate SIGKILL and exits.
	p := startPiSupervisorSubprocess(t,
		"LLMSAFESPACES_PI_ATTACH_ON_BOOT=s-stuck",
		"LLMSAFESPACES_PI_SHUTDOWN_GRACE_SECONDS=30",
		"PI_FAKE_IGNORE_EOF=1",
		"PI_FAKE_TRAP_TERM=1")

	bootPath := filepath.Join(p.fixture, "boot", "s-stuck.boot")
	require.Eventually(t, func() bool {
		return len(piFakeLines(bootPath)) >= 1
	}, 15*time.Second, 50*time.Millisecond)

	start := time.Now()
	require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
	// rpc.md: "a second signal exits at once" — well inside the 30s
	// grace. Give the ladder room to reach the kill rung.
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
	p.waitExit(t, 15*time.Second)
	require.Less(t, time.Since(start), 20*time.Second,
		"repeat signal must not wait out the pending-work grace")
}

func TestSupervisePiSubprocess_MissingBinaryFailsLoudly(t *testing.T) {
	// Unset overlay marker + a binary path that does not exist: a
	// config error the controller can attribute (the pins-mandatory
	// posture — pi has no PATH-lookup legacy).
	withTestLogger(t)
	bin := buildAgentdBinary(t)
	//nolint:gosec // compiled test binary
	cmd := exec.Command(bin, "supervise-pi")
	// Hermetic env: this suite also runs INSIDE real workspace pods
	// where AGENTD_IMAGE_VOLUME/PI_IMAGE_VOLUME are set with real
	// pins — a leaked pod env would fail self-verify before the
	// config break under test. Minimal env = deterministic verdict.
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=/usr/bin:/bin",
		"LLMSAFESPACES_PI_BINARY=/nonexistent/pi",
		"LLMSAFESPACES_PI_SESSION_DIR=" + t.TempDir(),
		"LLMSAFESPACES_PI_AGENT_DIR=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "not found", "the failure names the config break")
	var exitErr *exec.ExitError
	require.True(t, asExitError(err, &exitErr))
	require.Equal(t, 1, exitErr.ExitCode())
}

func TestSupervisePiEnvParsing(t *testing.T) {
	withTestLogger(t)
	t.Run("bad poll interval falls back", func(t *testing.T) {
		d := piEnvDuration(nil, envPiPollInterval, piDefaultPollInterval)
		require.Equal(t, piDefaultPollInterval, d)
	})
	t.Run("negative interval falls back", func(t *testing.T) {
		t.Setenv(envPiPollInterval, "-3")
		d := piEnvDuration(nil, envPiPollInterval, piDefaultPollInterval)
		require.Equal(t, piDefaultPollInterval, d)
	})
	t.Run("fractional seconds parse", func(t *testing.T) {
		t.Setenv(envPiPollInterval, "0.25")
		d := piEnvDuration(nil, envPiPollInterval, piDefaultPollInterval)
		require.Equal(t, 250*time.Millisecond, d)
	})
	t.Run("env default", func(t *testing.T) {
		require.Equal(t, "/x", piEnvDefault(envPiSessionDir, "/x"))
		t.Setenv(envPiSessionDir, "/y")
		require.Equal(t, "/y", piEnvDefault(envPiSessionDir, "/x"))
	})
}

func TestSupervisePiPolicySwitchValidation(t *testing.T) {
	withTestLogger(t)
	log := zap.NewNop()
	t.Setenv(envPiImageVolume, "")
	t.Setenv(envPiBinary, "/nonexistent/pi")
	t.Setenv(envPiSessionDir, t.TempDir())
	t.Setenv(envPiAgentDir, t.TempDir())
	t.Setenv(envPiPolicy, "per-session")
	_, err := newPiSupervisorFromEnv(log)
	require.ErrorContains(t, err, "not found", "binary presence is checked before policy")

	t.Setenv(envPiBinary, piFakeBinary(t))
	t.Setenv(envPiPolicy, "nonsense")
	_, err = newPiSupervisorFromEnv(log)
	require.ErrorContains(t, err, "unknown LLMSAFESPACES_PI_POLICY")
	require.Contains(t, err.Error(), piPolicyPerSession)
	require.Contains(t, err.Error(), piPolicySingle)
}

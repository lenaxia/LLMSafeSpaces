// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_mode_test.go — US-2 (design 0051): the `--sidecar` mode.
//
// The sidecar is the policy half of the split: muxes, watchdog, SSE
// tracking, relay injector — everything EXCEPT owning the opencode child.
// Its process interactions (restart, vitals, cgroup metrics) all cross
// the Appendix-A socket. These tests pin the wiring invariants:
//
//   - credentials arrive via ENV (uid-2000 space; the 0600 uid-1000 files
//     in /sandbox-cfg are unreadable cross-uid) and missing credentials
//     are FATAL (D5.2/D5.3 doctrine — never boot ungated);
//   - the watchdog's restarter and vitals are socket-backed;
//   - the sidecar never spawns or reaps children;
//   - statusz sys-metrics and the pressure monitor read the WORKSPACE
//     container's cgroup through the socket, never the sidecar's own.

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSidecar_EnvCredentials_Present pins happy-path env resolution.
func TestSidecar_EnvCredentials_Present(t *testing.T) {
	t.Setenv("AGENTD_SIDECAR_PASSWORD", "pw-sidecar")
	t.Setenv("AGENTD_ADMIN_TOKEN", "tok")

	pw, err := readSidecarPasswordFromEnv()
	require.NoError(t, err)
	require.Equal(t, "pw-sidecar", pw)

	tok, err := resolveSidecarAdminTokenFromEnv()
	require.NoError(t, err)
	require.Equal(t, "tok", tok)
}

// TestSidecar_EnvCredentials_MissingIsFatal: no password / no admin token
// must refuse to boot — the D5.2/D5.3 fail-closed doctrine carried into
// sidecar mode. An empty password is equally fatal (guessable Basic
// credential class).
func TestSidecar_EnvCredentials_MissingIsFatal(t *testing.T) {
	t.Setenv("AGENTD_SIDECAR_PASSWORD", "")
	t.Setenv("AGENTD_ADMIN_TOKEN", "")
	_, err := readSidecarPasswordFromEnv()
	require.Error(t, err)
	_, err = resolveSidecarAdminTokenFromEnv()
	require.Error(t, err)

	t.Setenv("AGENTD_SIDECAR_PASSWORD", "present-but-admin-missing")
	_, err = readSidecarPasswordFromEnv()
	require.NoError(t, err)
	_, err = resolveSidecarAdminTokenFromEnv()
	require.Error(t, err)
}

// TestSidecarDeps_NoManagedProcess: the sidecar's deps must carry NO
// managedProcess — it does not own the child (that is supervise-opencode
// in the workspace container). Its restart path is the socket adapter.
func TestSidecarDeps_NoManagedProcess(t *testing.T) {
	withTestLogger(t)
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()

	deps := buildSidecarDeps(sidecarConfig{
		password:    "pw",
		adminToken:  "tok",
		controlAddr: srv.addr(),
	})
	require.Nil(t, deps.proc, "sidecar must never own a managedProcess")
	require.NotNil(t, deps.restarter, "watchdog restart path must be wired")
}

// TestSidecarDeps_WatchdogRestartCrossesSocket: an end-to-end sidecar
// watchdog restart — socket adapter against the real socket server —
// reaches the supervisor-side proc with the health_watchdog reason and
// the parity grace.
func TestSidecarDeps_WatchdogRestartCrossesSocket(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	deps := buildSidecarDeps(sidecarConfig{
		password:    "pw",
		adminToken:  "tok",
		controlAddr: srv.addr(),
	})
	require.NotNil(t, deps.restarter)
	deps.restarter.restart()

	require.Equal(t, int64(1), proc.restarts.Load())
	require.Equal(t, RestartReasonHealthWatchdog, *proc.lastReason.Load(),
		"the socket adapter must forward the closed reason enum, not a bare restart")
}

// TestSidecarDeps_VitalsAreSocketBacked: deps must carry the socket vitals
// gatherer (nil vitals would restore pre-#892 kill-on-timeout semantics —
// the exact regression US-2 must not ship).
func TestSidecarDeps_VitalsAreSocketBacked(t *testing.T) {
	withTestLogger(t)
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()

	deps := buildSidecarDeps(sidecarConfig{
		password:    "pw",
		adminToken:  "tok",
		controlAddr: srv.addr(),
	})
	_, ok := deps.vitals.(*socketVitalsGatherer)
	require.True(t, ok, "sidecar deps must wire socketVitalsGatherer, got %T", deps.vitals)
}

// TestSidecarDeps_PressureReadsWorkspaceCgroupViaSocket: the pressure
// monitor's readCurrent must be socket-backed — the sidecar's own cgroup
// is the wrong container (0050 finding).
func TestSidecarDeps_PressureReadsWorkspaceCgroupViaSocket(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	// Serve real cgroup numbers through the socket.
	r := writeCgroupFixture(t, "500\n", "1000\n", "usage_usec 1\n")
	srv := newControlSocketServerWithProcAndMetrics(t, "127.0.0.1:0", proc, r.read)
	go srv.serve()

	deps := buildSidecarDeps(sidecarConfig{
		password:    "pw",
		adminToken:  "tok",
		controlAddr: srv.addr(),
	})
	cur, err := deps.pressureMonitor.readCurrent()
	require.NoError(t, err)
	require.Equal(t, int64(500), cur, "pressure source must be the socket's workspace-cgroup numbers")
}

// TestSidecar_MarkerPathEnvOverride pins the unified marker rule: env
// LLMSAFESPACES_RESTART_MARKER_PATH overrides the single-container default.
// The controller stamps it (SidecarRestartMarkerPath, shared /sandbox-
// runtime tmpfs) on BOTH containers when agentdSidecar is enabled, so the
// sidecar's watchdog/reload writes and the supervisor's crash-path writes
// land on one cross-uid-readable file the next sidecar boot surfaces.
func TestSidecar_MarkerPathEnvOverride(t *testing.T) {
	require.Equal(t, RestartReasonMarkerPath, markerPathFromEnv(),
		"env unset → single-container default, byte-identical behavior")

	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", SidecarRestartMarkerPath)
	require.Equal(t, SidecarRestartMarkerPath, markerPathFromEnv())
}

// TestRestartMarker_SharedModeIsGroupReadable: markers written under the
// shared path must be group-readable (0640) — writers straddle uids
// (sidecar 2000, supervisor 1000) with only the pod's shared group 1000
// in common.
func TestRestartMarker_SharedModeIsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/marker.json"
	require.NoError(t, writeRestartReasonMarker(path, "health_watchdog", nil))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0640), info.Mode().Perm(),
		"marker must be readable by the pod's shared group across uids")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "health_watchdog")
}

// TestSidecar_SpawnEnvConsumerReady is the US-0.2(a) seam check: the
// supervisor's stored spawn env (US-1) must reach the next child's
// environment — the sidecar will push reload-composed env over the socket
// (full consumer flow lands with US-4's mount relocations, but the
// supervisor half is US-2's to keep honest).
func TestSidecar_SpawnEnvConsumerReady(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.healthCheckURL = ""
	p.start()
	defer p.stop()

	adapter := &managedProcAdapter{p: p}
	adapter.SetSpawnEnv(map[string]string{"PROBE_VAR": "socket-handed"})
	_, _ = adapter.Restart("credential_reload", 5)

	// The fake child re-execs the test binary; its env carried the probe.
	require.Eventually(t, func() bool {
		pid := p.pid()
		if pid <= 0 {
			return false
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			return false
		}
		return containsEnvPair(string(data), "PROBE_VAR=socket-handed")
	}, 5*time.Second, 100*time.Millisecond, "next spawn must run with the spawn_env-handed environment")
}

// --- small helpers (test-local) ---

func containsEnvPair(environ, pair string) bool {
	for _, entry := range strings.Split(environ, "\x00") {
		if entry == pair {
			return true
		}
	}
	return false
}

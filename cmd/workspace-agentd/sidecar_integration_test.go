// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_integration_test.go — US-2 (design 0051) integration level L1.
//
// Components integrated IN ONE PROCESS TREE (see
// docs/testing/0051-us2-integration-test-plan.md):
//
//   - the SUPERVISOR half: managedProcess supervising a REAL child
//     process (the TestHelperProcess fake opencode — real fork/exec,
//     real signals, real port binding), the managedProcAdapter, and the
//     real controlSocketServer on an ephemeral port;
//   - the SIDECAR half: buildSidecarDeps' real socket consumers —
//     socketRestarter (watchdog restart path), socketVitalsGatherer
//     (corroboration), socketOps.pressureMonitor (cgroup reads) — and
//     the real controlClient.
//
// The only fakes are the opencode BINARY (test re-exec) and the metrics
// source (fixture snapshot — live cgroup values drift between reads).
// Everything between them is production wiring.
//
// L2 (envtest pod admission) and L3+ (kind/TEST cluster) live elsewhere;
// this file pins the in-pod interaction contract end to end.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// integrationHarness is one supervisor+sidecar pair.
type integrationHarness struct {
	proc    *managedProcess
	srv     *controlSocketServer
	cc      *controlClient
	deps    serverDeps
	childFn func() (pid int, port int)
}

// newSidecarIntegrationHarness boots the supervisor (real child on an
// ephemeral port) and the sidecar deps pointed at the real socket.
func newSidecarIntegrationHarness(t *testing.T) *integrationHarness {
	t.Helper()
	withTestLogger(t)
	port := freeTCPPort(t)

	p := newTestManagedProcess(t, port, 0)
	p.healthCheckURL = ""
	p.start()
	t.Cleanup(p.stop)

	adapter := &managedProcAdapter{p: p}
	srv, err := newControlSocketServer("127.0.0.1:0", adapter)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.close() })
	// Fixture metrics: a STATIC snapshot so client reads are comparable.
	snapshot := &cgroupMetrics{
		MemoryCurrentBytes: 1111,
		MemoryMaxBytes:     2222,
		CPUUsageUsec:       3333,
		CPUThrottledUsec:   4444,
	}
	srv.metricsSource = func() *cgroupMetrics { return snapshot }
	go srv.serve()

	cc := newControlClient(srv.addr())
	deps := buildSidecarDeps(sidecarConfig{
		password:    "integration-pw",
		adminToken:  "integration-tok",
		controlAddr: srv.addr(),
	})

	h := &integrationHarness{proc: p, srv: srv, cc: cc, deps: deps}
	h.childFn = func() (int, int) { return p.pid(), port }
	return h
}

// TestIntegration_SidecarAndSupervisor_StatusAndMetrics: the sidecar's
// client observes the supervisor's real child, and the workspace-cgroup
// numbers cross the socket verbatim.
func TestIntegration_SidecarAndSupervisor_StatusAndMetrics(t *testing.T) {
	h := newSidecarIntegrationHarness(t)
	ctx := context.Background()

	// The child must be observed RUNNING through the socket.
	require.Eventually(t, func() bool {
		st, err := h.cc.Status(ctx)
		return err == nil && st.ChildPID > 0 && st.ChildState == "running"
	}, 5*time.Second, 100*time.Millisecond, "sidecar must observe the supervisor's real child as running")

	_, port := h.childFn()
	require.Eventually(t, func() bool { return isPortBound(fmt.Sprintf("127.0.0.1:%d", port)) },
		3*time.Second, 50*time.Millisecond, "real child must be serving")

	// Metrics cross verbatim (static fixture — see file header).
	m, err := h.cc.Metrics(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1111), m.MemoryCurrentBytes)
	require.Equal(t, int64(2222), m.MemoryMaxBytes)
	require.Equal(t, int64(3333), m.CPUUsageUsec)
	require.Equal(t, int64(4444), m.CPUThrottledUsec)

	// The sidecar's pressure seam reads the SAME numbers through its own
	// client (buildSidecarDeps wires socketOps.pressureMonitor).
	cur, err := h.deps.pressureMonitor.readCurrent()
	require.NoError(t, err)
	require.Equal(t, int64(1111), cur)
}

// TestIntegration_SidecarWatchdogRestart_ThroughSocket: the sidecar's
// watchdog restart path (socketRestarter → socket → adapter →
// managedProcess → real SIGTERM → respawn) swaps the real child.
func TestIntegration_SidecarWatchdogRestart_ThroughSocket(t *testing.T) {
	h := newSidecarIntegrationHarness(t)

	require.Eventually(t, func() bool { pid, _ := h.childFn(); return pid > 0 },
		3*time.Second, 50*time.Millisecond, "initial child must be up before the restart")
	oldPID, port := h.childFn()
	require.True(t, oldPID > 0)

	h.deps.restarter.restart() // the sidecar's only restart verb

	// A NEW real child must come up on the same port.
	require.Eventually(t, func() bool {
		newPID, _ := h.childFn()
		return newPID > 0 && newPID != oldPID && isPortBound(fmt.Sprintf("127.0.0.1:%d", port))
	}, 5*time.Second, 100*time.Millisecond, "the socket restart must produce a fresh serving child")

	// And the socket reports the new generation.
	st, err := h.cc.Status(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, oldPID, st.ChildPID)
}

// TestIntegration_SpawnEnvHandoff_CrossesToRealChild: the US-0.2(a) env
// handoff end-to-end — sidecar composes env → socket → supervisor memory
// → the NEXT real spawn's environment. This is the seam US-4's reload
// path will drive in production.
func TestIntegration_SpawnEnvHandoff_CrossesToRealChild(t *testing.T) {
	h := newSidecarIntegrationHarness(t)
	ctx := context.Background()

	// Route the wrapper through the adapter's base-factory seam with the
	// harness's fake child (production composes on defaultOpencodeCmdFactory).
	// The SOCKET server must speak to this adapter, so rebuild the server
	// pair against it (same child, same port) — wiring the adapter's
	// composition as the process factory exactly as newSupervisorProcess
	// does in production.
	h.proc.mu.Lock()
	base := h.proc.cmdFactory
	h.proc.mu.Unlock()
	adapter := &managedProcAdapter{p: h.proc, baseCmdFactory: base}
	h.proc.mu.Lock()
	h.proc.cmdFactory = adapter.composeChild
	h.proc.mu.Unlock()
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", adapter)
	go srv.serve()
	cc := newControlClient(srv.addr())

	require.NoError(t, cc.SpawnEnv(ctx, map[string]string{
		"USER_SECRET_A": "alpha",
		"USER_SECRET_B": "beta",
	}))
	_, err := cc.Restart(ctx, "credential_reload", 5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		pid, _ := h.childFn()
		if pid <= 0 {
			return false
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err != nil {
			return false
		}
		env := string(data)
		return strings.Contains(env, "USER_SECRET_A=alpha\x00") && strings.Contains(env, "USER_SECRET_B=beta\x00")
	}, 5*time.Second, 100*time.Millisecond, "the next real spawn must run with the socket-handed env")

	// US-4a merge semantics: the supervisor composes parent + handed delta
	// (platform vars win — the sidecar cannot compose the parent, A.4).
	data, err := os.ReadFile("/proc/" + strconv.Itoa(mustPID(t, h)) + "/environ")
	require.NoError(t, err)
	require.True(t, strings.Contains(string(data), "GO_TEST_FAKE_OPENCODE=1") &&
		strings.Contains(string(data), "USER_SECRET_A=alpha\x00"),
		"next spawn runs parent+delta: platform vars retained AND the handed delta applied")
}

// TestIntegration_Vitals_AgainstRealChildAndSocket: with the real child
// serving and the socket live, the sidecar's gatherer must return
// non-lethal evidence; when the child is NOT yet bound (boot window) the
// same evidence must classify as the respawn window, never HUNG.
func TestIntegration_Vitals_AgainstRealChildAndSocket(t *testing.T) {
	h := newSidecarIntegrationHarness(t)

	// Child up, socket up → open port, live pid → not lethal.
	v := h.deps.vitals.(interface {
		gather(context.Context) vitalSigns
	}).gather(context.Background())
	verdict, why := v.classify()
	require.NotEqual(t, verdictHung, verdict, "live serving child must never be lethal: %s", why)

	// Young-boot shape: refused dial (unbound port) + recent
	// last_restart_at → RESPAWN, never HUNG. Built through the REAL
	// gatherer with a real unbound port and a real socket server whose
	// status reports a young child.
	unbound := freeTCPPort(t)
	srv2 := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	srv2.proc.(*fakeRestartProc).overrideState.Store(&procStateOverride{
		pid: 123, state: "running", lastRestartAt: time.Now().Add(-2 * time.Second),
	})
	go srv2.serve()
	g := newSocketVitalsGatherer(fmt.Sprintf("127.0.0.1:%d", unbound), newControlClient(srv2.addr()))
	v2 := g.gather(context.Background())
	verdict2, why2 := v2.classify()
	require.Equal(t, verdictRespawn, verdict2,
		"refused dial on a young child is the respawn window, never the watchdog's to kill: %s", why2)
}

func mustPID(t *testing.T, h *integrationHarness) int {
	t.Helper()
	require.Eventually(t, func() bool { pid, _ := h.childFn(); return pid > 0 },
		3*time.Second, 50*time.Millisecond)
	pid, _ := h.childFn()
	return pid
}

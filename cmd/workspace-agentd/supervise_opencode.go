// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervise-opencode subcommand (design 0051 US-1).
//
// Phase 2 splits agentd into a sidecar (policy, credentials, muxes) and
// a same-uid supervisor that is PID 1 of the workspace container and
// owns the opencode child. US-1 delivers the supervisor role against
// the EXISTING binary/image: same agentd image, new mode. The process
// supervision machinery is the existing managedProcess — reused 1:1,
// not forked — with:
//
//   - the Appendix-A control socket on 127.0.0.1:4099 (the sidecar's
//     only interface once US-2 splits the containers);
//   - a supervisedProcIface adapter: control-socket vocabulary
//     (reason-enum restart, in-progress-wins, memory-only spawn env)
//     mapped onto managedProcess's existing restart() semantics.
//
// The supervisor scope invariant (design 0051 D1): this process is
// plumbing — spawn/reap/signal/status/metrics-forward. Nothing else
// grows here.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// runSuperviseOpencodeCommand is the `workspace-agentd supervise-opencode`
// entry point. It never returns normally; exit code communicates failure.
func runSuperviseOpencodeCommand(_ []string) int {
	log := newLogger()
	defer func() { _ = log.Sync() }()

	// PID 1 duties: reap orphans exactly like the current in-container
	// agentd does (#904/#908 paths unchanged).
	_ = becomeSubreaper()

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	proc := newSupervisorProcess()
	proc.start()
	adapter := &managedProcAdapter{p: proc}

	// Socket address: the wire CONTRACT fixes 127.0.0.1:4099 in-pod
	// (Appendix A.0) and production never sets the override — it exists
	// so the exec-level integration test can run the real subcommand on
	// an ephemeral port without colliding with anything else on the host
	// (same env-override pattern as LLMSAFESPACES_AGENT_CONFIG_PATH).
	addr := os.Getenv("LLMSAFESPACES_CONTROL_SOCKET_ADDR")
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", ControlSocketPort)
	}
	srv, err := newSupervisorControlServer(addr, adapter)
	if err != nil {
		log.Error("FATAL: control socket listen failed", zap.String("addr", addr), zap.Error(err))
		return 1
	}
	go srv.serve()
	log.Info("supervise-opencode: control socket serving", zap.String("addr", srv.addr()))

	<-rootCtx.Done()
	log.Info("supervise-opencode: shutdown signal")
	_ = srv.close()
	proc.stop()
	return 0
}

// newSupervisorProcess builds the supervisor's managedProcess with
// supervisor-mode flags. Extracted from runSuperviseOpencodeCommand so the
// configuration is pinnable by test (the command itself never returns).
//
//   - onChildStarted nil: no session tracker in supervisor mode (the
//     sidecar owns policy).
//   - skipHealthProbe: the probe URL would hit the sidecar's bearer-gated
//     readyz, and the supervisor never holds that token (D1 keeps it out
//     of uid-1000 space). The sidecar's watchdog + the pod probes own
//     health semantics in the split topology.
func newSupervisorProcess() *managedProcess {
	proc := &managedProcess{}
	proc.onChildStarted = nil
	proc.skipHealthProbe = true
	return proc
}

// newSupervisorControlServer builds the supervisor's control socket with
// the metrics source wired (US-2): the supervisor's own cgroup IS the
// workspace container's (0050 finding) — served over the socket for the
// sidecar's statusz/pressure/ops-metrics surfaces. Extracted so the
// wiring is pinnable by test.
func newSupervisorControlServer(addr string, adapter *managedProcAdapter) (*controlSocketServer, error) {
	srv, err := newControlSocketServer(addr, adapter)
	if err != nil {
		return nil, err
	}
	srv.metricsSource = newWorkspaceCgroupReader().read
	return srv, nil
}

// managedProcAdapter maps control-socket vocabulary (Appendix A) onto
// managedProcess. It holds no state of its own except the memory-only
// spawn env (A.2).
type managedProcAdapter struct {
	p *managedProcess
	// baseCmdFactory builds the child the spawn-env wrapper composes on
	// top of. Nil resolves to defaultOpencodeCmdFactory at first use
	// (production); tests inject the fake-opencode factory so the
	// wrapper does not silently switch the child to the production
	// `opencode` binary (absent on CI runners — the wrapper then
	// crash-loops a failing Start and restart blocks on upCh forever).
	baseCmdFactory func() *exec.Cmd
	// spawnEnv is the US-0.2(a) IPC-handed env delta: memory-only,
	// write-only, last-write-wins. MERGED onto the base factory's env at
	// the NEXT spawn (US-4a; wholesale replacement was US-2's interim
	// shape). Never returned over the socket (A.4 invariant 1), never
	// written to disk.
	spawnEnv []string
}

func (a *managedProcAdapter) factory() func() *exec.Cmd {
	if a.baseCmdFactory != nil {
		return a.baseCmdFactory
	}
	return defaultOpencodeCmdFactory
}

// Restart maps the reason-enum restart onto managedProcess. In-progress-
// wins is enforced at the socket (restartMu); the adapter is only
// reached when it holds the lock, so no second restart can be
// concurrently in flight here.
func (a *managedProcAdapter) Restart(reason string, graceSeconds int) (bool, bool) {
	if reason != "" {
		log.Info("control: restart requested", zap.String("reason", reason))
	}
	// grace_seconds maps to the SIGTERM→SIGKILL window (US-2 wiring of
	// the deferred US-1 item). Out-of-range values collapse to the
	// 5s default (the socket already clamps to 1..300; this is the
	// adapter-side guard for direct callers).
	grace := defaultRestartGrace
	if graceSeconds > 0 && graceSeconds <= 300 {
		grace = time.Duration(graceSeconds) * time.Second
	}
	a.p.restartWithGrace(grace)
	return true, false
}

// State reports child pid/state/restart count for hello/status.
func (a *managedProcAdapter) State() (pid int, state string, restarts int, lastRestartAt time.Time) {
	p := a.p.pid()
	state = "stopped"
	if p > 0 {
		state = "running"
	}
	a.p.mu.Lock()
	restarts = a.p.restartCount
	lastRestartAt = a.p.lastRestartAt
	a.p.mu.Unlock()
	return p, state, restarts, lastRestartAt
}

// SetSpawnEnv stores the composed child env (memory-only, A.2) and
// installs the factory wrapper so the NEXT spawn uses it.
func (a *managedProcAdapter) SetSpawnEnv(env map[string]string) {
	flat := make([]string, 0, len(env))
	for k, v := range env {
		flat = append(flat, k+"="+v)
	}
	a.spawnEnv = flat
	base := a.factory()
	a.p.mu.Lock()
	a.p.cmdFactory = func() *exec.Cmd {
		cmd := base()
		// US-4a merge semantics: the sidecar hands ONLY the secrets
		// delta (A.4 forbids env OUT of the supervisor, so the sidecar
		// cannot compose the parent); the supervisor composes parent +
		// delta with platform vars winning — buildEnvFrom parity.
		cmd.Env = parentPlusDelta(cmd.Env, env)
		return cmd
	}
	a.p.mu.Unlock()
}

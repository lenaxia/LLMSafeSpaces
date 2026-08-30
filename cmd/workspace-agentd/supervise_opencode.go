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
	"strings"
	"syscall"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"go.uber.org/zap"
)

// runSuperviseOpencodeCommand is the `workspace-agentd supervise-opencode`
// entry point. It never returns normally; exit code communicates failure.
func runSuperviseOpencodeCommand(_ []string) int {
	log := newLogger()
	defer func() { _ = log.Sync() }()

	// Step-2 migration: verify-before-exec, moved from the baked
	// entrypoint's bash (#863) into the supervisor itself — the main
	// container now starts here directly. Exit 81 keeps the controller's
	// AgentdVerificationFailed detection contract. Runs before ANY work
	// (socket, children, markers) — fail closed.
	//
	//nolint:errcheck // selfExe read failure → empty hash → pin mismatch → 81
	if err := runSupervisorSelfVerify("/proc/self/exe"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = writeTerminationLog(err.Error())
		return supervisorExitVerifyFailed
	}

	// PID 1 duties: reap orphans exactly like the current in-container
	// agentd does (#904/#908 paths unchanged).
	_ = becomeSubreaper()

	// Relocated entrypoint env work (step-2 migration precedent): rebuild
	// mise shims before the first child spawn. Build-time `mise reshim`
	// wrote to MISE_DATA_DIR, which the /workspace PVC mount shadows at
	// runtime — fresh PVC ⇒ empty shims dirs ⇒ toolchains unresolvable in
	// every non-interactive shell (harness tool shells never see `mise
	// activate`). Best-effort by design (D1: the supervisor is plumbing;
	// a broken mise degrades to the documented `mise which` fallback and
	// must never block boot).
	ensureMiseShims(log)

	// Design 0053 S2: same relocated-entrypoint-work precedent as
	// ensureMiseShims — install the redact PATH wrapper before the first
	// child spawn. Best-effort: a failed install degrades to the PATH
	// fallback and never blocks boot.
	ensureRedactWrapper(log)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	proc, adapter := newSupervisorProcess()
	proc.start()

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

// newSupervisorProcess builds the supervisor's managedProcess and its
// control-socket adapter with supervisor-mode flags. Extracted from
// runSuperviseOpencodeCommand so the configuration is pinnable by test
// (the command itself never returns).
//
//   - onChildStarted nil: no session tracker in supervisor mode (the
//     sidecar owns policy).
//   - skipHealthProbe: the probe URL would hit the sidecar's bearer-gated
//     readyz, and the supervisor never holds that token (D1 keeps it out
//     of uid-1000 space). The sidecar's watchdog + the pod probes own
//     health semantics in the split topology.
//
// Design 0053 S1: the opencode spawn base (overlay verify + binary
// resolution) is resolved ONCE here — before the socket, before any
// child — and shared by the process and the adapter, so a socket spawn-env
// push wraps the SAME verified factory instead of re-resolving or
// regressing to the PATH-lookup default. Verify failure exits 83/84 from
// opencodeSpawnBaseFactory before this function returns.
func newSupervisorProcess() (*managedProcess, *managedProcAdapter) {
	return newSupervisorProcessPulling(newSpawnEnvPullerForPod())
}

// newSupervisorProcessPulling is newSupervisorProcess with the spawn-env
// puller injected (tests pin the URL/bound; production resolves the pod
// mux + §D1 credential from the workspace password file).
func newSupervisorProcessPulling(puller *spawnEnvPuller) (*managedProcess, *managedProcAdapter) {
	base := opencodeSpawnBaseFactory()
	// US-70.1 (design 0057 R2): every spawn PULLS the current delta
	// (bounded wait + last-good memory cache) instead of consuming a
	// boot-time push that structurally cannot land. Never-block-spawn:
	// the bound holds under any mux failure mode.
	proc := &managedProcess{cmdFactory: withSpawnEnvPull(base, puller)}
	proc.onChildStarted = nil
	proc.skipHealthProbe = true
	return proc, &managedProcAdapter{p: proc, baseCmdFactory: base, spawnPuller: puller}
}

// newSpawnEnvPullerForPod resolves the production pull target: the pod's
// user mux (single netns, both modes) with the §D1 workspace credential
// read from the uid-1000-owned password file (A2: the supervisor reads
// ONLY files it owns; the HTTP boundary carries the crossing).
func newSpawnEnvPullerForPod() *spawnEnvPuller {
	pw := ""
	if b, err := os.ReadFile(agentd.PasswordPath); err == nil {
		pw = strings.TrimSpace(string(b))
	}
	return newSpawnEnvPuller(fmt.Sprintf("http://127.0.0.1:%d", agentd.AgentdPort), pw, defaultSpawnPullBound)
}

// defaultSpawnPullBound bounds the spawn-time pull (never-block-spawn).
// Generous vs a healthy mux (localhost, ms-scale) and vs the spawn path
// overall; the last-good cache makes longer outages non-blocking.
const defaultSpawnPullBound = 2 * time.Second

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
	// top of. Nil resolves to the overlay-aware production base at first
	// use (opencodeSpawnBaseFactory — legacy PATH lookup, or the
	// sha256-verified overlay binary when OPENCODE_IMAGE_VOLUME=1);
	// tests inject the fake-opencode factory so the wrapper does not
	// silently switch the child to the production `opencode` binary
	// (absent on CI runners — the wrapper then crash-loops a failing
	// Start and restart blocks on upCh forever). Production
	// (newSupervisorProcess) always sets it to the once-resolved base.
	baseCmdFactory func() *exec.Cmd
	// spawnEnv is the US-0.2(a) IPC-handed env delta: memory-only,
	// write-only, last-write-wins. MERGED onto the base factory's env at
	// the NEXT spawn (US-4a; wholesale replacement was US-2's interim
	// shape). Never returned over the socket (A.4 invariant 1), never
	// written to disk.
	spawnEnv []string
	// spawnPuller (US-70.1) surfaces spawned_rev + degraded reason for
	// the status endpoint — terminal verification at the point of
	// consumption (I4), loud degradation (I10).
	spawnPuller *spawnEnvPuller
}

// SpawnStatus reports the terminal spawn-env state for the control
// socket's status surface: the rev the child actually spawned with and
// the active degrade reason ("" healthy).
func (a *managedProcAdapter) SpawnStatus() (rev, degraded string) {
	if a.spawnPuller == nil {
		return "", ""
	}
	return a.spawnPuller.spawnedRev(), a.spawnPuller.degradedReason()
}

func (a *managedProcAdapter) factory() func() *exec.Cmd {
	if a.baseCmdFactory != nil {
		return a.baseCmdFactory
	}
	return opencodeSpawnBaseFactory()
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

// ensureMiseShims regenerates mise's shim directory (best-effort, never
// returns an error). See the call site for the PVC-shadowing rationale.
// Idempotent: mise itself no-ops when shims are current.
func ensureMiseShims(log *zap.Logger) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		if log != nil {
			log.Info("supervise-opencode: mise absent (degraded base?); skipping shim rebuild")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// G204: `mise` is resolved via LookPath on the container's PATH — the
	// same trust domain as PID 1 itself (an attacker who can inject PATH
	// here already owns the container). Fixed argv "reshim"; output is
	// only logged, never executed or parsed.
	//nolint:gosec // G204: boot-time, fixed argv, same-uid binary from the runtime image
	out, err := exec.CommandContext(ctx, mise, "reshim").CombinedOutput()
	if err != nil {
		if log != nil {
			log.Warn("supervise-opencode: mise reshim failed (non-fatal; toolchains resolve via 'mise which')",
				zap.Error(err), zap.String("output", string(out)))
		}
		return
	}
	if log != nil {
		log.Info("supervise-opencode: mise shims rebuilt")
	}
}

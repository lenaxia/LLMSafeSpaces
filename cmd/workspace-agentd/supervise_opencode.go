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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

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

	proc, adapter := newSupervisorProcess(rootCtx)
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
// child — and shared by the process and the adapter, so the spawn-env
// composition wraps the SAME verified factory instead of re-resolving or
// regressing to the PATH-lookup default. Verify failure exits 83/84 from
// opencodeSpawnBaseFactory before this function returns.
//
// US-70.1 (design 0057 R2): every spawn PULLS the secrets delta from the
// sidecar user mux with a bounded wait (preSpawn, outside managedProcess
// locks); composeChild merges the fresh-or-last-good delta onto the base
// env and records spawned_rev at the point of consumption (I4).
func newSupervisorProcess(ctx context.Context) (*managedProcess, *managedProcAdapter) {
	base := opencodeSpawnBaseFactory()
	a := &managedProcAdapter{
		baseCmdFactory: base,
		puller:         newSpawnEnvPuller(spawnEnvPullAddr(), supervisorSpawnCredential()),
		pullCtx:        ctx,
		filesPuller:    newSpawnFilesPuller(spawnEnvPullAddr(), supervisorSpawnCredential()),
		delivery:       fileDeliveryFromEnv(),
	}
	proc := &managedProcess{cmdFactory: a.composeChild, preSpawn: a.preSpawn}
	proc.onChildStarted = nil
	proc.skipHealthProbe = true
	a.p = proc
	return proc, a
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

// spawnEnvStateReport is the terminal-verified spawn-env state (US-70.1,
// design 0057 I4/I10): the revision of the delta the last-spawned child
// actually spawned with, plus the machine-readable degrade reason when
// delivery is not complete. Crosses the control socket as part of
// `status`; contains no env values (A.4 invariant 1).
type spawnEnvStateReport struct {
	SpawnedRev string
	Degraded   bool
	Reason     string
	// R2b (#1165): the file-class delivery state — the terminal revision
	// over the files actually written by this uid and its degrade reason
	// ("" healthy).
	FilesRev    string
	FilesReason string
}

// managedProcAdapter maps control-socket vocabulary (Appendix A) onto
// managedProcess. It owns the US-70.1 spawn-time pull state: the
// last-good delta and the spawned revision live here, in supervisor
// memory only (design 0057 I7 — no PVC, no logs, no socket read-back of
// values).
type managedProcAdapter struct {
	p *managedProcess
	// baseCmdFactory builds the child the spawn-env composition wraps.
	// Nil resolves to the overlay-aware production base at first use
	// (opencodeSpawnBaseFactory — legacy PATH lookup, or the
	// sha256-verified overlay binary when OPENCODE_IMAGE_VOLUME=1);
	// tests inject the fake-opencode factory so the wrapper does not
	// silently switch the child to the production `opencode` binary
	// (absent on CI runners — the wrapper then crash-loops a failing
	// Start and restart blocks on upCh forever). Production
	// (newSupervisorProcess) always sets it to the once-resolved base.
	baseCmdFactory func() *exec.Cmd

	// pullMu guards the US-70.1 pull state below. Deliberately NOT
	// managedProcess's mutex: preSpawn runs outside it (bounded I/O must
	// never block restart/state/metrics readers), and the socket's
	// spawn_env push can land between spawns.
	pullMu         sync.Mutex
	puller         *spawnEnvPuller
	pullCtx        context.Context
	currentDelta   map[string]string
	degradedReason string
	spawnedRev     string
	// servedEnvRevAnchor is the "<seq>:<manifestHash>" prefix of the last
	// successfully pulled served rev (US-70.2): composeChild anchors
	// spawned_rev with it while the content hash stays self-computed
	// (I4). Empty for legacy (content-hash-only) served revs.
	servedEnvRevAnchor string

	// R2b (#1165) file-class delivery state, same locking discipline as
	// the env pull state (preSpawn is the sole writer; SpawnEnvState the
	// sole reader of record).
	filesPuller *spawnEnvPuller
	delivery    fileDelivery
	filesRev    string
	// servedFilesRevAnchor anchors files_rev the same way
	// servedEnvRevAnchor anchors spawned_rev.
	servedFilesRevAnchor string
	filesReason          string
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

// SetSpawnEnv stores a pushed secrets delta (the legacy US-0.2(a)/US-4a
// push path — superseded by the spawn-time pull; demolition lands with
// US-70.5). The delta composes onto the base env at the NEXT spawn; a
// later successful pull supersedes it (pull is the sole correctness
// source, I3). Memory-only (A.2): never returned over the socket, never
// written to disk.
func (a *managedProcAdapter) SetSpawnEnv(env map[string]string) {
	a.pullMu.Lock()
	a.currentDelta = env
	a.pullMu.Unlock()
}

// preSpawn is the US-70.1 spawn-time pull, invoked by the supervisor
// loop before every child build (first boot, restarts, crash recovery).
// A successful pull — including an EMPTY delta, which is revocation (I12:
// absence is the delete) — becomes the current delta and clears any
// degrade. A failed pull keeps the last-good delta from memory and marks
// the degrade with a machine-readable reason. Never blocks beyond the
// puller's bound: never-block-boot extends to never-block-spawn.
// preSpawn is the US-70.1 spawn-time pull (env) plus the R2b file-class
// delivery, invoked by the supervisor loop before every child build.
// Env: a successful pull — including an EMPTY delta (revocation, I12) —
// becomes the current delta; failure keeps the last-good delta in memory
// and degrades loudly. Files: the pulled manifest is applied by THIS uid
// (ownership by construction); the on-disk delivered set is the
// last-good cache — a failed pull keeps it and degrades loudly, and
// tools read files at invocation time, so the next successful apply
// heals without losing the session. Never blocks beyond the bounds.
func (a *managedProcAdapter) preSpawn() {
	if a.puller != nil {
		res, reason, err := a.puller.pullBounded(a.pullCtx)
		a.pullMu.Lock()
		if err != nil {
			if a.degradedReason != reason {
				log.Warn("spawn-env pull failed; spawning with the last-good delta",
					zap.String("reason", reason), zap.Error(err))
			}
			a.degradedReason = reason
		} else {
			a.degradedReason = ""
			a.currentDelta = res.Env
			a.servedEnvRevAnchor = anchoredPrefix(res.Rev)
		}
		a.pullMu.Unlock()
	}

	if a.filesPuller != nil {
		files, reason, err := a.filesPuller.pullFilesBounded(a.pullCtx)
		a.pullMu.Lock()
		if err != nil {
			if a.filesReason != reason {
				log.Warn("spawn-files pull failed; keeping the delivered set",
					zap.String("reason", reason), zap.Error(err))
			}
			a.filesReason = reason
		} else if rev, applyErr := a.delivery.apply(files.Files); applyErr != nil {
			if errors.Is(applyErr, errBadDeliveryPath) {
				reason = spawnFilesReasonBadPath
			} else {
				reason = spawnFilesReasonUnavailable
			}
			if a.filesReason != reason {
				log.Warn("spawn-files delivery failed; keeping the delivered set",
					zap.String("reason", reason), zap.Error(applyErr))
			}
			a.filesReason = reason
		} else {
			a.filesReason = ""
			a.servedFilesRevAnchor = anchoredPrefix(files.Rev)
			a.filesRev = anchoredSpawnRev(a.servedFilesRevAnchor, rev)
		}
		a.pullMu.Unlock()
	}
}

func (a *managedProcAdapter) composeChild() *exec.Cmd {
	cmd := a.factory()()
	delta := a.snapshotDelta()
	var effective map[string]string
	if len(delta) > 0 {
		effective = effectiveDelta(cmd.Env, delta)
		cmd.Env = parentPlusDelta(cmd.Env, delta)
	}
	rev := spawnDeltaRev(effective)
	a.pullMu.Lock()
	a.spawnedRev = anchoredSpawnRev(a.servedEnvRevAnchor, rev)
	a.pullMu.Unlock()
	return cmd
}

func (a *managedProcAdapter) snapshotDelta() map[string]string {
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	return a.currentDelta
}

// SpawnEnvState reports the terminal-verified spawn-env state for the
// control socket's status method (I10/I13: loud, machine-readable).
func (a *managedProcAdapter) SpawnEnvState() spawnEnvStateReport {
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	return spawnEnvStateReport{
		SpawnedRev:  a.spawnedRev,
		Degraded:    a.degradedReason != "" || a.filesReason != "",
		Reason:      a.degradedReason,
		FilesRev:    a.filesRev,
		FilesReason: a.filesReason,
	}
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

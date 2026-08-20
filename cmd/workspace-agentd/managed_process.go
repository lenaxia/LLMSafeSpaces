// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// managedProcess supervises the opencode serve process.
//
// Lifecycle model
// ---------------
// One **supervisor goroutine** owns the *exec.Cmd. The supervisor is
// the SOLE caller of cmd.Wait() — concurrent Wait() on the same Cmd
// is undefined, and was the proximate cause of Bug 2 in worklog 0125
// where restart() called Wait() while the previous start()'s monitor
// goroutine was already waiting. The first Wait to return won, but
// the kernel had not yet reaped the child, so a new opencode failed
// to bind port 4096.
//
// The supervisor loop is:
//
//  1. spawn child via p.cmdFactory()
//  2. announce "child is up" by closing p.upCh and re-creating it
//     (so the next iteration has a fresh signal)
//  3. p.cmd.Wait() — blocks until the child exits
//  4. inspect intent flags:
//     - stopRequested → exit the goroutine (no restart)
//     - restartRequested → loop with restartCount=0
//     - neither → loop with backoff
//
// Callers communicate intent via:
//
//   - start() — spawn the supervisor; idempotent under the mutex
//   - restart() — signal the current child, set restartRequested,
//     await the next "child is up"
//   - stop() — signal the current child, set stopRequested, await
//     supervisor goroutine exit
//
// All three are safe to call from any goroutine.
type managedProcess struct {
	mu            sync.Mutex
	cmd           *exec.Cmd
	restartCount  int
	lastRestartAt time.Time

	// adminToken is the resolved-once admin-mux bearer (boot, main) used
	// by healthProbeAfterRestart — closes the gate/use re-read TOCTOU.
	adminToken string

	// cmdFactory builds a fresh *exec.Cmd for each (re)start.
	// Production wires `opencode serve …`; tests inject a fake.
	// Set lazily by start() if nil at call time, so production code
	// can construct managedProcess{} with no arguments and tests can
	// pre-populate the field before calling start().
	cmdFactory func() *exec.Cmd

	// healthCheckURL is the URL polled after restart() to verify the
	// new child is serving. Empty means skip the health check.
	healthCheckURL string

	// skipHealthProbe suppresses the post-restart probe even when
	// healthCheckURL is set (supervisor mode: the URL would target the
	// sidecar's bearer-gated readyz, and the supervisor must never hold
	// that token — design 0051 D1 keeps it out of uid-1000 space; the
	// sidecar's watchdog owns health semantics in the split topology).
	skipHealthProbe bool

	// supervisor lifecycle channels.
	//
	//   upCh — closed by the supervisor every time a fresh child is
	//          running. restart() reads upCh under the mutex,
	//          releases the mutex, and waits on the captured channel
	//          for the supervisor to close it.
	//   doneCh — closed by the supervisor when it exits permanently
	//            (after stop()). stop() awaits this.
	//   stopRequested / restartRequested — flags read by the
	//          supervisor inside the loop body to decide what to do
	//          after the current child exits. Both protected by mu.
	upCh             chan struct{}
	doneCh           chan struct{}
	stopRequested    bool
	restartRequested bool

	// onChildStarted, when non-nil, is invoked by the supervisor
	// goroutine each time a fresh child has been successfully started —
	// once per opencode generation (first boot, operator restarts, crash
	// recovery). Invoked after Start() succeeds and before upCh closes,
	// so the callback observes the generation boundary before the new
	// child can serve any request. Production wires the tracker's
	// busy-flag reset (design 0050 D2); nil = no callback.
	onChildStarted func()

	// probeWg tracks any in-flight healthProbeAfterRestart
	// goroutines. stop() waits on it so the probe can no longer
	// touch the package-level log after stop() returns. Without this
	// a leaked probe and a test's t.Cleanup that swaps out the
	// logger race on `log` (caught by go test -race).
	probeWg sync.WaitGroup
}

const maxBackoffSec = 30

// healthCheckURL targets agentd's own /v1/readyz on the admin port,
// NOT opencode's :4096 (which serves the SPA and requires HTTP basic
// auth on every endpoint — so the previous :4096 URL always failed).
// agentd's readyz reports agentd liveness plus a kernel-level TCP check
// on opencode's port (design 0050 D4) — never opencode responsiveness,
// so a CPU-starved-but-alive opencode does not fail this probe. When
// AGENTD_ADMIN_TOKEN is set the
// endpoint is Bearer-gated (server.go requireBearerToken); the probe
// attaches the token from the same env var.
//
// Built from the agentd.AgentdAdminPort constant (not a hardcoded
// literal) so the two cannot drift.
var healthCheckURL = fmt.Sprintf("http://localhost:%d/v1/readyz", agentd.AgentdAdminPort)

// start spawns the supervisor goroutine. Calling start() more than
// once is a no-op (it does NOT restart — use restart() for that).
//
// In production this is invoked exactly once at agentd boot. The
// supervisor goroutine outlives every individual *exec.Cmd; restarts
// are loop iterations inside the supervisor, not new goroutines.
func (p *managedProcess) start() {
	p.mu.Lock()
	if p.doneCh != nil {
		// Supervisor already running.
		p.mu.Unlock()
		return
	}
	if p.cmdFactory == nil {
		p.cmdFactory = defaultOpencodeCmdFactory
	}
	if p.healthCheckURL == "" {
		p.healthCheckURL = healthCheckURL
	}
	p.upCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go p.supervise()
}

// supervise is the supervisor goroutine. Sole owner of cmd.Wait().
//
// Loop invariants:
//
//   - On entry, the previous iteration's child (if any) has been
//     reaped by Wait(). Port resources are free.
//   - p.cmd is overwritten each iteration; the previous value's
//     ProcessState is set, exposing whether the child exited cleanly.
//   - p.upCh is closed exactly once per spawned child, then replaced
//     with a fresh channel before the next iteration.
//
// The loop terminates only when stopRequested is observed after a
// child exit. doneCh is closed before return so stop() can join.
func (p *managedProcess) supervise() {
	defer func() {
		p.mu.Lock()
		close(p.doneCh)
		p.mu.Unlock()
	}()

	for {
		p.mu.Lock()
		// stop() may have run while we were in a crash backoff sleep:
		// it signals the child it saw (possibly the dead one) and waits
		// on doneCh. Without this check the supervisor would respawn a
		// child nobody will ever signal, and stop() would hang forever
		// (found while building the real-subprocess regression harness
		// for the respawn-boot window; pinned by
		// TestManagedProcess_StopDuringCrashBackoffReturns).
		if p.stopRequested {
			p.mu.Unlock()
			return
		}
		// Build a fresh cmd. exec.Cmd is single-shot — one Start +
		// one Wait per instance.
		cmd := p.cmdFactory()
		if err := cmd.Start(); err != nil {
			log.Error("failed to start opencode", zap.Error(err))
			// Reset request flags so the next loop iteration can
			// decide based on backoff rather than a stale signal.
			p.restartRequested = false
			stopReq := p.stopRequested
			p.mu.Unlock()
			if stopReq {
				return
			}
			// Treat Start() failure the same as a crash: backoff.
			p.applyBackoff()
			continue
		}
		p.cmd = cmd
		// Register with the orphan reaper from Start to Wait: the
		// reaper must never steal this child's exit status (#904).
		pkgOrphanReaper.track(cmd.Process.Pid)
		p.lastRestartAt = time.Now()
		log.Info("opencode started",
			zap.Int("pid", cmd.Process.Pid),
			zap.Int("restartCount", p.restartCount))

		// Announce the new child is up. close() must be called
		// exactly once per channel; we replace upCh before the next
		// iteration so the next close() targets a fresh channel.
		upCh := p.upCh
		p.upCh = make(chan struct{})
		p.mu.Unlock()

		// Generation boundary (design 0050 D2): the previous child has
		// been reaped (Wait returned before this iteration) and the new
		// one is started but not yet announced — run the callback before
		// upCh closes so it lands before any request reaches the new
		// child.
		if p.onChildStarted != nil {
			p.onChildStarted()
		}
		close(upCh)

		// Sole Wait() in the codebase. This is the contract that
		// Bug 2 broke.
		waitErr := cmd.Wait()
		pkgOrphanReaper.untrack(cmd.Process.Pid)

		p.mu.Lock()
		stopReq := p.stopRequested
		restartReq := p.restartRequested
		p.restartRequested = false
		p.mu.Unlock()

		if stopReq {
			log.Info("opencode supervisor exiting",
				zap.Int("pid", cmd.Process.Pid),
				zap.Error(waitErr))
			return
		}
		if restartReq {
			// Operator-initiated restart: reset counters and loop
			// immediately (no backoff).
			p.mu.Lock()
			p.restartCount = 0
			p.mu.Unlock()
			continue
		}

		// Crash path: classify exit, handle OOM, record metric, log, backoff, loop.
		exitKind := classifyExit(waitErr)
		if isOOMExit(exitKind) {
			handleOOMExit(workspaceIDFromEnv(), markerPathFromEnv())
		} else {
			if err := writeRestartReasonMarker(markerPathFromEnv(), "crash", nil); err != nil {
				log.Error("failed to write restart-reason marker", zap.Error(err))
				pkgOpsMetrics.RecordMarkerWriteFailure(workspaceIDFromEnv(), "crash")
			} else {
				logRestartReasonAtWrite("crash", nil, log.Core())
			}
			pkgOpsMetrics.RecordRestart(workspaceIDFromEnv(), "crash")
		}
		log.Warn("opencode exited unexpectedly",
			zap.Error(waitErr),
			zap.Int("restartCount", p.restartCount))
		p.applyBackoff()
	}
}

// applyBackoff advances the restart counter and sleeps. Called only
// from the supervisor goroutine after an unexpected child exit.
//
// Resets the counter when the previous child stayed up for >60s,
// which prevents legitimate long-running children from being
// penalized by historical crashes.
func (p *managedProcess) applyBackoff() {
	p.mu.Lock()
	p.restartCount++
	backoff := time.Duration(1<<min(p.restartCount, 5)) * time.Second
	if backoff > maxBackoffSec*time.Second {
		backoff = maxBackoffSec * time.Second
	}
	if time.Since(p.lastRestartAt) > 60*time.Second {
		p.restartCount = 0
		backoff = time.Second
	}
	p.mu.Unlock()
	log.Info("restarting opencode", zap.Duration("backoff", backoff))
	time.Sleep(backoff)
}

// pid returns the PID of the currently supervised opencode child, or 0
// when no child process is attached (start never called, or the previous
// child exited and the supervisor is between iterations). Used by the
// health-watchdog's starvation corroboration (watchdog_vitals.go) to read
// the child's CPU counter.
//
// Mutex-guarded: supervise() overwrites p.cmd under the same lock. The
// returned pid may be stale by the time it is used — callers must treat
// read failures as "no evidence" rather than retry.
func (p *managedProcess) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// childStartedAt returns the time the CURRENT child was started (zero
// time when no child has been started yet). The health-watchdog's vitals
// gatherer (watchdog_vitals.go) uses it to disarm the kill during the
// respawn boot window: a freshly spawned child has not bound its port,
// so a refused dial against a young pid is boot, not hang. Mutex-guarded
// alongside pid(); the same staleness caveat applies.
func (p *managedProcess) childStartedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastRestartAt
}

// restart signals the current child to exit and blocks until the
// supervisor has spawned and started a replacement. Safe to call from
// HTTP handlers; bounded by SIGKILL fallback (5s) + Start() time.
//
// If the supervisor isn't running (start() was never called), this is
// a no-op — callers in tests pass nil rather than building a partial
// supervisor.
func (p *managedProcess) restart() {
	p.restartWithGrace(defaultRestartGrace)
}

// defaultRestartGrace is the historical SIGTERM→SIGKILL window. Kept as
// a named constant so the single-container behavior is pinned by test
// (US-2 parameterized the window for control-socket callers; every
// existing in-process caller keeps exactly this budget).
const defaultRestartGrace = 5 * time.Second

// restartWithGrace is restart() with a caller-supplied SIGTERM→SIGKILL
// window (design 0051 A.2 grace_seconds). A shorter window serves
// socket-requested restarts; a longer one would need a matching bump of
// the pod's terminationGracePeriodSeconds to be observable.
func (p *managedProcess) restartWithGrace(grace time.Duration) {
	if grace <= 0 {
		grace = defaultRestartGrace
	}
	p.mu.Lock()
	if p.doneCh == nil {
		// Supervisor not running.
		p.mu.Unlock()
		return
	}
	p.restartRequested = true
	cmd := p.cmd
	upCh := p.upCh
	pid := 0
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	p.mu.Unlock()

	if pid > 0 {
		log.Info("stopping opencode for restart", zap.Int("pid", pid))
		_ = syscall.Kill(pid, syscall.SIGTERM)
		// Give the child up to `grace` to exit on SIGTERM, then SIGKILL.
		// We can't Wait() here (supervisor owns Wait), so we rely on
		// the supervisor's loop iteration: when the child exits, the
		// supervisor will see restartRequested and loop. We poll
		// upCh to know when the new child is up.
		//
		// Uses syscall.Kill instead of cmd.Process.Signal/Kill to
		// avoid a data race with cmd.Wait() in supervise(): both
		// would concurrently access the same *os.Process struct.
		killTimer := time.AfterFunc(grace, func() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		})
		defer killTimer.Stop()
	}

	// Block until the supervisor closes the upCh that was current
	// when restart() was called. The supervisor closes upCh only
	// after a successful Start(), guaranteeing the new child is up
	// AND the old one is reaped (Wait returned).
	<-upCh

	// Optional: post-restart health probe. Spawn a background
	// goroutine; restart() does not block on it. Pre-fix this used a
	// fresh context to outlive the triggering HTTP request; same
	// reason here. Tracked via probeWg so stop() can wait for it,
	// preventing log-pointer races during test teardown.
	if p.healthCheckURL != "" && !p.skipHealthProbe {
		p.probeWg.Add(1)
		go func() {
			defer p.probeWg.Done()
			p.healthProbeAfterRestart()
		}()
	}
}

// stop signals the current child and blocks until the supervisor
// goroutine exits. Safe to call from any goroutine. Idempotent: a
// second stop() returns immediately because doneCh is already closed.
func (p *managedProcess) stop() {
	p.mu.Lock()
	if p.doneCh == nil {
		p.mu.Unlock()
		return
	}
	p.stopRequested = true
	cmd := p.cmd
	doneCh := p.doneCh
	pid := 0
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	p.mu.Unlock()

	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		killTimer := time.AfterFunc(5*time.Second, func() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		})
		defer killTimer.Stop()
	}

	<-doneCh

	// Drain any in-flight health-probe goroutines so they cannot
	// touch the package-level log after stop() returns. Bounded by
	// healthProbeAfterRestart's own 15s timeout AND the early-abort
	// on doneCh close — in practice this returns within tens of ms.
	p.probeWg.Wait()
}

// healthProbeAfterRestart polls the configured health URL up to 10
// times at 1-second intervals. Logs success or failure but does not
// block restart() — the probe is purely diagnostic.
//
// Aborts early if doneCh is closed by stop(): without this, the probe
// goroutine outlives the test that started it and races on the
// package-level log when withTestLogger restores the previous logger.
//
// Uses a fresh context: restart() may be invoked from a Gin handler
// whose ctx is canceled before the new child becomes ready, but we
// want the probe to outlive the triggering request.
func (p *managedProcess) healthProbeAfterRestart() {
	p.mu.Lock()
	doneCh := p.doneCh
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 10; i++ {
		select {
		case <-doneCh:
			return // supervisor shut down; abandon probe
		case <-time.After(time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.healthCheckURL, nil)
		if err != nil {
			log.Warn("health check request build failed", zap.Error(err))
			return
		}
		// readyz is Bearer-gated when an admin token is set (requireBearerToken).
		// Empty token = no auth required (dev escape hatch, D5.2-logged).
		if p.adminToken != "" {
			req.Header.Set("Authorization", "Bearer "+p.adminToken)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			log.Info("opencode healthy after restart")
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	log.Warn("opencode did not become healthy within 10s after restart")
}

// defaultOpencodeCmdFactory builds the production *exec.Cmd that runs
// `opencode serve` on the well-known port. Pulled out so tests can
// substitute a fake without touching this function.
func defaultOpencodeCmdFactory() *exec.Cmd {
	// G204: argument list is fixed at compile time; agentd.AgentPort
	// is a typed int constant. The only "variable" here is
	// fmt.Sprintf converting that constant to a string. noctx:
	// opencode is a long-running daemon, no per-call deadline.
	//nolint:gosec,noctx // G204/noctx: fixed argv, daemon process
	cmd := exec.Command("opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", agentd.AgentPort))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = buildEnvFrom(agentd.SecretsEnvPath)
	return cmd
}

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

	proc := &managedProcess{}
	proc.onChildStarted = nil // no session tracker in supervisor mode (sidecar owns policy)
	proc.start()
	adapter := &managedProcAdapter{p: proc}

	srv, err := newControlSocketServer(fmt.Sprintf("127.0.0.1:%d", ControlSocketPort), adapter)
	if err != nil {
		log.Error("FATAL: control socket listen failed", zap.Int("port", ControlSocketPort), zap.Error(err))
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

// managedProcAdapter maps control-socket vocabulary (Appendix A) onto
// managedProcess. It holds no state of its own except the memory-only
// spawn env (A.2).
type managedProcAdapter struct {
	p *managedProcess
	// spawnEnv is the US-0.2(a) IPC-handed env: memory-only, write-only,
	// last-write-wins. Applied by the cmdFactory wrapper at the NEXT
	// spawn. Never returned over the socket (A.4 invariant 1), never
	// written to disk.
	spawnEnv []string
}

// Restart maps the reason-enum restart onto managedProcess.restart().
// In-progress-wins is enforced at the socket (restartMu); the adapter
// is only reached when it holds the lock, so no second restart can be
// concurrently in flight here.
func (a *managedProcAdapter) Restart(reason string, graceSeconds int) (bool, bool) {
	if reason != "" {
		log.Info("control: restart requested", zap.String("reason", reason))
	}
	// grace_seconds maps to the SIGTERM→SIGKILL window; the current
	// restart() hardcodes 5s. Honor longer graces by NOT mapping them
	// yet — US-2 wires grace through when the kill-timer becomes a
	// parameter (noted there); socket contract already carries it.
	a.p.restart()
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
	a.p.mu.Lock()
	a.p.cmdFactory = func() *exec.Cmd {
		cmd := defaultOpencodeCmdFactory()
		cmd.Env = append([]string{}, a.spawnEnv...)
		return cmd
	}
	a.p.mu.Unlock()
}

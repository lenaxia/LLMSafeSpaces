// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"golang.org/x/sys/unix"
)

// Orphan zombie reaper (#904). agentd is PID 1 in the workspace
// container, so processes orphaned anywhere below it (opencode bash
// tool children whose intermediate parent died mid-execution, children
// of a previous opencode generation) are reparented to agentd and —
// because the Go runtime only reaps children its own os/exec calls are
// waiting on — accumulate as permanent zombies. 2026-08-16 evidence:
// two prod pods each carried [bash]/[go] defunct entries for hours.
//
// Design: discover candidates by scanning /proc for zombie children of
// this process, skip pids registered in the owned set (children with an
// os/exec waiter), and reap the rest with a pid-specific
// Wait4(WNOHANG) only after they have been zombie for longer than the
// grace period.
//
//   - Pid-specific Wait4 can never steal another child's exit status.
//   - The owned set covers every process agentd spawns itself (the
//     opencode supervisor and the trackedOutput helper); owned zombies
//     are never reaped regardless of age.
//   - The grace period is defense-in-depth for any future direct
//     exec.Command caller that waits immediately: a child with an
//     active blocking Wait is reaped by its waiter within microseconds
//     of exit, so a zombie that stays visible past grace has no
//     waiter by construction.
//
// A ticker backs the SIGCHLD wake-up because signals coalesce and
// because reparenting itself raises no signal — only the orphan's
// later exit does.

const (
	// orphanGrace is how long a zombie must stay visible before the
	// reaper considers it unowned. Orders of magnitude above the
	// microseconds an actively-waited child spends zombie; far below
	// the hours the bug let zombies accumulate.
	orphanGrace = 5 * time.Second
	// orphanScanInterval bounds reaping latency when SIGCHLD is lost.
	orphanScanInterval = 5 * time.Second
)

// orphanReaper reaps adopted zombie children of this process.
type orphanReaper struct {
	mu  sync.Mutex
	own map[int]struct{} // pids with a live os/exec waiter
	// pending tracks zombie pids first observed on the previous pass,
	// keyed by first-seen time; only reaped once older than grace.
	pending map[int]time.Time

	grace    time.Duration
	interval time.Duration
	// scan returns the current zombie-children pids, or an error when
	// the /proc walk itself failed. Indirect for tests.
	scan func() ([]int, error)

	workspaceID string
	metrics     *opsMetrics
}

// pkgOrphanReaper is the process-wide instance. Production code paths
// (the opencode supervisor, trackedOutput) register their children on
// it; main() runs its loop for the agentd lifetime.
var pkgOrphanReaper = newOrphanReaper()

func newOrphanReaper() *orphanReaper {
	return &orphanReaper{
		own:         make(map[int]struct{}),
		pending:     make(map[int]time.Time),
		grace:       orphanGrace,
		interval:    orphanScanInterval,
		scan:        scanZombieChildren,
		workspaceID: workspaceIDFromEnv(),
		metrics:     pkgOpsMetrics,
	}
}

// track registers a pid as owned by an os/exec waiter. The reaper will
// never Wait4 it. Call immediately after a successful cmd.Start().
func (r *orphanReaper) track(pid int) {
	if pid <= 0 {
		return
	}
	r.mu.Lock()
	r.own[pid] = struct{}{}
	delete(r.pending, pid)
	r.mu.Unlock()
}

// untrack releases a pid's ownership. Call after cmd.Wait() returns.
func (r *orphanReaper) untrack(pid int) {
	if pid <= 0 {
		return
	}
	r.mu.Lock()
	delete(r.own, pid)
	r.mu.Unlock()
}

// owns reports whether pid is currently registered as owned by an
// os/exec waiter.
func (r *orphanReaper) owns(pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.own[pid]
	return ok
}

// run reaps adopted orphans until ctx is done. One pass per SIGCHLD or
// ticker tick.
func (r *orphanReaper) run(ctx context.Context) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGCHLD)
	defer signal.Stop(sig)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
		case <-ticker.C:
		}
		r.pass()
	}
}

// pass scans for zombie children and reaps those past grace. A failed
// scan aborts the pass before any state mutation — a transient /proc
// failure must not wipe pending grace clocks (which would delay every
// in-flight reap by a fresh grace cycle) or prune owned/pending state.
func (r *orphanReaper) pass() {
	now := time.Now()
	zombies, err := r.scan()
	if err != nil {
		log.Warn("orphan reaper: zombie scan failed; keeping pending grace clocks", zap.Error(err))
		return
	}
	for _, pid := range zombies {
		r.mu.Lock()
		if _, owned := r.own[pid]; owned {
			delete(r.pending, pid)
			r.mu.Unlock()
			continue
		}
		first, seen := r.pending[pid]
		if !seen {
			r.pending[pid] = now
			r.mu.Unlock()
			continue
		}
		if now.Sub(first) < r.grace {
			r.mu.Unlock()
			continue
		}
		delete(r.pending, pid)
		r.mu.Unlock()

		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err != nil {
			// ECHILD: reaped elsewhere between scan and wait (no such
			// waiter exists today). Nothing to do either way.
			continue
		}
		if wpid == pid {
			log.Info("reaped orphaned child",
				zap.Int("pid", pid),
				zap.Int("exit_status", ws.ExitStatus()),
				zap.Bool("signaled", ws.Signaled()))
			r.metrics.RecordOrphanReap(r.workspaceID)
		}
		// wpid == 0: still alive despite the Z sighting (state raced);
		// the next pass re-adds it with a fresh grace clock.
	}

	// Prune pending entries for zombies no longer present: their waiter
	// (reappeared) reaped them, or the pid vanished. Without this, a
	// stale first-seen time would leak entries and — on pid reuse —
	// admit a fresh zombie past grace without aging.
	present := make(map[int]struct{}, len(zombies))
	for _, pid := range zombies {
		present[pid] = struct{}{}
	}
	r.mu.Lock()
	for pid := range r.pending {
		if _, still := present[pid]; !still {
			delete(r.pending, pid)
		}
	}
	r.mu.Unlock()
}

// scanZombieChildren returns the pids of this process's zombie
// children by walking /proc. Entries racing exit/reap are skipped.
// A /proc walk failure returns the error distinct from "no zombies"
// so the caller can preserve in-flight grace clocks.
func scanZombieChildren() ([]int, error) {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var zombies []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		state, ppid, ok := readProcStat(filepath.Join("/proc", e.Name(), "stat"))
		if !ok || ppid != self || state != 'Z' {
			continue
		}
		zombies = append(zombies, pid)
	}
	return zombies, nil
}

// readProcStat parses the state character and parent pid from a
// /proc/<pid>/stat file. comm (field 2) may contain spaces and parens,
// so parsing starts after the final ')'.
func readProcStat(path string) (state byte, ppid int, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	return parseProcStat(string(raw))
}

// parseProcStat parses the state character and parent pid from a
// /proc/<pid>/stat record. Parsing starts after the final ')' so a
// comm containing spaces or parens cannot shift the field indexes.
func parseProcStat(raw string) (state byte, ppid int, ok bool) {
	closeIdx := strings.LastIndexByte(raw, ')')
	if closeIdx < 0 || closeIdx+2 >= len(raw) {
		return 0, 0, false
	}
	fields := strings.Fields(raw[closeIdx+2:])
	if len(fields) < 2 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], ppid, true
}

// becomeSubreaper marks this process as a child subreaper so orphaned
// descendants reparent to it instead of the namespace init. Production
// agentd is PID 1 and already the reaper; this keeps the fix effective
// when agentd runs under another init (local runs, debugging). Best
// effort — failure is logged and survived.
func becomeSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		log.Warn("failed to set child subreaper; orphan reaping limited to direct children", zap.Error(err))
		return err
	}
	return nil
}

// trackedOutput runs cmd to completion with Output() semantics —
// stdout returned, stderr captured into ExitError.Stderr — while
// registering it with the orphan reaper so its exit is never stolen.
// Callers that need a command's output must use this instead of
// cmd.Output().
func trackedOutput(cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	pkgOrphanReaper.track(pid)
	err := cmd.Wait()
	pkgOrphanReaper.untrack(pid)
	if ee, ok := err.(*exec.ExitError); ok && stderr.Len() > 0 {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

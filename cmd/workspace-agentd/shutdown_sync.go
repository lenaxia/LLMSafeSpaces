// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// shutdown_sync.go — #1808 H1 belt: flush dirty pages on the
// platform's own shutdown paths.
//
// The zero-fill corruption class (#1808) enters below the file API:
// ext4 delayed allocation + an unclean stop can persist i_size while
// the data blocks never land — zero-fill at preserved length on the
// next mount. The suspend path (pod delete → SIGTERM → bounded grace
// → kubelet unmount → CSI detach) normally flushes at unmount, but the
// platform never proactively flushes dirty pages (including user
// tooling's buffered writes — sed -i, renderers, git) while it still
// controls the window. This belt triggers a filesystem-wide writeback
// flush at the last moment we are guaranteed alive, bounded so a
// pathological volume can never hang shutdown past its budget: the
// kernel completes a triggered writeback asynchronously even after the
// triggering process exits.

import (
	"syscall"
	"time"
)

// flushSync is the seam (production: syscall.Sync — filesystem-wide
// writeback trigger; tests override). A var, not a call, so the
// shutdown wiring is pinnable.
var flushSync = syscall.Sync

// shutdownFlushWait bounds how long shutdown WAITS on the flush. The
// flush itself, once triggered, completes in the kernel regardless.
var shutdownFlushWait = 10 * time.Second

// flushWorkspaceWrites triggers the writeback flush and waits bounded.
// Returns true when the flush returned within the bound. The 40s pod
// termination grace (and the supervisor's own shutdown budget) owns
// the outer bound; this wait is the inner one.
func flushWorkspaceWrites() bool {
	done := make(chan struct{})
	go func() {
		flushSync()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(shutdownFlushWait):
		return false
	}
}

// flushWorkspaceWritesLogged is the shutdown-path wrapper: loud on the
// pathological (unbounded flush abandoned) case, silent on the healthy
// one — a clean shutdown should not grow log noise for a belt.
func flushWorkspaceWritesLogged() {
	if !flushWorkspaceWrites() {
		log.Warn("workspace-agentd: writeback flush did not complete within the shutdown bound; kernel writeback continues async")
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// #1744 safety: never delete anything with an open file handle. The
// whole-dir classes added in #1744 (playwright browsers, session
// gocache, npm payloads, envtest assets) are re-downloadable — but
// reaping one out from under the process using it breaks a LIVE lane
// (the exact regression class the issue's ENOSPC evidence documents).
//
// openHandles is one sweep's snapshot of the paths this pod's
// same-uid processes hold open, collected from:
//
//   - /proc/<pid>/fd  — every open file descriptor (readlink target)
//   - /proc/<pid>/exe — executed binaries (a running playwright browser
//     holds its chromium via the exe link, not necessarily an fd)
//   - /proc/<pid>/cwd — processes sitting inside a candidate tree
//
// mmap-only holders are a documented gap (no /proc/*/maps scan): every
// known mmap consumer in these cache classes also holds the binary via
// exe or the data via fd, so the gap is empty in practice.
//
// Policy for what cannot be read:
//
//   - a vanished pid (ENOENT between the /proc readdir and the fd
//     read) holds nothing — skipped silently;
//   - a FOREIGN-uid pid's table is skipped by policy: only the sandbox
//     user's processes can hold these user-owned caches open (verified
//     on the incident pod: 7/7 pid tables readable, all one uid — the
//     workspace container has no foreign-uid processes);
//   - a same-uid table that stays PERMISSION-DENIED is also skipped by
//     policy. Two unreachable-by-design classes exist (both probed
//     empirically, #1744 worklog): (a) a pid caught mid-execve is
//     transiently non-dumpable — microseconds, self-resolving; (b) a
//     non-dumpable session helper such as systemd's (sd-pam) — alive
//     for the host's whole session, persistent, and structurally
//     unreadable to same-uid readers. Neither class exists inside the
//     workspace container's pid namespace, where the engine actually
//     runs (single process tree, all dumpable, one uid — verified);
//     on shared CI runners they can never hold a test's temp dirs.
//     The residual gap — a future in-pod dumpable=0 helper holding a
//     cache — is accepted and documented here, not silently ignored;
//   - /proc ITSELF being unreadable fails the scan CLOSED: every
//     candidate is then treated as held (in_use). An unmeasurable
//     process set must never turn into an unguarded sweep.
type openHandles struct {
	paths     map[string]struct{}
	available bool // /proc was enumerable at all
}

// scanOpenHandles walks /proc once and snapshots the open-path set.
func scanOpenHandles() openHandles {
	oh := openHandles{paths: map[string]struct{}{}}
	dirents, err := os.ReadDir("/proc")
	if err != nil {
		return oh // available=false → blocked() → fail closed
	}
	oh.available = true
	for _, d := range dirents {
		if !d.IsDir() || !allDigits(d.Name()) {
			continue
		}
		procDir := filepath.Join("/proc", d.Name())
		fds, ferr := os.ReadDir(filepath.Join(procDir, "fd"))
		if ferr != nil {
			if errors.Is(ferr, fs.ErrNotExist) {
				// vanished mid-scan — holds nothing.
				continue
			}
			// Permission-denied: a foreign-uid table, or a same-uid
			// table that is structurally unreadable (mid-execve
			// transient; non-dumpable session helpers like (sd-pam)).
			// Skipped by policy — see the note above for both the
			// evidence and the accepted residual gap.
			continue
		}
		for _, fd := range fds {
			collectLinkTarget(oh.paths, filepath.Join(procDir, "fd", fd.Name()))
		}
		collectLinkTarget(oh.paths, filepath.Join(procDir, "exe"))
		collectLinkTarget(oh.paths, filepath.Join(procDir, "cwd"))
	}
	return oh
}

// collectLinkTarget resolves one /proc link into the open-path set.
// Non-filesystem targets (pipe:, socket:, anon_inode:) are skipped.
// Targets carrying the " (deleted)" marker stay in the set: the inode
// is still held by a live process — over-holding is the safe direction.
func collectLinkTarget(paths map[string]struct{}, link string) {
	t, err := os.Readlink(link)
	if err != nil || !strings.HasPrefix(t, "/") {
		return
	}
	paths[t] = struct{}{}
}

// blocked reports whether the snapshot is NOT a trustworthy "no open
// handles" proof. Blocked snapshots hold every candidate.
func (oh openHandles) blocked() bool {
	return !oh.available
}

// holds reports whether any open path is path itself or inside it.
// A blocked snapshot holds everything (fail closed).
func (oh openHandles) holds(path string) bool {
	if oh.blocked() {
		return true
	}
	for p := range oh.paths {
		if p == path || strings.HasPrefix(p, path+"/") {
			return true
		}
	}
	return false
}

// holdReason explains a blocked snapshot (the in_use reason string).
func (oh openHandles) holdReason() string {
	return "open-handle scan unavailable (/proc not readable) — treated as held"
}

// allDigits reports whether s is a non-empty string of ASCII digits
// (the numeric-pid filter for /proc dirents).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

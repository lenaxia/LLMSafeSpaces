// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
//     workspace container has no foreign-uid processes), and skipping
//     them keeps the scan complete on shared CI runners where root
//     system daemons are unreadable by design;
//   - a SAME-UID table that cannot be read is counted unreadable and
//     FAILS THE SCAN CLOSED: every candidate is then treated as held
//     (in_use) — an unmeasurable holder set must never turn into an
//     unguarded sweep. Same for /proc itself being unreadable.
type openHandles struct {
	paths      map[string]struct{}
	available  bool // /proc was enumerable at all
	unreadable int  // same-uid fd tables that could not be read
}

// scanOpenHandles walks /proc once and snapshots the open-path set.
func scanOpenHandles() openHandles {
	oh := openHandles{paths: map[string]struct{}{}}
	dirents, err := os.ReadDir("/proc")
	if err != nil {
		return oh // available=false → blocked() → fail closed
	}
	oh.available = true
	uidNum := os.Getuid()
	if uidNum < 0 {
		// Unreachable where /proc exists (POSIX getuid is >= 0); on
		// platforms without one the ReadDir above already failed
		// closed. Guarded conversion per gosec G115.
		return oh
	}
	uid := uint32(uidNum) //nolint:gosec // G115: guarded by the uidNum < 0 return above — getuid() >= 0 on every POSIX host
	for _, d := range dirents {
		if !d.IsDir() || !allDigits(d.Name()) {
			continue
		}
		procDir := filepath.Join("/proc", d.Name())
		own := pidOwnedBy(procDir, uid)
		fds, ferr := os.ReadDir(filepath.Join(procDir, "fd"))
		if ferr != nil {
			switch {
			case errors.Is(ferr, fs.ErrNotExist):
				// vanished mid-scan — holds nothing
			case own:
				oh.unreadable++ // same-uid blind spot → fail closed
			default:
				// foreign-uid table: skipped by policy (see above)
			}
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
	return !oh.available || oh.unreadable > 0
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
	if !oh.available {
		return "open-handle scan unavailable (/proc not readable) — treated as held"
	}
	return fmt.Sprintf("open-handle scan incomplete (%d same-uid pid table(s) unreadable) — treated as held", oh.unreadable)
}

// pidOwnedBy reports whether the /proc/<pid> directory belongs to uid
// (the directory's owner IS the process uid). Unstat-able pids are
// treated as foreign (skipped by policy).
func pidOwnedBy(procDir string, uid uint32) bool {
	fi, err := os.Stat(procDir)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Uid == uid
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

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// TEMPORARY CI diagnostic (#1744): dumps the /proc identity of every
// same-uid pid whose fd table is unreadable, from inside a test child
// on the CI runner. Removed once the failing mechanism is identified.
package diskrecovery

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestCIProcProbe(t *testing.T) {
	self := os.Getuid()
	dirents, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("proc readdir: %v", err)
	}
	var unreadable int
	for _, d := range dirents {
		if !d.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(d.Name()); err != nil {
			continue
		}
		p := filepath.Join("/proc", d.Name())
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(self) {
			continue
		}
		if _, err := os.ReadDir(filepath.Join(p, "fd")); err == nil {
			continue
		}
		unreadable++
		t.Logf("UNREADABLE pid=%s mode=%v", d.Name(), fi.Mode())
		for _, f := range []string{"comm", "status", "stat", "cmdline"} {
			if b, err := os.ReadFile(filepath.Join(p, f)); err == nil {
				t.Logf("  %s=%q", f, strings.TrimSpace(string(b)))
			} else {
				t.Logf("  %s unreadable: %v", f, err)
			}
		}
		exe, err := os.Readlink(filepath.Join(p, "exe"))
		t.Logf("  exe=%q err=%v", exe, err)
		cwd, err := os.Readlink(filepath.Join(p, "cwd"))
		t.Logf("  cwd=%q err=%v", cwd, err)
	}
	t.Logf("self pid=%d uid=%d same-uid-unreadable=%d", os.Getpid(), self, unreadable)
	if b, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope"); err == nil {
		t.Logf("yama ptrace_scope=%q", strings.TrimSpace(string(b)))
	} else {
		t.Logf("yama ptrace_scope unreadable: %v", err)
	}
	// The probe FAILS when it found offenders so CI prints the logs
	// above (go test only shows output for failing tests without -v).
	if unreadable > 0 {
		t.Fatalf("probe found %d unreadable same-uid table(s) — see logs", unreadable)
	}
}

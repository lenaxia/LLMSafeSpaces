// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package diskrecovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// prSetDumpable is PR_SET_DUMPABLE (prctl): clearing dumpability makes
// the process's /proc/<pid>/{fd,exe,cwd} unreadable to same-uid
// readers — the exact structural property of the runner's (sd-pam)
// helper and of pids caught mid-execve (#1744 CI r2 finding).
const prSetDumpable = 4

// PIN (skip-by-policy, #1744 CI r2): a same-uid process whose fd table
// is structurally unreadable (non-dumpable) must be SKIPPED by the
// scan — the scan stays complete (not blocked), and the process's
// held paths are invisible by the documented policy. This is the
// regression pin for the (sd-pam) class that bricked both Test legs
// when unreadable tables failed the scan closed.
func TestUnreadableTablesSkippedByPolicy(t *testing.T) {
	if os.Getenv("DISKRECOVERY_NONDUMPABLE_CHILD") == "1" {
		// Child: become non-dumpable, hold a file open, and park until
		// the parent reaps us.
		if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
			os.Exit(3)
		}
		f, err := os.Open(os.Getenv("DISKRECOVERY_CHILD_HOLD"))
		if err != nil {
			os.Exit(4)
		}
		defer f.Close() //nolint:errcheck // test-helper exit path
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}

	dir := t.TempDir()
	hold := filepath.Join(dir, "held.dat")
	if err := os.WriteFile(hold, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestUnreadableTablesSkippedByPolicy", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		"DISKRECOVERY_NONDUMPABLE_CHILD=1",
		"DISKRECOVERY_CHILD_HOLD="+hold)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait until the child is non-dumpable and holding: its fd table
	// must read EACCES from here (that IS the condition under test).
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "fd"))
		if err == nil {
			// still readable — the child is not non-dumpable yet
		} else if strings.Contains(err.Error(), "permission denied") {
			break // the condition under test
		} else {
			t.Fatalf("child vanished before becoming non-dumpable: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became non-dumpable (fd table still readable)")
		}
		time.Sleep(50 * time.Millisecond)
	}

	oh := scanOpenHandles()
	if !oh.available {
		t.Fatal("/proc enumerated fine — scan must be available")
	}
	// The policy pin: the structurally-unreadable table is SKIPPED, not
	// a fail-closed trigger — the (sd-pam) regression.
	if oh.blocked() {
		t.Fatal("a non-dumpable same-uid table must be skipped by policy, not block the scan")
	}
	// The documented residual gap, pinned as policy: the invisible
	// holder's paths do not hold candidates (they cannot be read).
	if oh.holds(dir) {
		t.Fatalf("non-dumpable holder's paths must be invisible (skip policy); got a hold on %s", dir)
	}
}

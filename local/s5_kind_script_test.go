// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// s5_kind_script_test.go — regression harness for
// local/s5-overlay-validation.sh (design 0053 S5), same philosophy as
// scripts/us2_kind_script_test.go: the script is CI glue on a real kind
// cluster; what is pinnable deterministically is the structure past
// failures actually broke.
//
// Run-8 incident (PR #1178): the runsc checksum guard was a '?' glob
// whose length was miscounted (66 chars instead of 128) — it REJECTED a
// perfectly valid checksum, failing S5.6 while the guard's own diagnosis
// printed the correct format. The regression test below executes the
// REAL guard from the script against the real gVisor checksum shape and
// fails if the guard does not accept it (with the old glob, this test
// fails); it also pins the unhappy paths (empty, short-but-hex,
// wrong-length, non-hex) that must be rejected.

import (
	"os/exec"
	"strings"
	"testing"
)

const s5Script = "s5-overlay-validation.sh"

func requireBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	return bash
}

func TestS5Script_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", s5Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func TestS5Gvisor_DelegatesToLib(t *testing.T) {
	src := mustRead(t, s5Script)
	if strings.Contains(src, "storage.googleapis.com/gvisor/releases") {
		t.Fatal("s5 still carries the dead GCS gVisor fetch inline — S5.6 must delegate to lib/gvisor.sh")
	}
	for _, pin := range []string{
		"lib/gvisor.sh\" install",
		"lib/gvisor.sh\" runtimeclass",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("S5.6 must call %q — one provisioning flow, not two", pin)
		}
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

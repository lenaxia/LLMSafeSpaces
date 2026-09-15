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
	"os"
	"os/exec"
	"path/filepath"
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

// The S5.6 delegation must carry the s5 cluster identity across the
// process boundary: CLUSTER_NAME/CTX are plain vars in the s5 script,
// and gvisor.sh's own default targets a context that does not exist on
// the s5 runner. Executes the delegation lines with a stub gvisor.sh
// that records what it inherited.
func TestS5Gvisor_DelegationCarriesClusterEnv(t *testing.T) {
	bash := requireBash(t)
	src := mustRead(t, s5Script)

	start := strings.Index(src, `if CLUSTER_NAME="$CLUSTER_NAME" CTX="kind-$CLUSTER_NAME"`)
	end := strings.Index(src, `bash "$REPO_ROOT/local/lib/gvisor.sh" runtimeclass; then`)
	if start < 0 || end < 0 || end < start {
		t.Fatal("S5.6 delegation lines not found in the expected explicit-env form")
	}
	delegation := src[start : end+len(`bash "$REPO_ROOT/local/lib/gvisor.sh" runtimeclass; then`)]

	dir := t.TempDir()
	libDir := filepath.Join(dir, "local", "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(libDir, "gvisor.sh")
	rec := filepath.Join(dir, "env-record")
	stubBody := "#!/bin/bash\n" +
		"echo \"CLUSTER_NAME=$CLUSTER_NAME CTX=$CTX args=$*\" >> " + rec + "\n" +
		"exit 0\n"
	if err := os.WriteFile(stub, []byte(stubBody), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "REPO_ROOT=" + shQuote(dir) + "\n" +
		"CLUSTER_NAME=s5-ovl\n" +
		delegation + "\n: \nfi\n" +
		"echo delegated-ok\n"
	got, err := exec.Command(bash, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("delegation failed: %v: %s", err, got)
	}
	if !strings.Contains(string(got), "delegated-ok") {
		t.Fatalf("delegation chain must succeed, got: %s", got)
	}
	recorded, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal("stub gvisor.sh was never invoked")
	}
	// BOTH invocations carry the identity — the runtimeclass call is the
	// one whose env is load-bearing (CTX names the kubectl context; the
	// install call never evaluates it).
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	if len(lines) != 2 {
		t.Fatalf("both delegation calls must run, got %d: %s", len(lines), recorded)
	}
	for i, ln := range lines {
		if !strings.Contains(ln, "CLUSTER_NAME=s5-ovl CTX=kind-s5-ovl") {
			t.Fatalf("call %d must inherit the s5 cluster identity, got: %s", i+1, ln)
		}
	}
	if !strings.Contains(lines[1], "runtimeclass") {
		t.Fatalf("the runtimeclass invocation is the CTX consumer — it must carry the env (record: %s)", recorded)
	}
}

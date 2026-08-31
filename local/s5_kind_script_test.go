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

// extractGuard pulls the checksum-validation block from the script. The
// guard is located by its unique markers and re-exposed as a function of
// $EXPECTED so the test exercises the script's OWN regex, not a copy.
func extractGuard(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("s5-overlay-validation.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	src := string(raw)
	const marker = `[[ "$EXPECTED" =~ ^[0-9a-f]{128}$ ]]`
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("checksum guard not found in script — it must keep the regex form")
	}
	return marker
}

func TestS5Script_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", s5Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func TestS5Script_RunscChecksumGuardAcceptsRealShape(t *testing.T) {
	bash := requireBash(t)
	guard := extractGuard(t)

	// The real gVisor publication shape: 128 lowercase hex + "  runsc"
	// (captured from run 8's diagnosis output).
	const realChecksumFile = "84936438d583ec976800f464e75a83e1515f0890b451b9b4db219c4472b54ca9b106a6772ee683f1e64cce2128871d7637b14d800591f8451b8137f6c39fb2ef  runsc"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runsc.sha512"), []byte(realChecksumFile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "cd " + dir + "\nEXPECTED=$(cut -d\" \" -f1 runsc.sha512)\n" + guard + " || exit 1\nexit 0\n"
	out, err := exec.Command(bash, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("guard REJECTED a valid 128-hex checksum (the run-8 regression): %s", out)
	}
}

func TestS5Script_RunscChecksumGuardRejectsMalformed(t *testing.T) {
	bash := requireBash(t)
	guard := extractGuard(t)

	cases := map[string]string{
		"empty":            "",
		"short-but-hex":    strings.Repeat("a", 66), // the miscounted-glob length itself
		"too-long-hex":     strings.Repeat("a", 129),
		"non-hex-128chars": strings.Repeat("z", 128),
	}
	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			script := "EXPECTED=" + shQuote(expected) + "\n" + guard + " && exit 1\nexit 0\n"
			out, err := exec.Command(bash, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("harness failed: %s", out)
			}
			// exit 0 == the guard rejected (the && branch did not run)
		})
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

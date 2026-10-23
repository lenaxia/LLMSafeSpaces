// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1305's integration legs, automated: deleting either check from
// cmd/repolint/main.go, breaking its registration plumbing, or dropping
// the CI step must fail HERE — the package tests alone cannot detect
// silent deregistration (the exact gap the r1 review flagged).

// TestRepolintMain_RegistersNewRules statically pins the wiring: the
// binary's main() must invoke both new checks. A deregistered rule
// passes every package test but fails this pin.
func TestRepolintMain_RegistersNewRules(t *testing.T) {
	src, err := os.ReadFile(repoRoot(t) + "/cmd/repolint/main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{
		"failures += runAgentIDPrefix(root)",
		"failures += runSpecCouplingMarker(root)",
		"func runAgentIDPrefix(root string) int {",
		"func runSpecCouplingMarker(root string) int {",
		"repolint.AgentIDPrefixCheck(root)",
		"repolint.SpecCouplingMarkerCheck(root)",
	} {
		if !strings.Contains(string(src), pin) {
			t.Errorf("cmd/repolint/main.go lost its wiring: %q not found — the rule would be silently disabled", pin)
		}
	}
}

// TestCIWorkflow_RunsRepolintLint pins the CI invocation: the lint job
// runs `make repolint`, which builds and runs ./bin/repolint with no
// per-rule flags — registration in main() IS the wiring.
func TestCIWorkflow_RunsRepolintLint(t *testing.T) {
	root := repoRoot(t)
	for _, wf := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		src, err := os.ReadFile(filepath.Join(root, wf))
		if err != nil {
			t.Fatalf("%s: %v", wf, err)
		}
		if !strings.Contains(string(src), "make repolint") {
			t.Errorf("%s no longer runs `make repolint` — the new rules would never run in CI", wf)
		}
	}
}

// TestRepolintBinary_InvokesNewRules is the end-to-end leg: build the
// real binary, run it against a deliberately-violating scratch tree
// (exit non-zero, file:line attribution for BOTH rules) and against
// the repository itself (exit 0, both ok-lines). Skipped under -short
// (it shells out to the go toolchain).
func TestRepolintBinary_InvokesNewRules(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build smoke test skipped under -short")
	}
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "repolint")
	build := exec.Command("go", "build", "-o", bin, "./cmd/repolint")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/repolint: %v\n%s", err, out)
	}

	scratch := t.TempDir()
	writeGoFile(t, scratch, "api/internal/handlers/leaky.go", `package handlers

import "strings"

func dispatch(id string) bool { return strings.HasPrefix(id, "que_") }
`)
	writeSpecFile(t, scratch, "openapi: 3.0.3\ninfo:\n  title: T\n  version: \"1\"\npaths:\n  /x:\n    get:\n      responses:\n        \"200\":\n          description: tracks upstream opencode.\n          x-opencode-proxy: true\n")

	out, err := exec.Command(bin, "-repo", scratch).CombinedOutput()
	if err == nil {
		t.Fatalf("violating scratch tree must exit non-zero; got 0\n%s", out)
	}
	scratchOut := string(out)
	for _, want := range []string{
		"agent ID prefixes",
		"api/internal/handlers/leaky.go:",
		"spec coupling markers",
		"sdks/openapi.yaml:",
		"x-opencode-proxy",
	} {
		if !strings.Contains(scratchOut, want) {
			t.Errorf("binary output against the scratch tree must contain %q; got:\n%s", want, scratchOut)
		}
	}

	out, err = exec.Command(bin, "-repo", root).CombinedOutput()
	if err != nil {
		t.Fatalf("real tree must pass clean; got %v\n%s", err, out)
	}
	realOut := string(out)
	for _, want := range []string{"ok    agent ID prefixes", "ok    spec coupling markers"} {
		if !strings.Contains(realOut, want) {
			t.Errorf("binary output against the real tree must contain %q (registration proof); got:\n%s", want, realOut)
		}
	}
}

// TestRepolintBinary_NumberingModes pins the two #1838 CLI modes at the
// binary level — the numbering-gate workflow invokes `-numbering-only`
// autonomously against merge trees and consumes its exit code for the
// redden/clear decision, and `-next-numbers` is the authors' soft
// lever. A broken flag wiring (or a mode that exits 0 on a colliding
// tree) passes every package test but fails here — the #1305 class.
func TestRepolintBinary_NumberingModes(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build smoke test skipped under -short")
	}
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "repolint")
	build := exec.Command("go", "build", "-o", bin, "./cmd/repolint")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/repolint: %v\n%s", err, out)
	}

	newScratch := func() string {
		scratch := t.TempDir()
		wl := filepath.Join(scratch, "worklogs")
		mig := filepath.Join(scratch, "api", "migrations")
		if err := os.MkdirAll(wl, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(mig, 0o755); err != nil {
			t.Fatal(err)
		}
		// A clean migration pair on every scratch so a missing-dir
		// error can never satisfy the exit-code assertions below.
		mustWrite(t, filepath.Join(mig, "000001_init.up.sql"), "")
		mustWrite(t, filepath.Join(mig, "000001_init.down.sql"), "")
		return scratch
	}

	// Colliding tree: the 1181x2 shape (both files above the
	// grandfather threshold). `-numbering-only` must exit non-zero and
	// name BOTH files — the exact output the gate embeds in its red
	// check runs.
	colliding := newScratch()
	mustWrite(t, filepath.Join(colliding, "worklogs", "1181_2026-10-22_lane-alpha.md"), "")
	mustWrite(t, filepath.Join(colliding, "worklogs", "1181_2026-10-22_lane-beta.md"), "")
	out, err := exec.Command(bin, "-repo", colliding, "-numbering-only").CombinedOutput()
	if err == nil {
		t.Fatalf("-numbering-only on a colliding tree must exit non-zero; got 0\n%s", out)
	}
	for _, want := range []string{
		"FAIL  worklogs uniqueness",
		"1181_2026-10-22_lane-alpha.md",
		"1181_2026-10-22_lane-beta.md",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("-numbering-only output must contain %q; got:\n%s", want, out)
		}
	}

	// Clean tree: exit 0 with the pass line.
	clean := newScratch()
	mustWrite(t, filepath.Join(clean, "worklogs", "1181_2026-10-22_lane-alpha.md"), "")
	mustWrite(t, filepath.Join(clean, "api", "migrations", "000002_next.up.sql"), "")
	mustWrite(t, filepath.Join(clean, "api", "migrations", "000002_next.down.sql"), "")
	out, err = exec.Command(bin, "-repo", clean, "-numbering-only").CombinedOutput()
	if err != nil {
		t.Fatalf("-numbering-only on a clean tree must exit 0; got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "numbering checks passed") {
		t.Errorf("-numbering-only clean-tree output must contain the pass line; got:\n%s", out)
	}

	// -next-numbers: both lines, correct values for the clean scratch
	// (worklog max 1181 → 1182; migration max 2 → 3). The scratch is
	// not a git repo, so mainline/claims discovery contributes nothing —
	// the output is deterministic.
	out, err = exec.Command(bin, "-repo", clean, "-next-numbers").CombinedOutput()
	if err != nil {
		t.Fatalf("-next-numbers must exit 0; got %v\n%s", err, out)
	}
	for _, want := range []string{
		"next worklog number:   1182",
		"next migration number: 3",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("-next-numbers output must contain %q; got:\n%s", want, out)
		}
	}
}

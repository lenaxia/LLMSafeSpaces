// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaLintFreezeGate proves the schema-governance gate itself (issue
// #1135 test plan: schema_lint_freeze_gate): buf lint passes on the live
// module, and buf breaking DETECTS a breaking change against a frozen
// baseline — demonstrated on fixtures so the gate is proven before the real
// S2 freeze arms it. The repo-level freeze marker (abi/FROZEN, recorded by
// US-69.8) switches the CI gate from lint-only to lint+breaking against the
// recorded baseline ref.
func TestSchemaLintFreezeGate(t *testing.T) {
	buf, err := exec.LookPath("buf")
	if err != nil {
		t.Skipf("buf not on PATH — install via `make tools-install`; CI runs this test with buf installed")
	}
	repoRoot := findRepoRoot(t)

	t.Run("lint_live_module", func(t *testing.T) {
		cmd := exec.Command(buf, "lint")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("buf lint failed on the live ABI module:\n%s", out)
		}
	})

	t.Run("breaking_change_detected", func(t *testing.T) {
		frozen := filepath.Join(repoRoot, "abi", "testdata", "frozen")
		if _, err := os.Stat(frozen); err != nil {
			t.Fatalf("frozen fixture missing: %v", err)
		}
		broken := t.TempDir()
		if err := copyTree(frozen, broken); err != nil {
			t.Fatalf("copy fixture: %v", err)
		}
		mutateBreaking(t, filepath.Join(broken, "probe.proto"))

		cmd := exec.Command(buf, "breaking", ".", "--against", frozen)
		cmd.Dir = broken
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("buf breaking ACCEPTED a field deletion against the frozen baseline — the gate does not gate:\n%s", out)
		}
		if !strings.Contains(string(out), "probe") {
			t.Errorf("breaking output does not reference the mutated message:\n%s", out)
		}
	})

	t.Run("breaking_clean_copy_passes", func(t *testing.T) {
		frozen := filepath.Join(repoRoot, "abi", "testdata", "frozen")
		cmd := exec.Command(buf, "breaking", ".", "--against", frozen)
		cmd.Dir = frozen
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("buf breaking rejected an unchanged schema:\n%s", out)
		}
	})

	t.Run("freeze_not_yet_armed", func(t *testing.T) {
		marker := filepath.Join(repoRoot, "abi", "FROZEN")
		if _, err := os.Stat(marker); err == nil {
			t.Log("abi/FROZEN exists — S2 freeze armed; breaking gate must run against the recorded ref in CI")
		} else {
			t.Log("abi/FROZEN absent — S1 evolution window; CI gate is lint-only by design (D5)")
		}
	})
}

// mutateBreaking simulates a post-freeze violation: deleting a field from a
// frozen message (FILE breaking rules catch field deletion).
func mutateBreaking(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	var kept []string
	for _, l := range lines {
		if strings.Contains(l, "int64 seq = 2") {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) == len(lines) {
		t.Fatalf("mutation target field not found in fixture %s", path)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root (go.mod) not found")
	return ""
}

func copyTree(src, dst string) error {
	return exec.Command("cp", "-a", src+"/.", dst).Run()
}

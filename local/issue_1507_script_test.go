// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/issue-1507-bounded-suspend-e2e.sh — the kind-level
// busy-suspend e2e (#1507 AC4's cluster row; the fake-client pins live
// in controller/internal/workspace/phase_suspend_1507_test.go). The
// script runs on the harness/pool cluster; these pins hold its shape
// (the TestUS70Scripts_BashSyntax / s5 pin-test precedents).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const issue1507Script = "issue-1507-bounded-suspend-e2e.sh"

func requireBashLocal(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	return bash
}

func TestIssue1507Script_BashSyntax(t *testing.T) {
	bash := requireBashLocal(t)
	out, err := exec.Command(bash, "-n", issue1507Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func mustRead1507(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(issue1507Script)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The incident row must exercise the REAL busy state (a running tool
// part in the session history) before suspending — a suspend without a
// proven-busy session tests nothing about the removed gate.
func TestIssue1507Script_AssertsBusyBeforeSuspend(t *testing.T) {
	src := mustRead1507(t)
	for _, marker := range []string{
		`running_tool_parts`,
		`R1: tool part running`,
		`workspaces/${WS}/suspend`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must contain %q (the busy-state proof or the suspend trigger)", marker)
		}
	}
	// The busy wait must PRECEDE the suspend call.
	busyIdx := strings.Index(src, `R1: tool part running`)
	suspendIdx := strings.Index(src, `workspaces/${WS}/suspend`)
	if busyIdx < 0 || suspendIdx < 0 || busyIdx > suspendIdx {
		t.Error("the busy-session wait must come before the suspend call — otherwise the row cannot prove busy-suspend")
	}
}

// The AC1 log assertion (no drain-defer line for suspend) and the
// outcome assertions (Suspended phase, pod deleted, PVC retained) are
// the row's verdicts — the script is dead weight without them.
func TestIssue1507Script_OutcomeAssertions(t *testing.T) {
	src := mustRead1507(t)
	for _, marker := range []string{
		`wait_phase "${WS}" Suspended`,
		`deferring pod deletion behind busy sessions`,
		`PVC ${PVC} retained`,
		`R1: pod object gone`,
		`still exists past the grace window`,
		`pod_gone`,
		`grep -q "NotFound"`,
		`R2: controller log fetch failed`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must assert %q — a #1507 row without this verdict is decorative", marker)
		}
	}
}

// Per-script workspace isolation: the WS_BASE sentinel must be set
// UNCONDITIONALLY (the lib's own default must never silently apply —
// the #1342 note).
func TestIssue1507Script_WorkspaceIsolation(t *testing.T) {
	src := mustRead1507(t)
	if !strings.Contains(src, `WS_BASE="e2e15070-`) {
		t.Error("script must set its own WS_BASE unconditionally (per-script isolation)")
	}
}

// The pod-gone predicate is pipefail-sensitive: kc exits 1 on
// NotFound, and a naive pipeline returns kc's status, not grep's. This
// pin executes the predicate's EXACT source form (extracted from the
// script, not copied) against a mock kc across the three cases — the
// r7 bug class the string pins structurally cannot catch.
func TestIssue1507Script_PodGonePredicateMockTable(t *testing.T) {
	src := mustRead1507(t)
	const marker = `pod_gone() {`
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("pod_gone function not found in the script")
	}
	// Extract the function body up to the closing brace at column 4.
	closer := "\n    }"
	end := strings.Index(src[i:], closer)
	if end < 0 {
		t.Fatal("pod_gone body end not found")
	}
	fn := src[i : i+end+len(closer)]

	cases := []struct {
		name string
		mock string
		want bool
	}{
		{"pod exists → not gone", `#!/usr/bin/env bash
printf "NAME ws-test
"
`, false},
		{"NotFound → gone", `#!/usr/bin/env bash
echo "Error from server (NotFound): pods "ws-test" not found" >&2
exit 1
`, true},
		{"query error → not gone (fail-closed)", `#!/usr/bin/env bash
echo "connection refused" >&2
exit 1
`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "kc"), []byte(tc.mock), 0o755); err != nil {
				t.Fatal(err)
			}
			script := "set -euo pipefail\nPOD=\"ws-test\"\nexport PATH=\"" + dir + ":$PATH\"\n" + fn +
				"\nif pod_gone; then echo GONE; else echo NOTGONE; fi\n"
			out, err := exec.Command("bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("predicate run failed: %v: %s", err, out)
			}
			got := strings.TrimSpace(string(out)) == "GONE"
			if got != tc.want {
				t.Errorf("pod_gone = %v, want %v (output: %q)", got, tc.want, string(out))
			}
		})
	}
}

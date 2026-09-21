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
		`pod ${POD} still exists after Suspended`,
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

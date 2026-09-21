// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/issue-1505-refresh-busy-e2e.sh — the kind-level
// busy-refresh e2e (#1505's cluster row; the fake-client pins live in
// controller/internal/workspace/force_recycle_test.go). The script runs
// on the harness/pool cluster (first recorded execution rides the
// #1456 wiring lane — the documented follow-up, same disposition as the
// #1507 row); these pins hold its shape (the TestUS70Scripts_BashSyntax
// / s5 / 1507 pin-test precedents).

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const issue1505Script = "issue-1505-refresh-busy-e2e.sh"

func mustRead1505(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(issue1505Script)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIssue1505Script_BashSyntax(t *testing.T) {
	bash := requireBashLocal(t)
	out, err := exec.Command(bash, "-n", issue1505Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The repro row must exercise the REAL busy state (a running tool part
// in the session history) before refreshing — a refresh without a
// proven-busy session tests nothing about the bypassed drain.
func TestIssue1505Script_AssertsBusyBeforeRefresh(t *testing.T) {
	src := mustRead1505(t)
	for _, marker := range []string{
		`running_tool_parts`,
		`R1: tool part running`,
		`workspaces/${WS}/refresh-compute`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must contain %q (the busy-state proof or the refresh trigger)", marker)
		}
	}
	// The busy wait must PRECEDE the refresh call.
	busyIdx := strings.Index(src, `R1: tool part running`)
	refreshIdx := strings.Index(src, `workspaces/${WS}/refresh-compute`)
	if busyIdx < 0 || refreshIdx < 0 || busyIdx > refreshIdx {
		t.Error("the busy-session wait must come before the refresh call — otherwise the row cannot prove busy-refresh")
	}
}

// The outcome assertions (back to Active within the budget, a NEW pod
// — the recycle really happened —, PVC retained, and the fail-closed
// audit gate with BOTH the negative drain-defer check and the POSITIVE
// forced-bypass check) are the row's verdicts.
func TestIssue1505Script_OutcomeAssertions(t *testing.T) {
	src := mustRead1505(t)
	for _, marker := range []string{
		`wait_phase "${WS}" Active`,
		`restartCount ${OLD_RC} → ${NEW_RC}`,
		`restartCount did not bump`,
		`PVC ${PVC} retained`,
		`deferring pod deletion behind busy sessions.*"reason": "restart_generation"`,
		`bypassing session drain.*restart_generation_user_forced`,
		`R2: controller log fetch failed`,
		`R2: controller pod not found`,
		`fail-closed`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must assert %q — a #1505 row without this verdict is decorative", marker)
		}
	}
}

// Per-script workspace isolation: the WS_BASE sentinel must be set
// UNCONDITIONALLY (the lib's own default must never silently apply —
// the #1342 note).
func TestIssue1505Script_WorkspaceIsolation(t *testing.T) {
	src := mustRead1505(t)
	if !strings.Contains(src, `WS_BASE="e2e15050-`) {
		t.Error("script must set its own WS_BASE unconditionally (per-script isolation)")
	}
}

// The reason strings in the R2 greps must match the controller's drain
// constants exactly — a drifted reason label makes the audit gate grep
// the wrong line (vacuously green on the negative check).
func TestIssue1505Script_ReasonStringsMatchController(t *testing.T) {
	src := mustRead1505(t)
	drainSrc, err := os.ReadFile("../controller/internal/workspace/session_drain.go")
	if err != nil {
		t.Skip("controller source not present in this checkout layout")
	}
	d := string(drainSrc)
	for _, pin := range []string{
		`drainReasonRestartGeneration     = "restart_generation"`,
	} {
		if !strings.Contains(d, pin) {
			t.Errorf("controller drain reason constant changed: %q", pin)
		}
	}
	if !strings.Contains(src, `"reason": "restart_generation"`) {
		t.Error("script's negative grep must use the exact restart_generation reason value")
	}
	forceSrc, err := os.ReadFile("../controller/internal/workspace/force_recycle.go")
	if err != nil {
		t.Skip("controller source not present in this checkout layout")
	}
	if !strings.Contains(string(forceSrc), `"restart_generation_user_forced"`) {
		// The forced reason is composed (drainReasonRestartGeneration +
		// "_user_forced") at the call site — pin the composition instead.
		activeSrc, err := os.ReadFile("../controller/internal/workspace/phase_active.go")
		if err != nil || !strings.Contains(string(activeSrc), `drainReasonRestartGeneration+"_user_forced"`) {
			t.Error("the forced-reason composition moved — update the script's positive grep with it")
		}
	}
}

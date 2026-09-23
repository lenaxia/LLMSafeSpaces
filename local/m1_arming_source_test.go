// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// M1 (design 0061 §3, crash-loud arming) — the source-truth pins: the
// wiring the unit tests cannot reach (main() is not unit-runnable) is
// pinned against the source, the release-smoke-marker precedent
// (local/release_smoke_relay_markers_test.go — the YAML-grep and the
// source literals cannot drift apart). The four design shapes:
//
//	unarmable   → exit 85 within the window (main.go's wiring, pinned here)
//	armed       → the enable line + exit 0 (the posture gate asserts it
//	              cluster-side; the literal is pinned by the release
//	              smoke)
//	parity      → 85 too (the #1548 provenance PR's assert — reconciles
//	              onto this constant when it merges; the reconciliation
//	              is the pin below)
//	flag-off    → byte-identical (the unit test's nil-nil shape)

import (
	"os"
	"strings"
	"testing"
)

func mustReadM1(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The wiring: main.go's staging-error path exits with THE constant, not
// bare 1 — reverting to os.Exit(1) turns this red (the #1548 ambiguity
// class).
func TestM1_MainExitsDistinctCodeOnStagingFailure(t *testing.T) {
	src := mustReadM1(t, "../controller/main.go")
	i := strings.Index(src, "relay-only key delivery: refusing to start")
	if i < 0 {
		t.Fatal("the staging refusal site not found in main.go")
	}
	// TIGHT window (r1 finding 4): the refusal line through the exit —
	// the call and its argument are adjacent; a ±400-char window tripped
	// on a foreign Exit(1) ~286 chars away.
	window := src[i : i+400]
	if !strings.Contains(window, "os.Exit(controller.RelayStagingExitCodeFor(err))") {
		t.Fatalf("the staging-error path must exit through the seam (design 0061 §3) — found:\n%s", window)
	}
}

// The constant's VALUE is the ladder's fifth rung (85) and the ladder
// comment names the doctrine — a future renumbering that collides with
// 81–84 fails here.
func TestM1_ExitConstantSourceTruth(t *testing.T) {
	src := mustReadM1(t, "../controller/internal/controller/controller.go")
	if !strings.Contains(src, "const RelayStagingNotArmedExitCode = 85") {
		t.Fatal("the constant must be 85 (the 81/82-agentd, 83/84-opencode ladder's next rung)")
	}
	if !strings.Contains(src, "81/82: agentd verify; 83/84: opencode verify") ||
		!strings.Contains(src, "85: relay staging not") {
		t.Fatal("the ladder comment must name the doctrine (the runbook's grep surface)")
	}
	// The window constant feeds the guard's context (no new timer).
	if !strings.Contains(src, "context.WithTimeout(context.Background(), ArmingStartupGuardWindow)") {
		t.Fatal("the startup guard must read ArmingStartupGuardWindow — the design's window IS the guard's budget")
	}
}

// The parity reconciliation pin: the #1548 provenance PR's
// verifyRelayStagingParity lands AFTER this constant; when it merges,
// its failure path must take THIS code too. Until then the pin asserts
// the reconciliation contract is recorded at the constant (the comment
// names the parity class). When the parity code lands, extend this pin
// to assert its exit site uses the constant.
func TestM1_ParityReconciliationContract(t *testing.T) {
	src := mustReadM1(t, "../controller/internal/controller/controller.go")
	i := strings.Index(src, "const RelayStagingNotArmedExitCode = 85")
	if i < 0 {
		t.Fatal("constant missing")
	}
	doc := src[max(0, i-1500):i]
	if !strings.Contains(doc, "relay-only but not") || !strings.Contains(doc, "split-brain") {
		t.Fatal("the constant's doc block must carry the not-armed contract and the #1548 class it exists for")
	}
}

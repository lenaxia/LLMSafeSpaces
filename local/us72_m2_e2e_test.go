// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Pins for local/us72-m2-fallback-e2e.sh — the M2/M4 e2e migration
// story (design 0061 §10; owns #1548's AC1 + AC4). The standing
// disposition: first recorded execution rides the reviewer runner / the
// #1456 wiring lane; these pins hold the shape (the 1505/1507/1534/1537
// precedents).

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const us72M2E2E = "us72-m2-fallback-e2e.sh"

func mustReadUS72M2(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(us72M2E2E)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUS72M2E2E_BashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	out, err := exec.Command(bash, "-n", us72M2E2E).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The two rows in order: the migration scenario (tokens + the first
// handoff + zero degrades) BEFORE the fallback arm (raw keys + the
// counter + the CRD condition).
func TestUS72M2E2E_RowsInOrder(t *testing.T) {
	src := mustReadUS72M2(t)
	r1 := strings.Index(src, "R1 — the migration scenario")
	r2 := strings.Index(src, "R2 — the fallback arm")
	if r1 < 0 || r2 < 0 {
		t.Fatal("both rows must exist")
	}
	if r1 >= r2 {
		t.Error("R1 (migration) must precede R2 (fallback)")
	}
	for _, marker := range []string{
		"the FIRST",        // AC1's first-handoff clause
		"workspace-relay-", // the handoff Secret
		"CredentialsStaged=True",
		"zero relay_staging_not_ready degrades", // AC4 (superseded text, still must hold when staging is healthy)
		"the RAW canary key",
		"relay_fallback_deliveries_total",
		"False relay_fallback_delivery", // the M4 seam contract
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("the e2e must assert %q", marker)
		}
	}
}

// The fallback delivery is checked BOTH ways: the token on the healthy
// path (NOT the canary) and the raw canary on the torn path — a script
// that only checks one direction can pass both rows on one stale
// config.
func TestUS72M2E2E_TokenAndRawBothAsserted(t *testing.T) {
	src := mustReadUS72M2(t)
	if !strings.Contains(src, `"${key}" != "${CANARY_KEY}"`) {
		t.Error("R1 must assert the token path is NOT the canary")
	}
	if !strings.Contains(src, `"${key}" == "${CANARY_KEY}"`) {
		t.Error("R2 must assert the fallback path IS the canary")
	}
}

// The counter assertion greps the API's /metrics with BOTH labels
// (workspace + provider_slug) — a label-less grep could pass on any
// workspace's fallback.
func TestUS72M2E2E_CounterLabelsPinned(t *testing.T) {
	src := mustReadUS72M2(t)
	if !strings.Contains(src, `provider_slug=\"m2e2e\"`) {
		t.Error("the counter grep must pin provider_slug=m2e2e")
	}
	if !strings.Contains(src, `grep -qF "${WS}"`) {
		t.Error("the counter grep must pin THIS workspace")
	}
}

// Per-script isolation (the #1342 pattern).
func TestUS72M2E2E_WorkspaceIsolation(t *testing.T) {
	src := mustReadUS72M2(t)
	if !strings.Contains(src, `WS_BASE="e2e072m2-`) {
		t.Error("the script must set its own WS_BASE unconditionally")
	}
}

// TestUS72M2E2E_ExecuteSmoke — the repo's harness-script mandate ("a new
// harness script lands with its smoke or it does not land"): the script
// runs under the shim harness. Depth pin, exactly as far as it reaches:
// the shims answer kubectl generically, so the cluster assertions fail
// — which is exactly the depth this smoke proves: BOTH rows execute
// end-to-end to the final verdict with no runtime abort (the r3
// invalid-UUID death at setup is caught in seconds).
func TestUS72M2E2E_ExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smoke spawns shim processes")
	}
	combined, exitVal := runScriptUnderShims(t, us72M2E2E, "Active", map[string]string{
		"CANARY_KEY": "sk-SMOKE-canary-0smoke1row2x",
	})
	assertSmokeTraversal(t, us72M2E2E, combined, exitVal, "row(s) failed", "")
	for _, reached := range []string{
		"R1 — the migration scenario",
		"R2 — the fallback arm",
		"row(s) failed",
	} {
		if !strings.Contains(combined, reached) {
			t.Fatalf("died before [%s] — a runtime death the needles cannot catch:\n%s", reached, smokeTail(combined))
		}
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/us-72-relay-only-flip-drill.sh — US-72.5's scripted
// rollback drill (design 0058 §D6.1: flip → validate → rollback →
// validate → flip). The drill runs on the harness/pool cluster (first
// recorded execution rides the reviewer's runner or the #1456 wiring
// lane — the established disposition); these pins hold its shape (the
// 1505/1507 pin precedents).

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const us72FlipDrill = "us-72-relay-only-flip-drill.sh"

func mustReadUS72Flip(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(us72FlipDrill)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUS72FlipDrill_BashSyntax(t *testing.T) {
	bash := requireBashLocal(t)
	out, err := exec.Command(bash, "-n", us72FlipDrill).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The D6.1 loop's four legs in order: flip on → token-only canary →
// rollback (with the positive control) → flip on again. A drill missing
// any leg — or running them out of order — does not exercise the
// rollback.
func TestUS72FlipDrill_FourLegsInOrder(t *testing.T) {
	src := mustReadUS72Flip(t)
	legs := []string{
		`R1 — flip ON`,
		`R2 — token-only canary`,
		`R3 — rollback`,
		`R4 — flip ON again`,
	}
	last := -1
	for _, leg := range legs {
		i := strings.Index(src, leg)
		if i < 0 {
			t.Errorf("drill must contain leg %q", leg)
			continue
		}
		if i <= last {
			t.Errorf("leg %q out of order (offset %d after %d)", leg, i, last)
		}
		last = i
	}
}

// The canary sweep is the drill's security verdict: it must grep EVERY
// uid-1000-readable surface (the design 0058 §1.2 inventory) for the
// planted bytes, and the rollback leg must assert the canary RETURNS
// (the positive control that proves the sweep can fail).
func TestUS72FlipDrill_CanarySweepAndPositiveControl(t *testing.T) {
	src := mustReadUS72Flip(t)
	for _, marker := range []string{
		`/sandbox-runtime/agent-config.json`,
		`/agentd-config/agent-config.json`,
		`/workspace/.local/opencode/auth.json`,
		`/sandbox-cfg/secrets.json`,
		`/sandbox-runtime/rt/secrets.json`,
		`/sandbox-runtime/rt/auth.json`,
		`/proc/self/environ`,
		`CANARY`,
		`apiKey is the token`,
		`baseURL points at the relay router`,
		`rollback restores the raw-key path`,
		`CredentialsStaged`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("drill must exercise %q — the flip drill's verdict without it is decorative", marker)
		}
	}
}

// The flips must REUSE the standing install's values — a bare upgrade
// would reset the harness's pinned images and break the cluster (the
// drill mutates a standing release, it does not install a fresh one).
func TestUS72FlipDrill_ReusesStandingValues(t *testing.T) {
	src := mustReadUS72Flip(t)
	if !strings.Contains(src, "--reuse-values") {
		t.Error("every helm upgrade in the drill must --reuse-values — otherwise the harness install's pinned values are reset mid-drill")
	}
	if strings.Contains(src, "helm upgrade --install llmsafespaces") && !strings.Contains(src, "--reuse-values") {
		t.Error("unreachable (belt+braces)")
	}
}

// Per-script workspace isolation (the #1342 note).
func TestUS72FlipDrill_WorkspaceIsolation(t *testing.T) {
	src := mustReadUS72Flip(t)
	if !strings.Contains(src, `WS_BASE="e2e72500-`) {
		t.Error("drill must set its own WS_BASE unconditionally (per-script isolation)")
	}
}

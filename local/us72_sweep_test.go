// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/us-72-rogue-agent-sweep.sh — US-72.6's close-out row
// (design 0058 §8; #820's exit criterion K1). Same pin family as the
// flip drill's.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const us72Sweep = "us-72-rogue-agent-sweep.sh"

func mustReadUS72Sweep(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(us72Sweep)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUS72Sweep_BashSyntax(t *testing.T) {
	bash := requireBashLocal(t)
	out, err := exec.Command(bash, "-n", us72Sweep).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The three rows in order: the exit-criterion sweep (zero canary
// bytes), the positive control (plant, find, scrub, zero), and the
// residue-boot migration row (plant on the PVC, suspend, resume — the
// BOOT scrub fires; the actual US-72.6 scenario). A sweep without the
// positive control cannot prove it can fail; without R3 it never
// exercises the PR's central trigger.
func TestUS72Sweep_RowsInOrder(t *testing.T) {
	src := mustReadUS72Sweep(t)
	r1 := strings.Index(src, "R1 — the exit-criterion sweep")
	r2 := strings.Index(src, "R2 — positive control")
	r3 := strings.Index(src, "R3 — the residue-boot migration row")
	if r1 < 0 || r2 < 0 || r3 < 0 {
		t.Fatal("all three rows must exist")
	}
	if r1 >= r2 || r2 >= r3 {
		t.Error("R1 → R2 → R3 (exit criterion → positive control → residue boot)")
	}
	for _, marker := range []string{
		"the sweep can fail",
		"post-sweep zero",
		"the scrub's own report shows the removal",
		"the BOOT scrub fired on the residue-bearing resume",
		"auth=1",
		"post-boot sweep zero",
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("sweep must assert %q", marker)
		}
	}
}

// The sweep covers the full uid-1000-readable inventory + every pod
// process's environ (the story's rogue gallery), and the scrub exec
// targets the agentd binary with the subcommand.
func TestUS72Sweep_SurfacesAndScrubExec(t *testing.T) {
	src := mustReadUS72Sweep(t)
	for _, marker := range []string{
		`/sandbox-runtime/agent-config.json`,
		`/agentd-config/agent-config.json`,
		`/workspace/.local/opencode/auth.json`,
		`/sandbox-cfg/secrets.json`,
		`/sandbox-runtime/rt/secrets.json`,
		`/sandbox-runtime/rt/auth.json`,
		`/proc/[0-9]*/environ`,
		`scrub-legacy-keys --workspace-root /workspace`,
		`/agentd/usr/local/bin/workspace-agentd`,
		// The planted residue IS the pre-US-35.7 shape (a regular file
		// at the auth path — the scrub's target class).
		`> /workspace/.local/opencode/auth.json`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("sweep must exercise %q", marker)
		}
	}
}

// The counting branch must be the drilled, pinned form (numeric-only
// grep -c; transport failure = a hit — fail-closed rows).
func TestUS72Sweep_CountingBranchMatchesDrill(t *testing.T) {
	src := mustReadUS72Sweep(t)
	for _, marker := range []string{
		`out=$(grep -ac "'"${CANARY_KEY}"'" "$p" 2>/dev/null || true)`,
		`[[ "${out}" =~ ^[0-9]+$ ]] && hits=$((hits + out))`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("sweep's counting branch must contain %q (the drilled form)", marker)
		}
	}
}

// Per-script isolation (the #1342 pattern).
func TestUS72Sweep_WorkspaceIsolation(t *testing.T) {
	src := mustReadUS72Sweep(t)
	if !strings.Contains(src, `WS_BASE="e2e72600-`) {
		t.Error("sweep must set its own WS_BASE unconditionally")
	}
}

// The plant must break the modern symlink first (r1 finding 3): a bare
// `>` follows the #1296 link into the live store and the row fails
// structurally on every modern pod.
func TestUS72Sweep_PlantBreaksTheSymlink(t *testing.T) {
	src := mustReadUS72Sweep(t)
	i := strings.Index(src, "rm -f /workspace/.local/opencode/auth.json")
	if i < 0 {
		t.Fatal("the plant must rm -f the symlink before writing the legacy regular file")
	}
	plant := strings.Index(src, "> /workspace/.local/opencode/auth.json")
	if plant >= 0 && i > plant {
		t.Error("the rm -f must precede the write (order is load-bearing)")
	}
}

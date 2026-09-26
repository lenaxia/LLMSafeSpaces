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
		"config=1",
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
	// The counting semantics (the drilled form): numeric-only counting,
	// unreadable paths pass. The per-path branch is an if-guard that
	// ALSO reports the hit path (the 36135708380 diagnosability gap —
	// 3 hits, no idea where). NOTE the r1 correction: the first version
	// of this change merged stderr into the pipe (`2>&1 | tail -1`) and
	// this pin was AMENDED TO ENSHRINE THE NO-OP — tail -1 dropped every
	// HIT line. The contract below is the honest one: tee /dev/stderr
	// surfaces the HIT lines to the CI log while tail -1 still captures
	// the count as the caller's stdout.
	for _, marker := range []string{
		`out=$(grep -ac "'"${CANARY_KEY}"'" "$p" 2>/dev/null || true)`,
		`if [[ "${out}" =~ ^[0-9]+$ ]] && (( out > 0 )); then`,
		`hits=$((hits + out)); echo "HIT ${p} x${out}" >&2`,
		`hits=$((hits + out)); echo "HIT ${env} x${out}" >&2`,
		`echo "${hits}"`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("sweep's counting branch must contain %q (the drilled semantics + the HIT-path report)", marker)
		}
	}
	// The output contract: tee /dev/stderr SURFACES the HIT lines (they
	// reach the CI log); tail -1 captures the count on stdout. The
	// no-op form (`2>&1 | tail -1` alone) must be ABSENT.
	if !strings.Contains(src, `2>&1 | tee /dev/stderr | tail -1 || echo 1`) {
		t.Error("sweep_hits must tee /dev/stderr (HIT lines surface) and tail the count — the bare 2>&1|tail form DROPS them (the r1 no-op)")
	}
	if strings.Contains(src, `2>&1 | tail -1`) {
		t.Error("the no-op merge form must be gone — it drops every HIT line")
	}
}

// Per-script isolation (the #1342 pattern).
func TestUS72Sweep_WorkspaceIsolation(t *testing.T) {
	src := mustReadUS72Sweep(t)
	if !strings.Contains(src, `WS_BASE="e2e72600-`) {
		t.Error("sweep must set its own WS_BASE unconditionally")
	}
}

// The R3 plant's SURVIVABILITY (r4 finding 1): init-fs manages the
// auth.json path (replaceSymlink deletes a pre-existing regular file
// before installing the #1296 symlink) — a Surface-1 plant never
// survives to the boot scrub. The R3 plant must be the Surface-2 copy
// (.local/config/opencode/agent-config.json) — scrub territory init-fs
// never touches.
func TestUS72Sweep_R3PlantIsInitFsSurvivable(t *testing.T) {
	src := mustReadUS72Sweep(t)
	if !strings.Contains(src, "> /workspace/.local/config/opencode/agent-config.json") {
		t.Fatal("R3 must plant the Surface-2 copy (agent-config.json under .local/config/opencode/)")
	}
	// SEMANTIC (r5; comment corrected r7 after the reviewer's
	// mutation proof): asserts against a MIRRORED copy of the managed
	// table, drift-guarded by TestUS72Sweep_InitFsTableMirrorTracksSource
	// below. The guarantee is ONE-DIRECTIONAL: modification or removal
	// of the four KNOWN entries trips the drift guard; ADDITION-direction
	// drift (a future fifth entry covering the plant path) does NOT fail
	// these pins — it is backstopped by R3's runtime poll timeout (loud,
	// but at cluster-run cost, not by this pin). The r4 literal-substring
	// form could not even see modifications (filepath.Join-built paths).
	pvcRoot := "/pvc"
	runtimeDir := "/sandbox-runtime"
	links := initFSManagedLinksForPin(pvcRoot, runtimeDir)
	plant := pvcRoot + "/workspace/.local/config/opencode/agent-config.json"
	for _, l := range links {
		if l[0] == plant {
			t.Errorf("init-fs manages the R3 plant path %s — the plant would be deleted before the boot scrub; move the plant", plant)
		}
	}
	// The SANITY half: the Surface-1 path IS managed (the reason R3
	// moved) — if init-fs ever DROPS that management, the r4 history
	// and the R2 rm -f rationale need re-review.
	managedAuth := pvcRoot + "/workspace/.local/opencode/auth.json"
	saw := false
	for _, l := range links {
		if l[0] == managedAuth {
			saw = true
		}
	}
	if !saw {
		t.Errorf("init-fs no longer manages %s — the r4 fix rationale is stale; re-review the R2/R3 plants", managedAuth)
	}
}

// initFSManagedLinksForPin mirrors the agentd package's
// initFSManagedLinks (same package at test time is not possible from
// local/ — the paths are the contract; keep them in sync with
// cmd/workspace-agentd/init_fs.go, enforced by the residue pin below).
func initFSManagedLinksForPin(pvcRoot, runtimeDir string) [][2]string {
	return [][2]string{
		{pvcRoot + "/home/.ssh", runtimeDir + "/rt/ssh"},
		{pvcRoot + "/home/.secrets", runtimeDir + "/rt/secrets"},
		{pvcRoot + "/home/.git-credentials", runtimeDir + "/rt/git-credentials"},
		{pvcRoot + "/workspace/.local/opencode/auth.json", runtimeDir + "/rt/auth.json"},
	}
}

// The mirrored table must track the source (the residue guard): if
// init_fs.go's table changes, this pin fails until the mirror is
// updated — keeping the survivability pin honest.
func TestUS72Sweep_InitFsTableMirrorTracksSource(t *testing.T) {
	initSrc, err := os.ReadFile("../cmd/workspace-agentd/init_fs.go")
	if err != nil {
		t.Fatal("init_fs.go not present in this checkout layout (the mirror residue guard requires it)")
	}
	src := string(initSrc)
	for _, entry := range []string{
		`"home", ".ssh"`,
		`"home", ".secrets"`,
		`"home", ".git-credentials"`,
		`"workspace", ".local", "opencode", "auth.json"`,
	} {
		if !strings.Contains(src, entry) {
			t.Errorf("init_fs.go's managed table no longer contains %q — update initFSManagedLinksForPin (the mirror drifted)", entry)
		}
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

// TestUS72Sweep_R3GateIsPodFresh (r3): the post-boot sweep's convergence
// gate reads POD-FRESH evidence (the resumed pod's live config carrying
// token+router) — NOT the CredentialsStaged condition, which survives
// suspend (conditions are never cleared on the suspend path) and so
// reads the PRE-SUSPEND pod's verdict after resume.
func TestUS72Sweep_R3GateIsPodFresh(t *testing.T) {
	src := mustReadUS72Sweep(t)
	if !strings.Contains(src, "config_converged() {") {
		t.Fatal("R3's gate must be the pod-fresh config_converged() check (token+router in the RESUMED pod's live config)")
	}
	if !strings.Contains(src, `.provider["us72sweep"].options[$f] // ""`) {
		t.Fatal("the sweep provider's field reader must exist (the drill's shape, keyed to us72sweep)")
	}
	// The stale form — polling CredentialsStaged AFTER the resume — must
	// appear only in the R1 setup gate (fresh workspace: no pre-existing
	// condition, the first True is genuinely fresh), never as R3's gate.
	r3Block := src[strings.Index(src, "R3 — the residue-boot migration row"):]
	if strings.Contains(r3Block, "condition_status CredentialsStaged") {
		t.Fatal("R3 must not gate on the CredentialsStaged condition — it survives suspend (the pre-suspend pod's verdict, not the resumed pod's)")
	}
}

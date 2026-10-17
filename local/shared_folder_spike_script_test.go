// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Companion pins for local/1709-shared-folder-spike.sh and
// local/lib/longhorn.sh (#1709 parked-PVC spike). The spike runs on the
// dind pool; these pins hold its shape off-cluster (the
// TestUS70Scripts_BashSyntax / issue_1505 / s5 precedents):
//
//   - bash syntax for both scripts (the house pin),
//   - rendered pod/job scripts pass dash -n (the review-C1 class: a
//     parse-fatal heredoc rendered `$$i` and could never produce leg-B/C
//     results; the render mode emits exactly the block bodies the live
//     heredocs produce, and /bin/sh of the debian/rclone images is dash
//     or busybox ash — dash -n is the stricter gate),
//   - the C1 signature stays dead: no `\$\$` double-escape anywhere in
//     the script (belt-and-braces against drift between the live
//     heredocs and the render mode),
//   - the journal parser's verdicts on synthetic fixtures (clean /
//     torn-tail / gap / dupe / empty) via the script's own selftest
//     (review N-B: a vacuous crash-consistency gate must fail loudly
//     here before it can fail quietly on the pool),
//   - the exit contract: any leg FAIL ⇒ non-zero exit (review C4),
//   - the runc/gvisor RuntimeClass preflight exists (review N-A),
//   - the hydration size gate exists (no fabricated throughput),
//   - longhorn.sh: retry uses return (not exit), the real chart key
//     preUpgradeChecker.jobEnabled, and the StorageClass carries the
//     prod parameter set including unmapMarkSnapChainRemoved (R2/R5).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	spikeScript = "1709-shared-folder-spike.sh"
	lhLibScript = "lib/longhorn.sh"
	spikeWF     = "../.github/workflows/shared-folder-spike.yml"
)

func mustReadSpike(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireBashSpike(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	return bash
}

func TestSharedFolderSpike_BashSyntax(t *testing.T) {
	bash := requireBashSpike(t)
	for _, script := range []string{spikeScript, lhLibScript} {
		if out, err := exec.Command(bash, "-n", script).CombinedOutput(); err != nil {
			t.Fatalf("bash -n %s failed: %s", script, out)
		}
	}
}

// The C1 signature must stay dead: a doubled \$ escape inside the
// unquoted heredocs renders the pod-internal loop variable as `$$i`
// (PID + i) and the generated script dies at parse time under dash/ash.
func TestSharedFolderSpike_NoDollarDollarEscape(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	if strings.Contains(src, `\$\$`) {
		t.Fatal("found a `\\$\\$` double-escape — the review-C1 parse-fatal class; render loop vars as \\$i")
	}
}

// Every generated pod/job script must parse under /bin/sh (dash) — the
// parse gate that would have caught C1 pre-push. render emits the same
// block bodies the live heredocs produce at default env.
func TestSharedFolderSpike_RenderedScriptsParseUnderDash(t *testing.T) {
	bash := requireBashSpike(t)
	dir := t.TempDir()
	out, err := exec.Command(bash, spikeScript, "render", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 4 {
		t.Fatalf("render emitted %d scripts, want >= 4 (seed/hydrate/writer/sync): %s", len(entries), out)
	}
	sh, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not available (debian/rclone images ship it as /bin/sh; CI runners have it)")
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
			t.Errorf("dash -n %s failed (the C1 class): %s", e.Name(), out)
		}
	}
}

// The journal parser + exit-contract helper selftest (fixtures: clean,
// torn-tail, gap, dupe, empty). A regression here is a vacuous
// crash-consistency gate on the pool (review N-B).
func TestSharedFolderSpike_Selftest(t *testing.T) {
	bash := requireBashSpike(t)
	out, err := exec.Command(bash, spikeScript, "selftest").CombinedOutput()
	if err != nil {
		t.Fatalf("selftest failed: %s", out)
	}
	if !strings.Contains(string(out), "SELFTEST=PASS") {
		t.Fatalf("selftest did not report PASS: %s", out)
	}
}

// Exit contract: the script's final gate must consult ALL THREE legs
// (review C4: a leg-B/C FAIL silently exiting 0 made the workflow check
// green without evidence).
func TestSharedFolderSpike_ExitContractCoversAllLegs(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	if !strings.Contains(src, `leg_exit "$A_STATUS" "$B_STATUS" "$C_STATUS" || exit 1`) {
		t.Fatal("final exit gate must be leg_exit over A/B/C — any FAIL exits non-zero")
	}
	if !strings.Contains(src, `leg_exit() { [ "$1" = "PASS" ] && [ "$2" = "PASS" ] && [ "$3" = "PASS" ]; }`) {
		t.Fatal("leg_exit helper must require PASS from all three legs")
	}
}

// The journal writer must append WHOLE records to the journal (grouped
// redirect) — the run-1 writer leaked every field but the newline to
// pod stdout and the journal recorded blank lines (review C3).
func TestSharedFolderSpike_WriterAppendsWholeRecords(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	if !strings.Contains(src, `} >> /shared/journal.bin`) {
		t.Fatal("journal writer must append the grouped record (printf + dd|base64 + newline) to /shared/journal.bin")
	}
}

// RuntimeClass preflight: both runtime classes the generated pods
// reference must be verified present BEFORE any pod is created (review
// N-A — a missing runc class killed the B2 comparison at admission).
func TestSharedFolderSpike_RuntimeClassPreflight(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	for _, marker := range []string{
		`for RC in gvisor runc`,
		`RuntimeClass ${RC} not registered`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("missing runtimeclass preflight marker: %q", marker)
		}
	}
}

// Throughput-validity: hydration jobs must assert the copied byte count
// before the leg accepts a rate (an empty dataset otherwise yields
// fabricated MB/s off pure wall time).
func TestSharedFolderSpike_HydrationSizeGate(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	for _, marker := range []string{
		`rclone size --json /shared`,
		`HYD_SIZE=FAIL`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("missing hydration size-gate marker: %q", marker)
		}
	}
}

// Longhorn lib pins: retry path returns (not exits — it runs inside an
// `if` in the wrapper), the real chart key preUpgradeChecker.jobEnabled
// (helm --set silently accepts the wrong `preUpgradeChecker.enabled`),
// and the user-shared StorageClass carries the prod parameter set
// including unmapMarkSnapChainRemoved (doc/script alignment, R5).
func TestLonghornLib_Shape(t *testing.T) {
	src := mustReadSpike(t, lhLibScript)
	if strings.Contains(src, `preUpgradeChecker.enabled=`) {
		t.Fatal("preUpgradeChecker.enabled is a dead key (chart 1.12.1: preUpgradeChecker.jobEnabled) — helm accepts it silently")
	}
	if !strings.Contains(src, `preUpgradeChecker.jobEnabled=false`) {
		t.Fatal("must disable the pre-upgrade checker via the real key jobEnabled")
	}
	if !strings.Contains(src, `unmapMarkSnapChainRemoved: "ignored"`) {
		t.Fatal("user-shared StorageClass must carry the full prod longhorn-2r parameter set (unmapMarkSnapChainRemoved)")
	}
	if strings.Contains(src, `exit 1`+"\n"+`    kubectl --context "${CTX}" -n "$NS_LONGHORN" wait`) {
		t.Fatal("install_once must return 1 (not exit) so the retry wrapper can loop")
	}
	// the instance-manager poll failure path must return, not exit
	if strings.Contains(src, `never appeared (600s)" >&2; exit 1; }`) {
		t.Fatal("instance-manager poll failure must return 1 so the retry wrapper can loop")
	}
}

// Workflow pins: the runc RuntimeClass must be applied beside gvisor
// (N-A), and the node must be image-pre-warmed via kind load (R3).
func TestSharedFolderSpike_WorkflowShape(t *testing.T) {
	src := mustReadSpike(t, spikeWF)
	for _, marker := range []string{
		`name: runc`,
		`handler: runc`,
		`kind load docker-image`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("workflow missing marker %q", marker)
		}
	}
}

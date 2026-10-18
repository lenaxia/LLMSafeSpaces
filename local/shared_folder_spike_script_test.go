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

// Every generated pod/job script must parse under /bin/sh (dash) AND be
// non-vacuous (≥5 lines + its anchor + its load-bearing construct) —
// the parse gate that would have caught C1 pre-push, made drift-proof
// by pinning that each rendered body's distinctive lines exist verbatim
// in the live script (N-R5-1: render once emitted 1-line files because
// the heredoc attached to `echo`, and the gate passed vacuously).
func TestSharedFolderSpike_RenderedScriptsParseUnderDash(t *testing.T) {
	bash := requireBashSpike(t)
	dir := t.TempDir()
	out, err := exec.Command(bash, spikeScript, "render", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("render failed: %s", out)
	}
	sh, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not available (debian/rclone images ship it as /bin/sh; CI runners have it)")
	}
	src := mustReadSpike(t, spikeScript)
	shape := map[string]string{ // file → load-bearing construct it MUST contain
		"seed.sh":    "while [ $i -lt",
		"hydrate.sh": `rclone size --json /shared`,
		"writer.sh":  "} >> /shared/journal.bin",
		"sync.sh":    "JOURNAL_VERDICT=PASS",
	}
	if len(shape) != 4 {
		t.Fatal("shape table edited — keep in lockstep with the render set")
	}
	for name, marker := range shape {
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("render did not emit %s: %s", name, out)
		}
		if n := strings.Count(string(body), "\n"); n < 5 {
			t.Errorf("%s rendered trivially (%d lines) — the N-R5-1 vacuous-gate class", name, n)
		}
		if !strings.Contains(string(body), "# spike-script:") {
			t.Errorf("%s missing its anchor comment", name)
		}
		if !strings.Contains(string(body), marker) {
			t.Errorf("%s missing its load-bearing construct %q", name, marker)
		}
		if !strings.Contains(src, marker) {
			t.Errorf("live↔render drift: %q is in the render but NOT in the live script", marker)
		}
		if cmd := exec.Command(sh, "-n", path); true {
			if o, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("dash -n %s failed (the C1 class): %s", name, o)
			}
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

// Exit contract: the script's final gate must consult ALL FOUR leg
// statuses (reviews C4 + N-R8-3 — a leg-B/C/0 FAIL silently exiting 0
// made the workflow check green without evidence).
func TestSharedFolderSpike_ExitContractCoversAllLegs(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	if !strings.Contains(src, `leg_exit "$S0_STATUS" "$A_STATUS" "$B_STATUS" "$C_STATUS" || exit 1`) {
		t.Fatal("final exit gate must be leg_exit over 0/A/B/C — any FAIL exits non-zero")
	}
	if !strings.Contains(src, `leg_exit() { [ "$1" = "PASS" ] && [ "$2" = "PASS" ] && [ "$3" = "PASS" ] && [ "$4" = "PASS" ]; }`) {
		t.Fatal("leg_exit helper must require PASS from all four legs")
	}
	if !strings.Contains(src, `a_status=SKIPPED_LEG0_FAILED`) {
		t.Fatal("leg-0 FAIL must skip legs A-C with a recorded row, not re-demonstrate the failure")
	}
}

// FAIL-sentinel normalization (N-R5-2): every failure sentinel the legs
// write must literally end in =FAIL so the per-leg aggregations (which
// grep "=FAIL") cannot silently PASS over it.
func TestSharedFolderSpike_FailSentinelsNormalize(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	if strings.Contains(src, "DELTA_SEED_FAIL") {
		t.Fatal("DELTA_SEED_FAIL escapes the =FAIL aggregation (ninth-round finding) — sentinels must end in =FAIL")
	}
	if !strings.Contains(src, `=FAIL (delta-seed job failed)`) {
		t.Fatal("the delta-seed failure row must be a =FAIL sentinel")
	}
}

// Leg-0 shape (N-R8-1/2/3 regressions): namespace pre-created before the
// first leg-0 apply, every delete namespace-scoped, S0 consumed.
func TestSharedFolderSpike_Leg0Shape(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	preCreate := strings.Index(src, `create namespace "${NS}"`)
	firstUse := strings.Index(src, "name: rwo-test")
	if preCreate < 0 || firstUse < 0 || preCreate > firstUse {
		t.Fatal("namespace pre-create must precede leg 0's first cluster-side use")
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "${K} delete") && !strings.Contains(trimmed, " -n ") {
			t.Fatalf("bare namespace-less delete (N-R8-2 class): %s", trimmed)
		}
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
	// the instance-manager poll failure path must return, not exit
	if strings.Contains(src, `never appeared (600s)" >&2; exit 1; }`) {
		t.Fatal("instance-manager poll failure must return 1 so the retry wrapper can loop")
	}
}

// Prereqs content pin (N-ISC-2 + reviews N-R18-1/N-R18-2/N-R22-3): the
// node provisioning block must gate on MEASURED exec identity inside the
// payload itself (single-exec gate — one-shot probes are a TOCTOU the
// identity drift walks through), re-check before each privileged group
// (the mutation is observed MID-EXEC too), verify iscsid (pgrep + IPC
// smoke test), and abort loud with the measured identity on filesystem
// failures — never a hardcoded identity claim. This block consumed
// seven rounds; it ships pinned to its current shape.
func TestLonghornLib_PrereqsContent(t *testing.T) {
	src := mustReadSpike(t, lhLibScript)
	for _, marker := range []string{
		`require_root() {`,
		`aborting before provisioning`,
		`pgrep -x iscsid`,
		`can not connect to iSCSI daemon`,
		`failed as measured user $(id -un)`,
		`systemd-tmpfiles --create`,
		`/etc/iscsi/initiatorname.iscsi`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("longhorn.sh prereqs missing marker %q (the seven-round unpinned block)", marker)
		}
	}
	if strings.Contains(src, "docker exec -u 0") {
		t.Fatal("-u 0 execs never deliver root on this pool (r18: uid=1001(runner), the runner-image shape) — bare exec + in-payload identity gate is the contract")
	}
}

// Exec-fed instrumentation guards (N-R18-3): the disk budget must reject
// non-numeric probe results as EXEC failures (not "disk too small"),
// and the iscsid liveness row must distinguish exec failure from dead.
func TestSharedFolderSpike_ExecFedInstrumentation(t *testing.T) {
	src := mustReadSpike(t, spikeScript)
	for _, marker := range []string{
		`node_free_gb=EXEC_FAILED`,
		`not a disk-size verdict`,
		`env_iscsid_alive=EXEC_FAILED`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("exec-fed instrumentation unguarded (N-R18-3): missing %q", marker)
		}
	}
	if strings.Contains(src, "docker exec -u 0") {
		t.Fatal("spike script still uses -u 0 execs (r18: never delivers root on this pool)")
	}
}

// Workflow pins: the runc RuntimeClass must be applied beside gvisor
// (N-A), the node must be image-pre-warmed via ctr, and EVERY ctr ref
// must carry a registry host (N-R6-2: the bare `debian@sha256:...` form
// resolves via dummy:// and killed run 39077806519 — mutation-verified
// as untested last round).
func TestSharedFolderSpike_WorkflowShape(t *testing.T) {
	src := mustReadSpike(t, spikeWF)
	for _, marker := range []string{
		`name: runc`,
		`handler: runc`,
		`ctr -n k8s.io images pull`,
	} {
		if !strings.Contains(src, marker) {
			t.Fatalf("workflow missing marker %q", marker)
		}
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `"`) && strings.Contains(trimmed, "@sha256:") {
			if !strings.Contains(trimmed, `docker.io/`) {
				t.Fatalf("ctr ref without registry host (N-R6-2 class): %s", trimmed)
			}
		}
	}
}

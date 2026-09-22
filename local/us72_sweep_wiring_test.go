// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us72_sweep_wiring_test.go — US-72.6 complementary vehicle (design 0058
// §8, line 201): the structural/executable pins for the NIGHTLY WIRING
// of the rogue-agent sweep. The sweep script itself
// (local/us-72-rogue-agent-sweep.sh) is the owner's #1537 vehicle — it
// is NOT on main while this wiring lands, so the workflow step
// feature-detects it and SKIPs loudly until #1537 merges (the F8 /
// #1342 absent-dependency precedent). These pins therefore test the
// WORKFLOW ONLY — they never read the script file (which would couple
// this PR to #1537's tree), and they prove:
//
//   - ordering: the sweep runs AFTER the drill (it rides the drill's
//     flipped-ON end state: relay on, namespaced scope, router up,
//     mock-llm present) and BEFORE the failure-dump/teardown;
//   - the step's env carries its own PORTFWD_PORT, isolated from every
//     other step's forward;
//   - the skip-guard is load-bearing and loud: with the script absent
//     the body exits 0 naming #1537 (never a hard nightly failure);
//     with the script present the body EXECUTES it (the invocation
//     path is live, not commented out) — both EXECUTED under bash -e,
//     the amputation/guard-inversion class the #1536 pins catch.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const us72SweepScriptRel = "local/us-72-rogue-agent-sweep.sh"

// us72SweepStepName is the nightly step that runs the sweep.
const us72SweepStepName = "Run the rogue-agent sweep (US-72.6, #820 exit criterion)"

// runSweepBodyIn executes the extracted sweep step body under bash -e
// with cwd as the working directory (the body's script path is relative
// to the checkout root) and returns combined output + exit err.
func runSweepBodyIn(t *testing.T, cwd string) (string, error) {
	t.Helper()
	body := stepBody(t, us70NightlyWorkflow, us72SweepStepName)
	script := "set -euo pipefail; export NS=llmsafespaces CLUSTER_NAME=ci\n" + body
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Pin (a): ordering — the sweep runs after the us-70 rows (its harness
// dependencies), after the router build and the drill pre-step (the
// flipped release it sweeps), immediately after the drill itself, and
// before the failure-dump (the dump must capture a sweep failure).
func TestUS72SweepWiring_NightlyOrdering(t *testing.T) {
	src := mustRead(t, us70NightlyWorkflow)
	idx := func(marker, what string) int {
		t.Helper()
		i := strings.Index(src, marker)
		if i < 0 {
			t.Fatalf("%s (%q) not found in the nightly workflow", what, marker)
		}
		return i
	}
	us70 := idx("Run secret-delivery e2e rows", "the us-70 suite step")
	routerBuild := idx("Build + load the llm-relay router image", "the router image build step")
	preStep := idx("Put the release into the drill's valid shape", "the drill pre-step")
	drill := idx("bash local/us-72-relay-only-flip-drill.sh", "the drill step")
	sweep := idx("bash "+us72SweepScriptRel, "the sweep invocation")
	sweepStep := idx(us72SweepStepName, "the sweep step")
	dump := idx("Dump cluster state on failure", "the failure-dump step")
	if us70 >= routerBuild || routerBuild >= preStep || preStep >= drill || drill >= sweepStep || sweepStep >= sweep || sweep >= dump {
		t.Fatalf("ordering violated: us70=%d routerBuild=%d preStep=%d drill=%d sweepStep=%d sweep=%d dump=%d — the sweep needs the drill's flipped end state BEFORE it and must precede the dump",
			us70, routerBuild, preStep, drill, sweepStep, sweep, dump)
	}
}

// Pin (b): the sweep step carries its own port-forward port and it is
// used by NO other step. The lib's cleanup() trap DOES reap its own
// forward on a normal exit — and on errexit too (bash runs the EXIT
// trap on `set -e` deaths; verified empirically, r2). The isolation is
// defense against the one case where the trap genuinely never fires:
// SIGKILL (runner cancellation kills hard) leaks the bound port. A
// distinct port means a leaked drill forward can never break the
// sweep's harness_start.
func TestUS72SweepWiring_PortIsolated(t *testing.T) {
	src := mustRead(t, us70NightlyWorkflow)
	if n := strings.Count(src, "18089"); n != 1 {
		t.Fatalf("the sweep's PORTFWD_PORT 18089 must appear exactly once in the nightly (found %d) — a second use races the forward", n)
	}
	body := stepBody(t, us70NightlyWorkflow, us72SweepStepName)
	// The env block (not the run body) carries the port; extract the
	// step's full text for the env assertions.
	at := strings.Index(src, us72SweepStepName)
	if at < 0 {
		t.Fatalf("sweep step %q not found", us72SweepStepName)
	}
	step := src[at:]
	if end := strings.Index(step, "\n      - name: "); end >= 0 {
		step = step[:end]
	}
	for _, want := range []string{
		"PORTFWD_PORT: 18089",
		"CTX: kind-${{ env.CLUSTER_NAME }}",
		"NS: ${{ env.NS }}",
		"CLUSTER_NAME: ${{ env.CLUSTER_NAME }}",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the sweep step env must carry %q (step env missing/broken)", want)
		}
	}
	if strings.Count(body, "port-forward") > 0 {
		t.Errorf("the step body must not manage port-forwards itself — harness_start inside the script owns the forward")
	}
}

// Pin (c): the skip-guard — EXECUTED, both branches.
//
//	absent script  → exit 0 with a LOUD skip naming #1537 (the sweep
//	                 script's vehicle): never a nightly failure, never a
//	                 silent pass;
//	present script → the body EXECUTES it (recorded via a stub marker).
//
// This is the executable-pin class (#1536 / TestUS70AC1D precedent): a
// guard whose test -f is inverted, a continuation break, or a
// commented-out invocation all fail HERE, not in the first nightly run.
func TestUS72SweepWiring_SkipGuardExecutable(t *testing.T) {
	// Absent: the skip branch.
	out, err := runSweepBodyIn(t, t.TempDir())
	if err != nil {
		t.Fatalf("with the script ABSENT the step must exit 0 (skip-loud, the F8/#1342 precedent) — got exit error %v:\n%s", err, out)
	}
	if !strings.Contains(out, "SKIP") || !strings.Contains(out, "#1537") {
		t.Fatalf("the skip must be LOUD and name the pending vehicle #1537 (got: %q)", strings.TrimSpace(out))
	}
	if !strings.Contains(out, "us-72-rogue-agent-sweep.sh not on this branch yet") {
		t.Fatalf("the skip message must name the missing script path (got: %q)", strings.TrimSpace(out))
	}

	// Present: the invocation branch — a stub script records that the
	// body actually ran it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "local"), 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "invoked")
	stub := "#!/usr/bin/env bash\ntouch " + shQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(dir, us72SweepScriptRel), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = runSweepBodyIn(t, dir)
	if err != nil {
		t.Fatalf("with the script PRESENT the step must execute it cleanly — got exit error %v:\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the step body never invoked %s — the invocation path is dead (commented out / amputated):\n%s", us72SweepScriptRel, out)
	}
}

// Pin (d): mutation insurance — the guard's condition must test the
// EXACT script path the invocation uses. A path typo in either half
// (guard checks foo.sh, invocation runs bar.sh) yields a step that
// either always skips or always fails its first real nightly run; both
// halves must reference the identical relative path.
func TestUS72SweepWiring_GuardAndInvocationSharePath(t *testing.T) {
	body := stepBody(t, us70NightlyWorkflow, us72SweepStepName)
	if n := strings.Count(body, us72SweepScriptRel); n < 2 {
		t.Fatalf("the step body must reference %s at least twice (the guard's test -f AND the bash invocation) — found %d;\nbody:\n%s", us72SweepScriptRel, n, body)
	}
	guardAt := strings.Index(body, "if [[ ! -f "+us72SweepScriptRel+" ]]")
	if guardAt < 0 {
		t.Fatalf("the skip-guard must be `if [[ ! -f %s ]]` exactly (relative to the checkout root):\n%s", us72SweepScriptRel, body)
	}
	if !strings.Contains(body, "bash "+us72SweepScriptRel) {
		t.Fatalf("the invocation must be `bash %s`:\n%s", us72SweepScriptRel, body)
	}
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Pins for local/us61-migration-recovery-e2e.sh — design 0061 §10's
// recovery + arming + posture rows (the formally-closing half of
// #1546/#1548's evidence; the M2 arm's script, us72-m2-fallback-e2e.sh,
// owns the migration scenario + the fallback itself). These pins hold
// the shape (the 1505/1507/1534/1537/us72-m2 precedents): rows in
// order, the load-bearing assertions present, the harness conventions
// followed, and the exit-85 + gate-assertion literals exact.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const us61RecoveryE2E = "us61-migration-recovery-e2e.sh"

func mustReadUS61(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(us61RecoveryE2E)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUS61RecoveryE2E_BashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	out, err := exec.Command(bash, "-n", us61RecoveryE2E).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

// The four rows in order: the fallback precondition BEFORE the recovery
// (the arc needs the degraded state reached), the arming contract
// after, posture convergence last (it clears the corpses R3 leaves).
func TestUS61RecoveryE2E_RowsInOrder(t *testing.T) {
	src := mustReadUS61(t)
	// The log lines (the header comment carries its own R1–R4 outline —
	// pinning the EXECUTED rows, not the prose).
	r1 := strings.Index(src, `log "R1 — the fallback precondition`)
	r2 := strings.Index(src, `log "R2 — RECOVERY`)
	r3 := strings.Index(src, `log "R3 — EXIT 85`)
	r4 := strings.Index(src, `log "R4 — POSTURE CONVERGENCE`)
	require.GreaterOrEqual(t, r1, 0, "R1 header present")
	require.GreaterOrEqual(t, r2, r1, "R2 follows R1")
	require.GreaterOrEqual(t, r3, r2, "R3 follows R2")
	require.GreaterOrEqual(t, r4, r3, "R4 follows R3")
}

// The recovery row's load-bearing assertions: the handoff re-creation
// (the NotFound→Create path — first live pin), the token's return, and
// the M4 condition healing to True.
func TestUS61RecoveryE2E_RecoveryAssertsTheFullArc(t *testing.T) {
	src := mustReadUS61(t)
	for _, needle := range []string{
		`R2: the controller RE-CREATED the deleted`,   // the NotFound→Create path, live
		`R2: the fresh batch carries the TOKEN again`, // the raw canary is gone
		`R2: CredentialsStaged healed back to True`,   // the M4 heal arm
		`healthy → fallback → recovered`,              // the arc, named
		`workspace-relay-${WS}`,                       // the handoff Secret's name shape
	} {
		require.Contains(t, src, needle)
	}
}

// The exit-85 row: the DISTINCT code (not a generic 1), the exact
// refusing line (M1's literal), and the reversibility (restore →
// Ready). §11 ruling 2.
func TestUS61RecoveryE2E_Exit85Contract(t *testing.T) {
	src := mustReadUS61(t)
	for _, needle := range []string{
		`lastState.terminated.exitCode`, // the pod-level assertion
		`"85"`,                          // THE code
		`relay-only key delivery: refusing to start (not armed)`,       // M1's exact literal
		`relayOnlyKeyDelivery.workspaceRouterURL="http://127.0.0.1:9"`, // the unarmable shape (unreachable router)
		`R3: restored — the controller is Ready again`,                 // reversibility
	} {
		require.Contains(t, src, needle)
	}
	// The unreachability lever must NOT carry --wait (the controller
	// will not converge — waiting would time out, not assert).
	unarmable := src[strings.Index(src, "--set relayOnlyKeyDelivery.workspaceRouterURL=\"http://127.0.0.1:9\""):]
	end := strings.Index(unarmable, "exit85=0")
	require.Greater(t, end, 0)
	require.NotContains(t, unarmable[:end], "--wait",
		"the unarmable upgrade must not --wait (it cannot converge; the exit code IS the assertion)")
}

// The posture row mirrors the M3 gate's assertions at e2e level:
// all-Ready in BOTH rendered namespaces, the 45s stability window with
// identical restart snapshots, the armed line, and zero forbidden with
// failure-checked enumeration + prior crashed containers. §11 ruling 3.
func TestUS61RecoveryE2E_PostureConvergenceMirrorsGate(t *testing.T) {
	src := mustReadUS61(t)
	for _, needle := range []string{
		`GATE_NSS=("${NS}" "llm-relay")`,                  // both rendered namespaces
		`wait --for=condition=available deployment --all`, // the gate's own idiom
		`sleep 45`,                                    // the stability window
		`restartCount`,                                // the snapshot basis
		`relay-only key delivery enabled`,             // the armed line (M1/M3 literal)
		`--all-containers=true --previous`,            // corpses included
		`an unenumerable namespace cannot be cleared`, // the failure-checked enumeration
		`could not fetch logs for`,                    // unreadable logs are red
	} {
		require.Contains(t, src, needle)
	}
}

// The harness conventions (the family's isolation + canary patterns).
func TestUS61RecoveryE2E_HarnessConventions(t *testing.T) {
	src := mustReadUS61(t)
	for _, needle := range []string{
		`source "${SCRIPT_DIR}/lib/us70-common.sh"`,
		`WS_BASE="e2e0610a-0000-4000-8000-000000000000"`, // all-hex (the M2 script's r5 lesson)
		`CANARY_KEY="${CANARY_KEY:-sk-US61RC-CANARY-0recover9arc}"`,
		`relay_fallback_deliveries_total`, // the counter, delta-pinned
		`pre_val`,                         // baseline capture before the tear
	} {
		require.Contains(t, src, needle)
	}
}

// The counter assertions are DELTA-based (the M2 script's lesson: a
// stale series from a prior run on a persistent harness cluster
// satisfies an existence grep without a fresh increment).
func TestUS61RecoveryE2E_CounterIsDeltaPinned(t *testing.T) {
	src := mustReadUS61(t)
	require.Contains(t, src, "post_val > pre_val",
		"the increment assertion must compare against the pre-tear baseline, not grep for existence")
}

// The nightly wiring: both migration scripts run on the drill chain,
// ordered (m2-fallback before the recovery arc — the recovery re-seats
// its own degraded state, but a broken fallback arm makes the posture
// assertions infrastructure noise), each ARMED on the full chain.
func TestUS61RecoveryE2E_NightlyWiring(t *testing.T) {
	b, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	m2 := strings.Index(wf, "bash local/us72-m2-fallback-e2e.sh")
	rc := strings.Index(wf, "bash local/us61-migration-recovery-e2e.sh")
	require.GreaterOrEqual(t, m2, 0, "the M2 fallback script is wired")
	require.GreaterOrEqual(t, rc, m2, "the recovery arc runs AFTER the fallback arm")
	for _, needle := range []string{
		"steps.relay-drill.outcome == 'success'", // armed on the drill chain
		"steps.m2-fallback.outcome == 'success'", // the recovery gates on the fallback arm
	} {
		require.Contains(t, wf, needle)
	}
}

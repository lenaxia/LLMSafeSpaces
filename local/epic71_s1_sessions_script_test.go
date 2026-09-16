// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// epic71_s1_sessions_script_test.go — pin tests for the epic-71 /
// s1-sessions (#1372) Act-path cluster suite
// (local/epic71-s1-sessions-act-e2e.sh) and its pool wiring, same
// philosophy as epic71_3a_walkaway_script_test.go: the cluster rows
// cannot run without a cluster, but what is pinnable deterministically
// is the structure past failures actually broke — bash syntax, the lib
// sourcing, the distinct UUID base, the r2-f2 abort-preempts budget, the
// agent-side truth asserts, and the pool running the rows BEFORE the
// fault seam is armed.

import (
	"os/exec"
	"strings"
	"testing"
)

const epic71S1Script = "epic71-s1-sessions-act-e2e.sh"

func TestEpic71S1Script_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", epic71S1Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func TestEpic71S1Script_SourcesCommonLibAndDistinctBase(t *testing.T) {
	src := mustRead(t, epic71S1Script)
	if !strings.Contains(src, "lib/us70-common.sh") {
		t.Fatalf("%s must source lib/us70-common.sh (shared harness helpers)", epic71S1Script)
	}
	if !strings.Contains(src, "e2e71100-0000-4000-8000-000000000000") {
		t.Fatalf("%s must keep its distinct UUID WS_BASE default (workspaces.id is a uuid column; sibling suites share the pool cluster)", epic71S1Script)
	}
}

func TestEpic71S1Script_RowPins(t *testing.T) {
	src := mustRead(t, epic71S1Script)

	// r2-f2: the abort row must BUDGET the preempt, not eyeball it — the
	// budget strictly below the slow-turn duration is what discriminates
	// a preempting abort from one queued behind the turn.
	if !strings.Contains(src, "S1B_ABORT_BUDGET_S") || !strings.Contains(src, "S1-SLOW-TURN") {
		t.Fatalf("s1 script must assert abort's preempt budget against a slow turn")
	}
	// The delete/rename rows' ground truth is AGENT-side (an API-side
	// assert would be circular — the write and the read share the regime).
	if !strings.Contains(src, "agent_get_session_code") || !strings.Contains(src, `.title`) {
		t.Fatalf("s1 script must assert delete/rename outcomes agent-side")
	}
	// The unhappy rows pin the byte-exact 502 bodies (the wire contract
	// does not move — only the write path).
	for _, pinned := range []string{"failed to send message", "failed to abort session", "failed to delete session"} {
		if !strings.Contains(src, pinned) {
			t.Fatalf("s1 script must pin the 502 body %q", pinned)
		}
	}
	// The happy send row asserts the round-tripped contract message, not
	// just a status code.
	if !strings.Contains(src, "S1-TURN-OK") {
		t.Fatalf("s1 script must assert the assistant marker through the Act send round trip")
	}
}

func TestEpic71S1Script_PoolWiring(t *testing.T) {
	pool := mustRead(t, "../.github/workflows/us-70-delivery-pool.yml")
	if !strings.Contains(pool, epic71S1Script) {
		t.Fatalf("us-70-delivery-pool.yml must run %s (the authority-regime sessions rows)", epic71S1Script)
	}
	// Seam-inert: the s1 rows must run BEFORE the fault seam arms (the
	// fault matrix injects failures; the sessions rows assert clean-path
	// semantics).
	s1Idx := strings.Index(pool, epic71S1Script)
	armIdx := strings.Index(pool, "- name: Arm fault seam")
	if s1Idx < 0 || armIdx < 0 || s1Idx > armIdx {
		t.Fatalf("the s1 rows must be wired before the fault seam arms in the pool")
	}
}

func TestEpic71S1Script_BudgetDefaultsEvaluate(t *testing.T) {
	// Pool r2 on this leg died at script START: the idle budget's
	// arithmetic default referenced S1B_SLOW_TURN_S before its
	// declaration (unbound under set -u). bash -n cannot see that class;
	// evaluate the extracted default block under set -euo pipefail.
	bash := requireBash(t)
	src := mustRead(t, epic71S1Script)
	start := strings.Index(src, `S1B_ABORT_BUDGET_S=`)
	end := strings.Index(src[start:], "failures=0")
	if start < 0 || end < 0 {
		t.Fatalf("could not locate the S1B budget-default block in %s", epic71S1Script)
	}
	block := src[start : start+end]
	cmd := exec.Command(bash, "-c", "set -euo pipefail\n"+block+"\necho ok=${S1B_IDLE_BUDGET_S}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("budget defaults fail under set -euo pipefail: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "ok=") {
		t.Fatalf("budget defaults did not evaluate: %s", out)
	}
}

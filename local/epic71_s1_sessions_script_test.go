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
	"fmt"
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

	// r2-f2: the abort row must BUDGET the preempt, not eyeball it —
	// and the duration must ride the PROMPT (the quoted-heredoc env
	// delivery broke the slow mode silently in every prior dispatch —
	// the r4 review's finding; the regex parse has no shell
	// substitution to break).
	if !strings.Contains(src, "S1B_ABORT_BUDGET_S") || !strings.Contains(src, "S1-SLOW-TURN") {
		t.Fatalf("s1 script must assert abort's preempt budget against a slow turn")
	}
	// r5: the slow turn is a DEDICATED always-slow mock whose duration
	// rides `kubectl set env` — no heredoc substitution (r4's class) and
	// no request-body dependence (r5's dispatch proved opencode's
	// provider request shape does not reliably carry the prompt text).
	if !strings.Contains(src, "S1_SLEEP_S") || !strings.Contains(src, "set env deployment/mock-llm-s1-slow") {
		t.Fatalf("the slow turn must be env-injected via kubectl set env on a dedicated mock")
	}
	if strings.Contains(src, "S1-SLOW-TURN (") {
		t.Fatalf("no request-body slow-mode parsing — the provider request shape is not ours to depend on")
	}
	// r4: the row must carry its own discriminator — the IN-FLIGHT
	// assertion (the send's completion log still absent when the abort
	// returns). Without it a no-op abort against a crashed turn passes.
	if !strings.Contains(src, "S1B_SEND_LOG") || !strings.Contains(src, "S1B_INFLIGHT") {
		t.Fatalf("s1 script must assert the send is still in flight when the abort returns")
	}
	// r4: the mock self-check — the row that would have exposed the
	// broken slow mode in one dispatch.
	if !strings.Contains(src, "mock self-check") {
		t.Fatalf("s1 script must self-check the mock's slow mode before any row relies on it")
	}
	// r4: a read failure is NOT settled — the settle loop must require a
	// 2xx read with a parsed status (the || true pipe false-passed
	// wedged/unreachable pods).
	if !strings.Contains(src, `S1B_CODE}" == "200"`) {
		t.Fatalf("s1 script's settle loop must distinguish read failures from settled statuses")
	}
	// The delete/rename rows' ground truth is AGENT-side (an API-side
	// assert would be circular — the write and the read share the regime).
	if !strings.Contains(src, "agent_get_session_code") || !strings.Contains(src, `.title`) {
		t.Fatalf("s1 script must assert delete/rename outcomes agent-side")
	}
	// The unhappy rows pin the definitive-failure 502 codes with raw-body
	// diagnostics, and the abort no-op's 204 (harness parity — the V1
	// abort route 200s unknown sessions, so flag-off answers 204 for the
	// same bytes).
	if !strings.Contains(src, `"${S1E_SEND}" == "502"`) || !strings.Contains(src, `"${S1E_ABORT}" == "204"`) {
		t.Fatalf("s1 script must pin the dead-session codes (send/delete 502, abort 204)")
	}
	// S1b's settle row must assert the not-wedged property via the
	// platform's own session read, and a read FAILURE must not count as
	// settled (r4: the || true pipe false-passed wedged/unreachable pods).
	if !strings.Contains(src, "wedged") || !strings.Contains(src, `.status // "absent"`) || !strings.Contains(src, "READ FAILURE IS NOT SETTLED") {
		t.Fatalf("s1 script must assert the post-abort settle as not-wedged via a 2xx platform session read")
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
	cmd := exec.Command(bash, "-c", "set -euo pipefail\n"+block+"\necho ok=${S1B_IDLE_BUDGET_S} abort=${S1B_ABORT_BUDGET_S} slow=${S1B_SLOW_TURN_S}")
	cmd.Env = []string{} // ambient env must not mask the defaults (r4)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("budget defaults fail under set -euo pipefail: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "ok=") {
		t.Fatalf("budget defaults did not evaluate: %s", out)
	}
	// The strict-below budget is the discriminator's arithmetic — pin
	// it, not just the substrings' presence (r4).
	var idle, abort, slow int
	if _, err := fmt.Sscanf(string(out), "ok=%d abort=%d slow=%d", &idle, &abort, &slow); err != nil {
		t.Fatalf("could not parse budget echo %q: %v", out, err)
	}
	if abort >= slow {
		t.Fatalf("S1B_ABORT_BUDGET_S (%d) must be strictly below S1B_SLOW_TURN_S (%d) — the preempt discriminator's arithmetic", abort, slow)
	}
	if idle < slow {
		t.Fatalf("S1B_IDLE_BUDGET_S (%d) must cover the turn's own duration (%d)", idle, slow)
	}
}

func TestEpic71S1Script_ShellcheckUnbound(t *testing.T) {
	// Pool r6 died at runtime on an unbound variable (a leftover MOCK_SVC
	// reference after the two-mock rework — bash -n is blind to it and
	// each pool cycle costs ~40min). shellcheck's SC2154 catches the
	// class statically; CI runners ship it, this box may not (skip then).
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not on PATH — CI runs this row with it preinstalled")
	}
	out, err := exec.Command(sc, "-S", "error", "-s", "bash", epic71S1Script).CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck found error-level defects (unbound vars among them):\n%s", out)
	}
}

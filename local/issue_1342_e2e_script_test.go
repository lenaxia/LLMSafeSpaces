// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1342_e2e_script_test.go — structural pins for the #1342 cluster
// e2e (local/issue-1342-graceful-restart-e2e.sh), same philosophy as
// us70_harness_script_test.go: the script is CI glue on a real kind
// cluster; what is pinnable deterministically is the structure past
// failures actually broke — bash syntax, the row set, and the presence
// of the assertions so rows cannot be silently dropped.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1342Script = "issue-1342-graceful-restart-e2e.sh"

func TestIssue1342E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1342Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1342E2EScript_RowsAndAssertions pins the two rows and the
// assertions the issue's E2E plan demands: the incident replay (no
// force-kill while streaming + CredentialsApplyPending surfaced + apply
// via restart + no eternal spinner) and the kill-9 backstop (sweep +
// transcript repair + the orphan metric).
func TestIssue1342E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1342Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// Harness init (the #1478 r1 class: without harness_start the
		// script aborts at seed_workspace's OWNER_ID guard — R0-fatal
		// on every cluster).
		`harness_start`,
		// R1 — incident replay.
		`bind_env "${WS}"`,                      // credential change lands mid-turn
		`CredentialsApplyPending`,               // operator-visible defer surface
		`"$(opencode_pid)" == "${PID_BEFORE}"`,  // streaming turn not force-killed
		`running_tool_parts '"${SID1}"') -eq 0`, // no eternal spinner in served history
		// R2 — kill-9 backstop.
		`kill -9 $(pgrep -f "opencode serve")`,      // the orphaning event
		`harness_restarted_parts "${SID2}")" -ge 1`, // transcript repair closes the part
		`llmsafespaces_orphan_parts_aborted_total`,  // the sweep is observable
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}

	// The harness-restart reason the script asserts must match the
	// contract constant (orphan_reason_pin_test.go pins the Go side).
	assert.Contains(t, src, `"harness restart"`)
}

// TestIssue1342E2EWorkflowRegistered pins the nightly workflow row so
// the script cannot rot unexecuted.
func TestIssue1342E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	assert.True(t, strings.Contains(src, "local/issue-1342-graceful-restart-e2e.sh"),
		"the #1342 e2e script must be registered in the nightly workflow")
}

// TestIssue1342E2EWorkflow_LLMCredsGate pins run-35564258221's
// adjudication: the #1342 rows are real-LLM rows (streaming tool turns)
// and the nightly has no LLM creds configured — without a gate both rows
// burn 120s each timing out ("tool never reached running state") and
// block the downstream arbitration. The gate must be LOUD (a named
// SKIP with the config keys, F8's SKIP-on-absent pattern), never a
// silent step-level if — and it must actually evaluate the SECRET
// (wired into the step env; us-63's `env.LLM_API_KEY` gate reads an
// env var nothing populates at that scope — separate latent defect,
// noted for its owner, not fixed here).
func TestIssue1342E2EWorkflow_LLMCredsGate(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	start := strings.Index(src, "Run graceful-restart e2e rows")
	require.Greater(t, start, 0, "the #1342 step must exist in the nightly workflow")
	end := strings.Index(src[start:], "\n      - name: ")
	step := src[start : start+end]

	assert.Contains(t, step, "LLM_API_KEY: ${{ secrets.LLM_API_KEY }}",
		"the step must wire the secret into its env — a gate on env.LLM_API_KEY alone reads a var nothing populates at that scope")
	assert.Contains(t, step, `if [[ -z "${LLM_API_KEY:-}" ]]`,
		"the step must guard in-run (loud), not via a silent step-level if")
	assert.Contains(t, step, "SKIP: LLM_API_KEY not configured",
		"the skip must be loud and name the reason")
	assert.Contains(t, step, "secrets.LLM_API_KEY (+ vars.E2E_LLM_MODEL)",
		"the skip must name the config keys that restore coverage")
	assert.Contains(t, step, "bash local/issue-1342-graceful-restart-e2e.sh",
		"the guarded path must still run the script when creds are present")
}

// TestIssue1342E2EWorkflow_GateExecutes runs the REAL gate guard with
// the creds unset: it must SKIP loudly with exit 0 (downstream suites
// proceed), exactly as the F8 SKIP-on-absent leg does.
func TestIssue1342E2EWorkflow_GateExecutes(t *testing.T) {
	bash := requireBash(t)
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	start := strings.Index(src, "Run graceful-restart e2e rows")
	end := strings.Index(src[start:], "\n      - name: ")
	body := src[start : start+end]
	runAt := strings.Index(body, "run: |")
	require.Greater(t, runAt, 0, "the step must have a run: | block")
	script := regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(body[runAt+len("run: |"):], "")
	guard := regexp.MustCompile(`(?s)if \[\[ -z "\$\{LLM_API_KEY:-\}" \]\].*?\nfi\n`).FindString(script)
	require.NotEmpty(t, guard, "the creds gate guard not found in the step body")
	out, err := exec.Command(bash, "-c", "set -u; unset LLM_API_KEY\n"+guard+"\necho PAST-GATE").CombinedOutput()
	require.NoError(t, err, "the unset-creds leg must exit clean (downstream proceeds)")
	assert.Contains(t, string(out), "SKIP: LLM_API_KEY not configured")
	assert.NotContains(t, string(out), "PAST-GATE", "the unset-creds leg must not fall through to the script")

	// The SET-creds leg (r1's gap): the REAL full step body runs with a
	// stubbed bash on PATH — the guard must fall through and invoke the
	// script (an unconditional-exit regression silently retires the rows).
	dir := t.TempDir()
	rec := filepath.Join(dir, "bash-argv")
	stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > " + rec + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bash"), []byte(stub), 0o755))
	out, err = exec.Command(bash, "-c", "set -u; export PATH="+shQuote(dir)+":$PATH LLM_API_KEY=dummy-creds\n"+script).CombinedOutput()
	require.NoError(t, err, "the set-creds leg must fall through clean, got:\n%s", out)
	argv, rerr := os.ReadFile(rec)
	require.NoError(t, rerr, "the script must have been invoked — an unconditional exit retired the rows silently")
	assert.Contains(t, string(argv), "local/issue-1342-graceful-restart-e2e.sh",
		"set creds must run the #1342 script")
}

// TestIssue1342E2EScript_WorkspaceIDCanonical pins the script's
// UNCONDITIONAL WS_BASE (the us-70-revisions r21 pattern — the lib
// shadows any :- default at source time, so the unconditional literal
// is the only LIVE per-script prefix): ws_id (base[:32] + 4-digit
// suffix) must construct canonical 8-4-4-4-12 UUIDs or PostgreSQL
// rejects the seed insert. Same shape as
// TestIssue1455E2EScript_WorkspaceIDCanonical.
func TestIssue1342E2EScript_WorkspaceIDCanonical(t *testing.T) {
	raw, err := os.ReadFile(issue1342Script)
	require.NoError(t, err)
	// Assert ALL matches, not just the first: bash honors the LAST
	// assignment, so a shadowing second line must not slip past the pin.
	matches := regexp.MustCompile(`(?m)^WS_BASE="([0-9a-f-]+)"$`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, matches, "unconditional WS_BASE assignment not found — the script's shape drifted (a :- default is dead post-source)")

	for _, m := range matches {
		base := m[1]
		for _, sfx := range []string{"0001", "0002", "9999", "1234"} {
			id := base[:32] + sfx
			assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, id,
				"ws_id(%q) output must be a canonical UUID or PostgreSQL rejects the seed insert", base)
		}
	}
}

// TestIssue1342E2EScript_JqFiltersCompile compiles every jq program the
// script embeds (the provenance comment's mirror ask; the 1452-r2
// incident class: an unbalanced filter aborts the script under set -e
// exactly when the fix works). jq exit 3 is the compile error; runtime
// errors do not matter — this pin only proves the programs parse. Same
// shape as TestIssue1452E2EScript_JqFiltersCompile.
func TestIssue1342E2EScript_JqFiltersCompile(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not on PATH — CI runs this row with it preinstalled")
	}
	raw, err := os.ReadFile(issue1342Script)
	require.NoError(t, err)
	joined := strings.ReplaceAll(string(raw), "\\\n", "")

	singleQuoted := regexp.MustCompile(`jq\s+(?:-[a-zA-Z]+\s+|--arg(?:json)?\s+\w+\s+"[^"]*"\s+)*'([^']+)'`)
	doubleQuoted := regexp.MustCompile(`jq\s+(?:-[a-zA-Z]+\s+)*"((?:[^"\\]|\\.)*)"`)

	var programs []string
	for _, m := range singleQuoted.FindAllStringSubmatch(joined, -1) {
		programs = append(programs, m[1])
	}
	for _, m := range doubleQuoted.FindAllStringSubmatch(joined, -1) {
		unescaped := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1])
		dup := false
		for _, p := range programs {
			if p == unescaped {
				dup = true
				break
			}
		}
		if !dup {
			programs = append(programs, unescaped)
		}
	}
	require.NotEmpty(t, programs, "extraction found no jq programs — the regexes drifted from the script's quoting style")

	// Stub every $variable any filter references; unused --arg stubs are
	// harmless, a missing one is itself a compile error jq reports.
	stubs := []string{"-n", "--arg", "s", "x", "--arg", "t", "x", "--arg", "w", "x", "--arg", "p", "x", "--arg", "phase", "x", "--arg", "m", "x", "--arg", "id", "x"}
	for _, program := range programs {
		cmd := exec.Command(jq, append(stubs, program)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
				t.Fatalf("jq program does not compile (the 1452-r2 class): %q: %s", program, out)
			}
		}
	}
}

// TestIssue1342E2EScript_ExecuteSmoke — same class as the repo-wide
// smokes (Refs #1474/#1480/#1482); sequenced after #1478's harness_start
// per orchestrator. The script's wait budgets ride the env knobs the
// script now exposes (defaults unchanged — the knobs exist for smoke
// speed only). Depth pin scope, exactly as far as it reaches: under the
// shims R1's first wait fails, so R1's inner block (bind/restart/repair
// rows) is structurally SKIPPED — the pin proves R0 green, R1's first
// wait row (fail path), and R2's setup executed; R1's deeper rows stay
// covered by the structural needles in RowsAndAssertions.
func TestIssue1342E2EScript_ExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smoke spawns ~hundreds of shim processes")
	}
	knobs := map[string]string{
		"LLM_MODEL":      "litellm/smoke-model",
		"R1_TOOL_WAIT_S": "1", "R1_RESTART_WAIT_S": "1", "R1_ORPHAN_WAIT_S": "1",
		"R2_TOOL_WAIT_S": "1", "R2_RESPAWN_WAIT_S": "1", "R2_REPAIR_WAIT_S": "1",
		"R1_SLEEP_S": "1",
	}
	combined, exitVal := runScriptUnderShims(t, "issue-1342-graceful-restart-e2e.sh", "Active", knobs)
	assertSmokeTraversal(t, "issue-1342-graceful-restart-e2e.sh", combined, exitVal,
		"failure(s)", "")
	if !strings.Contains(combined, "R2: tool never reached running state") {
		t.Fatalf("died before the pinned R2 depth — a shared lib or shim rule regression moved the death point:\n%s", smokeTail(combined))
	}
}

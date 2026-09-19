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
// speed only). Depth pin: the R2 row's wait ran, proving R1's rows and
// R2's setup executed under the shims.
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

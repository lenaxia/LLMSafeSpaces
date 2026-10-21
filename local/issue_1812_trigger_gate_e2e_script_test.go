// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1812_trigger_gate_e2e_script_test.go — structural pins for the
// #1812 trigger inline-gate-script e2e
// (local/issue-1812-trigger-gate-e2e.sh), the issue_1410/
// issue_1342 philosophy: the script is CI glue on the nightly kind
// cluster; what is pinnable deterministically is the structure past
// failures actually broke — bash syntax, the row set, and the presence
// of the assertions so rows cannot be silently dropped.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1812Script = "issue-1812-trigger-gate-e2e.sh"

func TestIssue1812E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1812Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1812E2EScript_RowsAndAssertions pins the rows and their key
// assertions: the self-teaching create rejections (R0), the closed-gate
// skip with the audit payload (R1), the open gate's delivered fire +
// workspace side effect (R2), fail-closed on script failure (R3), and
// the prompt-only backward-compat row (R4). Every execution row carries
// the zero-sessions no-LLM-call assertion; the adaptive sidecar arm
// (#1455) is pinned so the nightly's expected arm is a named outcome,
// not a silent pass.
func TestIssue1812E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1812Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// R0 — create-path validation (self-teaching 400s).
		`R0a: script + workflowId rejected naming script/routine`,
		`R0b: unsupported language rejected naming python, node`,
		`R0c: partial script rejected naming handler`,
		// R1 — the closed gate: skipped fire + gate payload + no LLM.
		`script:{language:"python",`,
		"\\\"proceed\\\": False",
		`R1 (exec mode): skipped fire with {gate:{proceed:false,vars}} in the audit`,
		`R1 (sidecar mode): fail-closed with the named script_env_unavailable cause`,
		`assert_no_sessions "R1"`,
		// R2 — the open gate on a script-only trigger: delivered +
		// vars + in-sandbox side effect, still no LLM leg.
		`issue-1812-marker`,
		"\\\"proceed\\\": True",
		`R2 (exec mode): delivered fire with {gate:{proceed:true,vars}}`,
		`side-effect marker present in the workspace`,
		`assert_no_sessions "R2"`,
		// R3 — fail-closed.
		`return 1 / 0`,
		`R3: fail-closed failed fire with the named gate error`,
		`assert_no_sessions "R3"`,
		// R4 — prompt-only unchanged.
		`R4: prompt-only create still works`,
		// The no-LLM evidence helper itself (the core pin's mechanism).
		`expected ZERO agent sessions`,
	} {
		assert.Contains(t, src, needle)
	}

	// The gate payload assertions read the audit's actionResult.gate —
	// the self-explaining-fire contract — not just statuses.
	assert.Contains(t, src, `.actionResult.gate.`)
	// Both gate outcomes' payloads are asserted by field.
	assert.True(t, strings.Contains(src, `gate_field proceed`) && strings.Contains(src, `gate_field vars`),
		"gate payload fields (proceed/vars) must be asserted, not just fire status")
}

// TestIssue1812E2EScript_GateFieldEvaluatesFalse pins the jq extractor
// itself against real jq (the r2 review's Finding 1: `// empty` swallows
// false, making the closed-gate assertion unreachable). A string-only
// structural needle cannot catch this class — the pin must EVALUATE the
// expression. Skips only where jq is unavailable (not on the nightly).
func TestIssue1812E2EScript_GateFieldEvaluatesFalse(t *testing.T) {
	raw, err := os.ReadFile(issue1812Script)
	require.NoError(t, err)
	src := string(raw)
	require.Contains(t, src, `| tostring`, "gate_field must not use jq's falsy-swallowing `// empty`")

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed — the nightly and dev pods carry it")
	}
	// The exact expression shape gate_field builds, evaluated against a
	// closed-gate audit payload.
	expr := `.actionResult.gate.proceed | tostring`
	cmd := exec.Command("bash", "-c", `printf '%s' '{"actionResult":{"gate":{"proceed":false,"vars":{"topic":"nothing-new"}}}}' | jq -r '`+expr+`'`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "jq evaluation failed: %s", string(out))
	require.Equal(t, strings.TrimSpace(string(out)), "false",
		"a closed gate's proceed MUST render as the string false — the R1 exec-mode assertion depends on it")
}

// TestIssue1812E2EWorkflowRegistered pins the nightly workflow row so
// the script cannot be dropped from the schedule silently (the #1452
// lesson: an unregistered harness never runs; found only in review).
func TestIssue1812E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	assert.Contains(t, string(raw), issue1812Script,
		"the #1812 gate e2e must stay registered in the nightly workflow")
}

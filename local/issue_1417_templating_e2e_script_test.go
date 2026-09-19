// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1417_templating_e2e_script_test.go — structural pins for the
// #1417 templating e2e (local/issue-1417-templating-e2e.sh), same
// philosophy as issue_1410_automation_e2e_script_test.go: the script
// is CI glue on the kind cluster; what is pinnable deterministically
// is the structure past failures actually broke — bash syntax, the
// row set, and the presence of the assertions so rows cannot be
// silently dropped.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1417Script = "issue-1417-templating-e2e.sh"

func TestIssue1417E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1417Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

func TestIssue1417E2EScript_RowAssertionsPresent(t *testing.T) {
	raw, err := os.ReadFile(issue1417Script)
	require.NoError(t, err)
	s := string(raw)

	// Both rows and their discriminative assertions must exist.
	for _, want := range []string{
		"{{.body.topic}}",                         // the nested-ref prompt shape under test
		"PROMPT-WAS: e2e-nested-topic",            // T1 happy: rendered value asserted
		"grep -q \"{{.missing.path}}\"",           // T2 unhappy: literal fallback asserted
		"registry_admits",                         // the stub model must be admitted before the turn
		"rollout status deployment/mock-llm-1417", // the mock upstream must be up
		"targetWorkspaceId",                       // the workflow targets the seeded workspace
		// #1446 rows (T3-T6) — pinned so none can be silently dropped.
		"fires endpoint answers for the owner", // T3: owner-shaped fires GET
		"foreign trigger UUID 404s",            // T4: the ownership guard
		"exposes .result (delivered",           // T5: capture-full content asserted
		"exposes its cause via .result",        // T6: failed routine's error visible
		"captureMode:\"full\"",                 // T5/T6 carry real capture triggers
	} {
		assert.Contains(t, s, want)
	}
	// The failure leg must not silently pass: failures gate the exit.
	assert.Contains(t, s, "note_fail")
	assert.Contains(t, s, `die "${failures} templating e2e row(s) failed"`)
	assert.NotContains(t, strings.ReplaceAll(s, "note_fail", ""), "TODO", "no stub rows")
}

// TestIssue1417E2EWorkflowRegistered pins the nightly workflow row so
// the script cannot rot unexecuted (the #1420 precedent).
func TestIssue1417E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	assert.True(t, strings.Contains(src, "local/issue-1417-templating-e2e.sh"),
		"the templating e2e script must be registered in the nightly workflow")
}

// TestIssue1417E2EScript_ExecuteSmoke — same class as the 1410 smoke
// (#1474 r4-r6): the script must traverse to its verdict gate under
// shims (harness_start's login/me/postgres surfaces answered, phase
// polls satisfied with Ready).
func TestIssue1417E2EScript_ExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smoke spawns ~hundreds of shim processes")
	}
	combined, exitVal := runScriptUnderShims(t, "issue-1417-templating-e2e.sh", "Ready", nil)
	assertSmokeTraversal(t, "issue-1417-templating-e2e.sh", combined, exitVal,
		"templating e2e row(s) failed", "issue-1417 templating e2e: all rows green")
}

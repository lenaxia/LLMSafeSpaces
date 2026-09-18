// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1452_e2e_script_test.go — structural pins for the #1452
// routine-session-index e2e (local/issue1452-routine-session-index-e2e.sh),
// same philosophy as issue_1410_automation_e2e_script_test.go: the script
// is CI glue on the nightly kind cluster; what is pinnable
// deterministically is the structure past failures actually broke — bash
// syntax, the row set, and the presence of the assertions so rows cannot
// be silently dropped.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1452Script = "issue1452-routine-session-index-e2e.sh"

func TestIssue1452E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1452Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1452E2EScript_RowsAndAssertions pins both rows and their key
// assertions: the PreserveAlways happy path (signed webhook fire →
// delivered fire with a captured session_id → the session appears in the
// platform session list with the trigger name as title) and the
// PreserveNever negative (delivered fire, ephemeral session deleted
// agent-side, no platform row).
func TestIssue1452E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1452Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// Shared plumbing: deterministic firing + the surface under test.
		`/rotate-secret`,                    // HMAC secret rotation endpoint
		`X-Hub-Signature-256: sha256=`,      // deliveries are HMAC-signed
		`/api/v1/workspaces/${WS}/sessions`, // the platform list route (the bug's surface)
		`preserveSession:$p`,                // preservation policy is parameterized per row
		`captureMode:"full"`,                // session_id must be capturable from the result
		// R1 — PreserveAlways happy path.
		`make_routine_trigger "e2e-1452-preserve-always" "always"`, // the row's trigger
		`select(.status=="delivered")`,                             // fire delivery asserted
		`.result.session_id // empty`,                              // captured session_id read
		`select(.id == $s)] | length`,                              // session present in platform list
		`R1c: preserved routine session appears in GET /workspaces/:id/sessions`,
		`R1d: indexed row carries the trigger name as title`,
		// R2 — PreserveNever negative.
		`make_routine_trigger "e2e-1452-preserve-never" "never"`, // the row's trigger
		`R2b: PreserveNever fire delivered`,                      // delivery asserted before the negative
		`R2c: ephemeral routine session`,                         // no-row assertion present
		`session list grew ${r2_before} -> ${r2_after}`,          // growth itself is the failure text
		// Cleanup so the nightly owner's trigger list stays clean.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}
}

// TestIssue1452E2EWorkflowRegistered pins the nightly workflow row so the
// script cannot rot unexecuted.
func TestIssue1452E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	assert.True(t, strings.Contains(src, "local/issue1452-routine-session-index-e2e.sh"),
		"the #1452 routine-session-index e2e script must be registered in the nightly workflow")
}

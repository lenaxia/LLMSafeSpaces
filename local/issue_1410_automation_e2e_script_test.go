// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1410_automation_e2e_script_test.go — structural pins for the
// #1410/#1411/#1412/#1413 automation e2e
// (local/issue-1410-1412-automation-e2e.sh), same philosophy as
// issue_1342_e2e_script_test.go: the script is CI glue on the nightly
// kind cluster; what is pinnable deterministically is the structure past
// failures actually broke — bash syntax, the row set, and the presence of
// the assertions so rows cannot be silently dropped.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1410Script = "issue-1410-1412-automation-e2e.sh"

func TestIssue1410E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1410Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1410E2EScript_RowsAndAssertions pins the six rows and their
// key assertions: create validation + first-occurrence slot (R1),
// immediate reschedule (R2), no-op-enable slot stability (R3), the loud
// missing-workflow fire (R4), run-input schema enforcement (R5), and
// trigger input mapping (R6 — envelope-wiring guard, mapped static input,
// webhook body mode over the rotated HMAC secret).
func TestIssue1410E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1410Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// R1 — create-path validation (#1411).
		`"expr":"not-a-cron"`,                              // invalid expr attempted
		`R1a: invalid cron expr rejected`,                  // 400 asserted
		`R1b: nextFireAt is a future scheduled occurrence`, // first-slot, not creation time
		// R2 — reschedule moves the slot immediately (#1410).
		`"sourceConfig":{"expr":"0 4 1 * *","tz":"UTC"}`,     // the new schedule
		`03:00" ]] && [[ "$(slot_hm "${r2_new}")" == "04:00`, // old→new slot asserted
		// R3 — no-op enable keeps the slot (review guard on #1410).
		`'{"enabled":true}' >/dev/null`,
		`R3: no-op enabled:true kept the slot`,
		// R4 — missing workflow is loud (#1412).
		`GHOST_WF="deadbeef-0000-4000-8000-000000000000"`, // nonexistent DAG target
		`select(.status=="failed")`,                       // failed fire asserted
		`*"workflow not found"*`,                          // payload asserted
		`R4c: consecutiveFailures incremented`,            // failure counter asserted
		// R5 — run input obeys inputSchema (#1413).
		`'{"input":{"wrong":true}}' >/dev/null`,    // non-conforming input attempted
		`R5a: schema-violating run input rejected`, // 400 before queueing asserted
		`R5b: conforming input passed schema validation`,
		// R6 — trigger input mapping (#1425/#1419, design 0059).
		`R6a: envelope-mode wiring to required-topic schema rejected`, // wiring guard row
		`R6b: mapped input {} rejected`,                               // mapped static input validated (unhappy)
		`R6b: mapped input {topic:\"nightly\"} accepted`,              // mapped static input validated (happy)
		`inputFrom:"body"`,                                            // webhook body-mode wiring spelled
		`inputFrom:"mapped"`,                                          // mapped-mode wiring spelled
		`/rotate-secret`,                                              // HMAC secret rotation endpoint
		`X-Hub-Signature-256: sha256=`,                                // deliveries are HMAC-signed
		`.input.topic == "e2e"`,                                       // payload-as-top-level-run-input asserted
		`R6c: payload arrived as top-level run input`,                 // body-mode row
		`select(.status=="validation_error")`,                         // failed-fire status asserted
		`*"schema_mismatch"*`,                                         // actionResult code asserted (no instance echo)
		`R6c: violating payload queued no run`,                        // no run on schema mismatch
		`select(.status=="queued" or .status=="running")`,             // single-inflight drain-wait before each delivery
		`[[ -n "${r6b_id}" ]] && created_triggers+=("${r6b_id}")`,     // R6b-bad unexpected-success cleanup guard
		// Cleanup so the nightly owner's trigger list stays clean.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}

	// The rows must never depend on a pinned calendar date — the
	// monthly-slot assertions compare HH:MM, not "2026-10-01".
	assert.NotContains(t, src, "2026-10-01", "slot assertions must not rot on calendar dates")
}

// TestIssue1410E2EWorkflowRegistered pins the nightly workflow row so the
// script cannot rot unexecuted.
func TestIssue1410E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	assert.True(t, strings.Contains(src, "local/issue-1410-1412-automation-e2e.sh"),
		"the automation e2e script must be registered in the nightly workflow")
}

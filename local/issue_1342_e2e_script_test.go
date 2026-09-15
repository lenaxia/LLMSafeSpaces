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
	"os"
	"os/exec"
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

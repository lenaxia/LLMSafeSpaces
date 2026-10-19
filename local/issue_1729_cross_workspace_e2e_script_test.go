// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1729_cross_workspace_e2e_script_test.go — structural pins for
// the #1729 cross-workspace routine-trigger e2e
// (local/issue-1729-cross-workspace-automation-e2e.sh), same philosophy
// as issue_1410_automation_e2e_script_test.go: the script is CI glue on
// the nightly kind cluster; what is pinnable deterministically is the
// structure past failures actually broke — bash syntax, the row set,
// and the presence of the assertions so rows cannot be silently dropped.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1729Script = "issue-1729-cross-workspace-automation-e2e.sh"

func TestIssue1729E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1729Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1729E2EScript_RowsAndAssertions pins the rows and their key
// assertions: the cross-workspace happy create (R1), the disclosure 404
// (R2), the authorization 403 + nothing-persisted check (R3), the
// omitted-param compatibility create (R4), the workflowId+target mutual
// exclusion (R5), the create-only 400 on the REST update surface plus
// the ordinary-patch regression check (R6), and the org-route rows that
// authorize the ACTING user (R7 — the r1 review's BLOCKER fix, with its
// graceful platform-admin-gated SKIP).
func TestIssue1729E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1729Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// R1 — happy cross-workspace create.
		`R1a: cross-workspace create 201, routine target is the NAMED workspace`,
		`R1b: created trigger lists with the target workspace`,
		// R2 — the disclosure-policy 404.
		`R2: nonexistent target answers the named 404`,
		// R3 — the authorization 403 and its persistence check.
		`R3a: another user's workspace answers the named 403`,
		`R3b: rejected create persisted nothing`,
		// R4 — omitted-param compatibility.
		`R4: plain workspaceId create unchanged by #1729`,
		// R5 — workflow-targeting rules unchanged.
		`R5: workflowId + targetWorkspaceId answers the named 400`,
		// R6 — create-only on the REST update surface.
		`R6a: REST PATCH with targetWorkspaceId answers the shared create-only 400`,
		`R6b: ordinary PATCH still applies`,
		// R7 — org route authorizes the ACTING user (r1 fix) + fail-closed.
		`R7a: org-route create via the ACTING org admin targets the org workspace (r1 fix)`,
		`R7b: org-route create without target rights still 403s (fail-closed preserved)`,
		`SKIP R7: org creation is platform-admin gated`,
		// The gate must run against SEEDED rows (psql metadata seeding).
		`INSERT INTO workspaces`,
		// Cleanup so the nightly owner's trigger list stays clean.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}
}

// TestIssue1729E2EWorkflowRegistered pins the nightly workflow row so
// the script cannot rot unexecuted.
func TestIssue1729E2EWorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)
	assert.True(t, strings.Contains(src, "local/issue-1729-cross-workspace-automation-e2e.sh"),
		"the #1729 cross-workspace e2e script must be registered in the nightly workflow")
}

// TestIssue1729E2E_HarnessStartFirst pins the run-35586321658 lesson
// (the #1410 script's harness_start-first contract): every row's Bearer
// ${API_KEY} is unbound before harness_start establishes the forward and
// seeds the session.
func TestIssue1729E2E_HarnessStartFirst(t *testing.T) {
	raw, err := os.ReadFile(issue1729Script)
	require.NoError(t, err)
	src := string(raw)
	callIdx := strings.Index(src, "\nharness_start")
	require.GreaterOrEqual(t, callIdx, 0, "the script must call harness_start — nothing else establishes its port-forward or seeds the API_KEY its rows authenticate with")
	apiIdx := strings.Index(src, "api POST")
	if apiIdx >= 0 {
		assert.Greater(t, apiIdx, callIdx, "the first authenticated row must come after harness_start (API_KEY is unbound before it — set -u aborts)")
	}
}

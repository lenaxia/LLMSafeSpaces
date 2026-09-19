// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// issue_1455_e2e_script_test.go — structural pins for the #1455
// scriptenv e2e (local/issue-1455-scriptenv-e2e.sh), same philosophy as
// issue_1452_e2e_script_test.go: the script is CI glue on the nightly
// kind cluster; what is pinnable deterministically is the structure —
// bash syntax, the adaptive mode-contract row, and the presence of its
// assertions so the row cannot be silently dropped.

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const issue1455Script = "issue-1455-scriptenv-e2e.sh"

func TestIssue1455E2EScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", issue1455Script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

// TestIssue1455E2EScript_RowsAndAssertions pins the adaptive mode-
// contract row: a script-node workflow run must terminate as EITHER
// executed-with-marker (single-container) OR script_env_unavailable
// with the sidecar-naming detail (sidecar mode) — any other terminal
// shape fails the row.
func TestIssue1455E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1455Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// Shared plumbing: a real workspace pod behind the API.
		`seed_workspace "${WS}"`,
		`wait_phase "${WS}" Active 360`,
		// The script node under contract: python handler with a marker output.
		`\"language\":\"python\"`, // inside the jq -nc workflow body
		`e2e-1455-scriptenv-ran`,
		// The adaptive row's three arms.
		`R1a: single-container mode — script node executed post-EnvCheck`,
		`script_env_unavailable`, // the loud #1455 code is asserted by name
		`R1b: sidecar mode — script node failed LOUD`,
		`R1b: failed with script_env_unavailable but the detail does not name the sidecar cause`,
		// The nothing-between arm: any other terminal shape fails.
		`R1: terminal shape outside the mode contract`,
		// Terminal-state polling is bounded (no hang on a stuck run).
		`run never reached a terminal state within`,
		// Cleanup so the nightly owner's workflow list stays clean.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}
}

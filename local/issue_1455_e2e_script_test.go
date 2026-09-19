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
	"regexp"
	"strings"
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
		// Shared plumbing: harness init (r2 review: seed_workspace
		// hard-requires harness_start's OWNER_ID — omitting it aborts
		// at R0) and a real workspace pod behind the API.
		`harness_start`,
		`seed_workspace "${WS}"`,
		`wait_phase "${WS}" Active 360`,
		// The script node under contract: python handler with a marker output.
		`\"language\":\"python\"`, // inside the jq -nc workflow body
		`e2e-1455-scriptenv-ran`,
		// The adaptive row's three arms.
		`R1a: single-container mode — script node executed post-EnvCheck (marker present in the node output)`,
		`script_env_unavailable`, // the loud #1455 code is asserted by name
		`R1b: sidecar mode — script node failed LOUD`,
		`R1b: failed with script_env_unavailable but the detail does not name the sidecar cause`,
		// The nothing-between arm: any other terminal shape fails.
		`R1: terminal shape outside the mode contract`,
		// R1's output extraction (r4: the marker ships in every row's
		// specSnapshot — asserting it on the raw row is a tautology).
		`Assert the marker on the extracted .output`,
		// Terminal-state polling is bounded (no hang on a stuck run).
		`run never reached a terminal state within`,
		// R2 — http-node secrets live join: bind + materialize + echo.
		`bind_env "${WS}" "WT1455_PROBE_TOKEN" "sekret-1455-e2e"`,
		`secrets_converged "${WS}" 300`,
		`wait_env_present "${WS}" "WT1455_PROBE_TOKEN=sekret-1455-e2e" 300`,
		`Bearer {{secrets.WT1455_PROBE_TOKEN}}`,
		`R2a: http-node {{secrets.*}} resolved from the materialized secrets-env coordinate`,
		`R2b: unbound ref stayed literal in the echoed request (documented pass-through semantics)`,
		// The echoed-body extraction (r3: asserting the literal on the raw
		// run row is a tautology — the specSnapshot embeds it).
		`.output | if type == "string" then . else (.body // tostring) end`,
		// Cleanup so the nightly owner's workflow list + cluster stay clean.
		`trap cleanup EXIT`,
		`delete deployment/echo-1455 service/echo-1455 configmap/echo-1455-config`,
	} {
		assert.Contains(t, src, needle, "the e2e script must keep its row assertions (dropping one silently drops the row)")
	}
}

// TestIssue1455E2EScript_WorkspaceIDCanonical pins the script's
// UNCONDITIONAL WS_BASE (r2-of-#1478: the lib sets its own WS_BASE at
// source time, so a :- default here is dead code — the unconditional
// literal is the only LIVE per-script prefix): ws_id (base[:32] +
// 4-digit suffix) must construct canonical 8-4-4-4-12 UUIDs or
// PostgreSQL rejects the seed insert.
func TestIssue1455E2EScript_WorkspaceIDCanonical(t *testing.T) {
	raw, err := os.ReadFile(issue1455Script)
	require.NoError(t, err)
	// Assert ALL matches, not just the first: bash honors the LAST
	// assignment, so a shadowing second line must not slip past the pin.
	matches := regexp.MustCompile(`(?m)^WS_BASE="([0-9a-f-]+)"$`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, matches, "unconditional WS_BASE assignment not found — the script's shape drifted (a :- default is dead post-source: the lib shadows it)")

	for _, m := range matches {
		base := m[1]
		suffixes := []string{"0001", "0002", "9999", "1234"}
		for _, sfx := range suffixes {
			id := base[:32] + sfx
			assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`, id,
				"ws_id(%q) output must be a canonical UUID or PostgreSQL rejects the seed insert (the r3 R0-fatal class)", base)
		}
	}
}

// TestIssue1455E2EScript_JqFiltersCompile compiles every jq program the
// script embeds (the 1452 round-2 class: an unbalanced filter aborts
// the script under set -e exactly when the fix works). jq exit 3 is the
// compile error; runtime errors do not matter — this pin only proves
// the programs parse.
func TestIssue1455E2EScript_JqFiltersCompile(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not on PATH — CI runs this row with it preinstalled")
	}
	raw, err := os.ReadFile(issue1455Script)
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

	stubs := []string{"-n", "--arg", "r", "x", "--arg", "w", "x", "--arg", "url", "x"}
	for _, program := range programs {
		cmd := exec.Command(jq, append(stubs, program)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("jq program does not compile: %q: %s", program, out)
		}
	}
}

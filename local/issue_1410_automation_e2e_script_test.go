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

// TestIssue1410E2EScript_RowsAndAssertions pins the rows and their
// key assertions: create validation + first-occurrence slot (R1),
// immediate reschedule (R2), no-op-enable slot stability (R3), the loud
// missing-workflow fire (R4), run-input schema enforcement (R5), and
// trigger input mapping (R6 — envelope-wiring guard, mapped static input,
// webhook body mode over the rotated HMAC secret), the update-path
// memory/capture cross-constraint (R9 — #1467), and the update-path
// workflow-existence contract (R1d/R1d-happy — #1519: ghost retarget
// rejected with the named 400, retarget to an existing workflow accepted).
func TestIssue1410E2EScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(issue1410Script)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// R1 — create-path validation (#1411).
		`"expr":"not-a-cron"`,                                                            // invalid expr attempted
		`R1a: invalid cron expr rejected`,                                                // 400 asserted
		`R1b: nextFireAt is a future scheduled occurrence`,                               // first-slot, not creation time
		`R1c: ghost targetWorkspaceId workflow create rejected with the named 400`,       // the audit's headline, live-API (35617684178)
		`R1c: nonexistent workflowId trigger create rejected with the named 400`,         // the create contract, live-API (35597973572)
		`R1d: ghost targetWorkspaceId PATCH on the workflow rejected with the named 400`, // the audit's update face, live-API
		`R1d: ghost workflowId PATCH on the trigger rejected with the named 400 (#1519)`, // the #1519 update face, live-API
		`R1d-happy: PATCH retarget to existing workflow`,                                 // the #1519 update contract's accept arm, live-API
		`R1e: ghost workspaceId run override rejected with the named 400`,                // the run-override face (instance 4), live-API
		`R1f: org auto-apply ghost serverId answers the named 404`,                       // instance 5's reachable face, live-API
		`R1f: org bind ghost serverId answers the named 404`,                             // instance 5's bind face, live-API
		`R1g: org member add with ghost userId answers the named 404`,                    // instance 8, live-API
		`R1h: org bind with REAL server + ghost workspaceId answers workspace-not-found`, // instance 7 live, discriminating body
		`R1i: org credential auto-apply ghost credID answers the named 404`,              // the credential_auto_apply class, live
		// R2 — reschedule moves the slot immediately (#1410).
		`"sourceConfig":{"expr":"0 4 1 * *","tz":"UTC"}`,     // the new schedule
		`03:00" ]] && [[ "$(slot_hm "${r2_new}")" == "04:00`, // old→new slot asserted
		// R3 — no-op enable keeps the slot (review guard on #1410).
		`'{"enabled":true}' >/dev/null`,
		`R3: no-op enabled:true kept the slot`,
		// R4 — missing workflow is loud (#1412, reshaped per the
		// 35597973572 contract: ghost-workflow creates are a named 400,
		// so fire coverage rides create-valid → delete-workflow — the
		// #1440 FK-SET-NULL path); R4d — the DELETE route (#1440).
		`--arg w "${REAL_WF_ID}"`,                         // creates target the shared REAL workflow
		`api DELETE "/api/v1/me/workflows/${REAL_WF_ID}"`, // R4 deletes it before the fire window
		`select(.status=="failed")`,                       // failed fire asserted
		`*"trigger_has_no_target"*`,                       // payload asserted (delete route, #1440) — pinned twice: R4b and R4d
		"silent zombie regression",                        // R4d auto-disable assertion present
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
		// R9 — update-path memory/capture cross-constraint (#1467).
		`memoryMode:"last_result"`,                                 // the violating flip spelled (create + update rows)
		`R9a: create rejects last_result without full`,             // create-path constraint asserted
		`R9b: flip to last_result without full rejected`,           // merged-view rejection asserted
		`R9c: flip with full accepted and persisted`,               // happy flip + persistence asserted
		`R9d: narrowing capture under last_result rejected`,        // reverse-direction violation asserted
		`*"memoryMode 'last_result' requires captureMode 'full'"*`, // the shared constraint error asserted
		`R5_WS="00000000-0000-4000-8000-000000000001"`,             // R8's workspaceId source defined (was unbound → set -u abort)
		// R10 — drain-targetless via trigger patch (#1473): the drain
		// twin of R4d (retarget clears workspace_id while a webhook
		// routine fire pends; NULLIF store semantics).
		`R10: drain-targetless fire failed with trigger_has_no_target`, // row's ok assertion
		`*"trigger_has_no_target"*`,                                    // unified payload asserted
		`"workspaceId":""`,                                             // the retarget patch spelled
		`consecutiveFailures`,                                          // accounting asserted
		`lost the tick race twice`,                                     // retry guard present
		// R8 — org-scope CRUD resolves the resource segment (#1449).
		`ownerEmail:"e2e-automation@example.invalid"`, // org created; API-key user becomes admin
		`R8a: org trigger GET resolves the trigger`,   // the shadowing 404'd here
		`R8b: org trigger PUT resolves and mutates`,
		`R8c: org trigger fires route reachable`,
		`R8d: org trigger DELETE resolves`,
		`R8e: foreign-org trigger GET fails closed`, // unhappy leg
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

// TestIssue1410E2E_HarnessStartFirst pins run-35586321658's TRUE root
// cause (review r1's refutation of the livez-race theory): the step
// died in 18ms — ECONNREFUSED on a dead local port, because the script
// never established its port-forward AT ALL and its rows' Bearer
// ${API_KEY} was never seeded. Every row depends on harness_start (the
// #1452/#1417 pinned pattern): it establishes the forward, runs the
// 10×1s livez retry gate, and seeds the session user + API key.
func TestIssue1410E2E_HarnessStartFirst(t *testing.T) {
	raw, err := os.ReadFile(issue1410Script)
	require.NoError(t, err)
	src := string(raw)
	callIdx := strings.Index(src, "\nharness_start")
	require.GreaterOrEqual(t, callIdx, 0, "the script must call harness_start — nothing else establishes its port-forward or seeds the API_KEY its rows authenticate with (run 35586321658: 18ms ECONNREFUSED, forwardless)")
	livezIdx := strings.Index(src, "/livez")
	if livezIdx >= 0 {
		assert.Less(t, callIdx, livezIdx, "harness_start must precede any standalone /livez probe (it establishes the forward)")
	}
	// The first authenticated row call must come AFTER harness_start —
	// before it, API_KEY is unbound (set -u abort).
	apiIdx := strings.Index(src, "api GET")
	if apiIdx >= 0 {
		assert.Less(t, callIdx, apiIdx, "harness_start must precede the first api call — the rows' Bearer ${API_KEY} is seeded there")
	}
	// Ledger hardening (the #1514 adjudication note: ordering pins
	// anchored on Index-of-first are move-tolerant within a span): EVERY
	// authenticated api call must sit after harness_start — not just the
	// first — so harness_start can never be moved below an early row.
	off := 0
	for {
		j := strings.Index(src[off:], "api ")
		if j < 0 {
			break
		}
		pos := off + j
		if strings.HasPrefix(src[pos:], "api GET") || strings.HasPrefix(src[pos:], "api POST") ||
			strings.HasPrefix(src[pos:], "api PUT") || strings.HasPrefix(src[pos:], "api DELETE") {
			assert.Greater(t, pos, callIdx,
				"every authenticated api call must come after harness_start (found one at byte %d before it)", pos)
		}
		off = pos + 1
	}
	// The cleanup's DELETE calls also ride ${API_KEY}: the trap may fire
	// before harness_start completes, and ${API_KEY:-} guarding is the
	// lib's convention — pin that the trap tolerates the empty case.
	assert.Contains(t, src, "${API_KEY:-}", "the EXIT-trap cleanup must tolerate an unset API_KEY (die-before-bootstrap)")
}

// TestIssue1410E2EScript_ExecuteSmoke runs the script end-to-end under
// curl/sleep/kubectl shims and asserts it TRAVERSES to the final gate
// (die "N row(s) failed") instead of aborting on an unbound variable,
// a failed substitution, or a missing command. Source-text needles
// cannot catch runtime aborts — this smoke is what would have caught
// the r3/r4/r5 harness defects before review did. The shim surface is
// deliberately generic: rows whose assertions need specific responses
// note_fail, which is fine — reaching the verdict gate is the property
// under test. NOTE: the refactor onto the shared helpers WIDENED the
// acceptance set (green-marker + exit 0 is also accepted) and changed
// the shim surface (POST /runs→202, -o honored, auth answered) — not
// behavior-identical to the pre-refactor test. Shared machinery:
// e2e_smoke_helpers_test.go.
func TestIssue1410E2EScript_ExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smoke spawns ~hundreds of shim processes")
	}
	combined, exitVal := runScriptUnderShims(t, issue1410Script, "Active", nil)
	assertSmokeTraversal(t, issue1410Script, combined, exitVal,
		"row(s) failed", "automation e2e: all rows passed")
}

// TestIssue1410E2E_ArbitrationFinal5 pins the arbitration's final-five
// fixes (run 35752548377): the jq extraction uses -rc (jq -r pretty-prints
// objects — head -1 got just '{'), the R4d loop uses if/then (the
// banned [[ ]]&& break pattern), and R8 handles the admin-gated 403.
func TestIssue1410E2E_ArbitrationFinal5(t *testing.T) {
	raw, err := os.ReadFile(issue1410Script)
	require.NoError(t, err)
	src := string(raw)

	// 1) jq extraction: all actionResult pipelines use -rc (compact).
	badExtraction := strings.Count(src, `jq -r '.fires[] | select(.status=="failed") | .actionResult`)
	assert.Zero(t, badExtraction,
		"actionResult extraction must use jq -rc (compact) — jq -r pretty-prints the JSON object and head -1 returns just '{' (run 35752548377)")
	compactCount := strings.Count(src, `jq -rc '.fires[]`)
	assert.Greater(t, compactCount, 0, "at least one jq -rc extraction must exist")
	// R10's @tsv pipeline must tostring the actionResult member — @tsv
	// rejects objects regardless of -r/-rc (r1's no-op catch).
	assert.Contains(t, src, `(.actionResult // "") | tostring`,
		"R10's @tsv pipeline must tostring the actionResult — @tsv cannot serialize the JSON object the engine writes (engine.go:799)")

	// 2) R4d loop: no [[ ]] && break pattern in the file.
	assert.NotContains(t, src, `]] && break`,
		"the R4d loop must use if/then break — the [[ ]] && break pattern is banned (set -e fragility, the us70 pins)")

	// 3) R8: the 403 admin-gate is handled with a loud skip.
	assert.Contains(t, src, `api_status}" == "403"`,
		"R8 must handle the admin-gated 403 on org creation (the tenant-user choice)")
	assert.Contains(t, src, "org creation admin-gated",
		"the skip must be loud and name the reason")
}

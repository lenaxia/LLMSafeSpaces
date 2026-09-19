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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// memory/capture cross-constraint (R9 — #1467).
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
		// R4 — missing workflow is loud (#1412); R4d — the DELETE route
		// (FK SET NULL) is loud too (#1440).
		`GHOST_WF="deadbeef-0000-4000-8000-000000000000"`, // nonexistent DAG target
		`select(.status=="failed")`,                       // failed fire asserted
		`*"workflow not found"*`,                          // payload asserted (never-existed route)
		`*"trigger_has_no_target"*`,                       // payload asserted (delete route, #1440)
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

// TestIssue1410E2EScript_ExecuteSmoke runs the script end-to-end under
// curl/sleep/kubectl shims and asserts it TRAVERSES to the final gate
// (die "N row(s) failed") instead of aborting on an unbound variable,
// a failed substitution, or a missing command. Source-text needles
// cannot catch runtime aborts — this smoke is what would have caught
// the r3/r4 harness defects (verdict capture, api_status subshell
// death) before review did. The shim surface is deliberately generic:
// rows whose assertions need specific responses note_fail, which is
// fine — reaching the verdict line is the property under test.
func TestIssue1410E2EScript_ExecuteSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smoke spawns ~hundreds of shim processes")
	}
	shimDir := t.TempDir()

	curlShim := `#!/usr/bin/env bash
# Deterministic response surface: -w expansions honored like real curl.
method="GET"; wfmt=""; path=""
prev=""
for a in "$@"; do
  case "${prev}" in
    -X) method="${a}" ;;
    -w) wfmt="${a}" ;;
  esac
  case "${a}" in http://*) path="${a}" ;; esac
  prev="${a}"
done
body='{"id":"smoke","trigger":{"id":"smoke"},"name":"x","enabled":false,"nextFireAt":"2026-10-01T03:00:00Z","consecutiveFailures":1,"fires":[],"runs":[]}'
code=200
case "${path}" in
  */livez) body="ok" ;;
  */hooks/*) code=202 ;;
  */rotate-secret) body='{"webhookSecret":"whsec_smoke","webhookUrl":"/api/v1/hooks/smoke"}' ;;
  *)
    [[ "${method}" == "POST" ]] && code=201
    ;;
esac
if [[ -n "${wfmt}" ]]; then
  # The script passes -w '\n%{http_code}' as literal backslash-n (real
  # curl expands it); match the literal, emit a real newline.
  if [[ "${wfmt}" == '\n%{http_code}' ]]; then
    printf '%s\n%s' "${body}" "${code}"
  elif [[ "${wfmt}" == *'%{http_code}'* ]]; then
    printf '%s' "${code}"
  else
    printf '%s' "${body}"
  fi
else
  printf '%s' "${body}"
fi
exit 0
`
	sleepShim := "#!/usr/bin/env bash\nexit 0\n"
	kubectlShim := "#!/usr/bin/env bash\nexit 0\n"

	for name, body := range map[string]string{
		"curl": curlShim, "sleep": sleepShim, "kubectl": kubectlShim,
	} {
		p := filepath.Join(shimDir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o755))
	}

	cmd := exec.Command("bash", issue1410Script)
	cmd.Env = append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		combined := out.String()
		// The generic shim cannot satisfy row-specific assertions, so
		// failures accumulate and the final gate must fire.
		if err == nil {
			t.Fatalf("script must exit non-zero under the generic shim (silent pass?)\n%s", tailOf(combined))
		}
		if !strings.Contains(combined, "row(s) failed") {
			t.Fatalf("script never reached the final verdict gate — aborted mid-row:\n%s", tailOf(combined))
		}
		for _, banned := range []string{"unbound variable", "command not found", "permission denied", "substitution"} {
			if strings.Contains(combined, banned) {
				t.Fatalf("runtime abort signature %q found:\n%s", banned, tailOf(combined))
			}
		}
		// Credential hygiene: the rotate-secret response carries the
		// one-time webhook secret; nothing may echo it to the log
		// (r5 finding 3's class — pinned here so it cannot regress).
		if strings.Contains(combined, "whsec_") {
			t.Fatalf("webhook secret material leaked into script output:\n%s", tailOf(combined))
		}
	case <-time.After(180 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("script did not terminate within 180s (hung poll loop?)\n%s", tailOf(out.String()))
	}
}

func tailOf(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	return strings.Join(lines, "\n")
}

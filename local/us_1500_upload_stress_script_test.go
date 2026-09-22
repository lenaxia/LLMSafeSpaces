// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Structural pins for the design-0060 §6 stress harness — same
// philosophy as the other harness pin suites: bash syntax, the row set
// with its assertions (rows cannot be silently dropped), the skip-DOWN
// markers (loud, counted), and the §6.6 precondition math.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uploadStressScript = "us-1500-upload-stress-e2e.sh"

func TestUploadStressScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", uploadStressScript)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}

func TestUploadStressScript_RowsAndAssertions(t *testing.T) {
	raw, err := os.ReadFile(uploadStressScript)
	require.NoError(t, err)
	src := string(raw)

	for _, needle := range []string{
		// §6.1 measured residency — GAUGE pins, not code-path asserts.
		`workspace_agentd_upload_staging_bytes`,
		`workspace_agentd_upload_staging_reserved_bytes`,
		`≤ 48 MiB budget`,
		// §6.2 isolation.
		`workspace_agentd_upload_staging_credential_bytes`,
		`credential_bytes never regressed`,
		// §4.1/§4.6 API-side rows shipped with the forwarding PR.
		`undeclared multipart body → 411`,
		`invalid_declared_length`,
		// §6.4 write-time edge.
		`507 dest_disk_full`,
		// §6.5 kills: partial-visibility + destination .tmp reclaim.
		`ls /workspace/uploads/*.tmp`,
		`destination .tmp bounded`,
		// §6.6 precondition + EXPLICIT skip-DOWN (never silent 3×).
		`skip-DOWN to 3×, explicitly`,
		`98566144`,
		// The loud-skip convention for not-yet-landed mechanisms.
		`SKIP-DOWN`,
		// The #1474-r4 no-subshell api() contract.
		`api_status="${out##*$'\n'}"`,
		`api_body="${out%$'\n'*}"`,
		// SR-2: correct route (r2 finding 4: /me/workspaces/ → 404).
		`/api/v1/workspaces/${WS}/reload-secrets`,
		// SR-5: 502 in the terminal set (r2 finding 5) + the §6.5
		// PRIMARY invariant (r3 finding 3: non-.tmp listing comparison).
		`"${st}" == "502"`,
		`PRE_KILL_LISTING`,
		`POST_KILL_LISTING`,
		`comm -13`,
		`awk 'NF{n++} END{printf "%d", n+0}'`,
		`non-.tmp partials`,
		// SR-2: the route-FIRED check (r3 finding 2).
		`reload-secrets returned ${api_status}, expected 200`,
		// SR-6: refused COUNT parsed + the regression guard AS AN
		// ASSERTION (r3 findings 1+4).
		// storm_report completeness (r3 finding 5).
		`total=%d`,
		// SR-5: the honest bound — per-phase 201 tracking (r5).
		`SR5_PHASE_STATUSES`,
		`SR5_DELIVERED`,
		// SR-5: post-kill listing exec-failure guard (r5).
		`POST_KILL_EXIT`,
		// SR-6: per-upload max(p95@N≤4) guard — the design's §6.6 quantity
		// (r5: the wall-clock form was conditionally vacuous).
		`SR6_GUARD=$((2 * L1))`,
		`sort -n | awk`,
		`max(p95@N`,
		`SR6_P95`,
		`ms-${i}`,
		// SR-6B: the literal-429 construct + the precondition gate.
		`SR6B_HAS_429`,
		`-eq 4 ]]; then`,
		// Cleanup.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the stress harness must keep its row assertions")
	}

	// The SR-3 backpressure row must be a LOUD skip until the fault
	// seam lands — never silently dropped.
	assert.Contains(t, src, `SR-3: copy-throttle injection absent`, "SR-3 skip must stay explicit")
}

// TestUploadStressScript_GuardBehavioralTest verifies the guard's
// compute logic against synthetic timing files, driving the SCRIPT'S
// OWN pipeline extracted at test runtime — a script-side regression
// (the r7 max-labeled median mutant) fails here.
func TestUploadStressScript_GuardBehavioralTest(t *testing.T) {
	raw, err := os.ReadFile(uploadStressScript)
	require.NoError(t, err)
	src := string(raw)
	assert.Contains(t, src, "sort -n | awk", "the guard pipeline must stay in the script")

	// Extract the LIVE pipeline from the script (r9 finding: a frozen
	// copy ships green through a script-side regression — the r7 max-
	// labeled median mutant passes the frozen test).
	awkIdx := strings.Index(src, "sort -n | awk")
	require.Greater(t, awkIdx, 0, "guard pipeline not found in script")
	bodyEnd := strings.Index(src[awkIdx:], "}}'")
	require.Greater(t, bodyEnd, 0, "guard awk body terminator not found")
	awkWithSort := src[awkIdx : awkIdx+bodyEnd+3]
	// Strip the leading sort (the test pipes into the awk part only).
	barIdx := strings.Index(awkWithSort, "|")
	awkOnly := strings.TrimSpace(awkWithSort[barIdx+1:])

	tests := []struct {
		name      string
		timings   string
		single    int
		wantTrips bool
	}{
		{"healthy concurrent: max == single", "100\n100\n100\n100\n", 100, false},
		{"healthy with overhead: max 1.5x single", "100\n150\n150\n150\n", 100, false},
		{"at boundary: max == 2x single (passes: <=)", "200\n200\n200\n200\n", 100, false},
		{"serialized N=3: max 3x single", "100\n200\n300\n", 100, true},
		{"serialized N=4: max 4x single", "100\n200\n300\n400\n", 100, true},
		{"partial serialization at N=4", "100\n150\n250\n300\n", 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, ms := range strings.Split(strings.TrimSpace(tt.timings), "\n") {
				_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("ms-%d", i+1)), []byte(ms+"\n"), 0o644)
			}
			cmd := exec.Command("bash", "-c",
				"cat "+filepath.Join(dir, "ms-*")+" 2>/dev/null | sort -n | "+awkOnly)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "out: %s", string(out))
			got, err := strconv.Atoi(strings.TrimSpace(string(out)))
			require.NoError(t, err, "parsing %q", string(out))
			guard := 2 * tt.single
			trips := got > guard
			assert.Equal(t, tt.wantTrips, trips,
				"max=%d, guard=%d: expected trips=%v", got, guard, tt.wantTrips)
		})
	}
}

func TestUploadStressScript_WorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), "local/"+uploadStressScript),
		"the stress harness must be registered in the nightly workflow")
}

// TestUploadStress_SR6DeliveryGate pins run 35679282297's adjudication:
// SR-6 must gate IN-RUN on the baseline upload's 507 (the half-stack's
// DESIGNED clean-fail — the delivery leg #1518/#1524 is absent). Keyed
// on 507 SPECIFICALLY so genuine 500/000/429 baselines reach the
// failure path. The actual hang of that run was a BARE `wait` at the
// storm joins (also waiting the immortal port-forward child) — the
// per-pid-wait + bare-wait-absence pins guard that mechanism. NEVER a
// silent step-level `if: false` on the workflow (the #1342 rule).
func TestUploadStress_SR6DeliveryGate(t *testing.T) {
	raw, err := os.ReadFile(uploadStressScript)
	require.NoError(t, err)
	src := string(raw)
	assert.Contains(t, src, `if [[ "${L1_STATUS}" == "507" ]]`,
		"the gate must key on 507 (the DESIGNED clean-fail) — a genuine 500/000/429 baseline reaches the failure path, not the skip")
	assert.Contains(t, src, `sr_skip "SR-6: baseline upload`,
		"SR-6's 507 baseline must skip-DOWN loudly (the script's own idiom)")
	assert.Contains(t, src, "delivery leg #1518/#1524 absent",
		"the skip must name the missing activation (the stack's own PR refs)")
	assert.Contains(t, src, `die "upload stress harness: ${failures} row(s) failed`,
		"the gate's exit must propagate prior row failures (the verdict must not be bypassed)")
	// The bare-wait hang guard: the SR-6 storms must wait per-pid.
	perPid := strings.Count(src, `wait "${p}"`)
	assert.GreaterOrEqual(t, perPid, 4,
		"the storm joins must be per-pid waits (SR-1, SR-2, SR-6, SR-6B) — a bare `wait` also waits the immortal port-forward child (run 35679282297's actual hang mechanism)")
	assert.NotContains(t, src, "\nwait\n",
		"a bare `wait` waits the port-forward child spawned by harness_start — 36 minutes of silence until cancellation")

	wf, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	wfs := string(wf)
	assert.NotContains(t, wfs, "if: false  # TODO(#1524)",
		"the workflow must NOT silently disable the step — the #1342 rule: loud in-run gates, never silent step-level ifs")
	assert.Contains(t, wfs, "SR-6 skips-DOWN loudly",
		"the workflow comment must teach the in-run gate")
}

// TestUploadStress_SR6Gate_Executes runs the REAL gate + baseline
// blocks (extracted from the script) across four legs: 507-clean →
// skip + exit 0 (the half-stack's designed path); 507 + prior failure
// → die (the r1 masking guard); non-507 non-201 (500) → note_fail +
// falls through to the storms (the r2 phantom-fix guard); 201 → clean
// fall-through to the latency rows (the happy path). Mutation-verified:
// deleting the baseline assertion, removing the gate's die or exit,
// or reverting a storm join to bare wait each turns a leg red.
func TestUploadStress_SR6Gate_Executes(t *testing.T) {
	bash := requireBash(t)
	src, err := os.ReadFile(uploadStressScript)
	require.NoError(t, err)
	text := string(src)

	// Extract the gate + baseline-assertion block.
	gateStart := strings.Index(text, `if [[ "${L1_STATUS}" == "507" ]]`)
	gateEnd := strings.Index(text, "The concurrency boundary")
	require.Greater(t, gateStart, 0, "gate not found")
	require.Greater(t, gateEnd, gateStart, "gate end not found")
	block := text[gateStart:gateEnd]

	dir := t.TempDir()
	stub := "#!/bin/sh\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte(stub), 0o755))

	run := func(status string, failures int) (string, error) {
		script := "set -u; export PATH=" + shQuote(dir) + ":$PATH\n" +
			"L1_STATUS=" + status + "\n" +
			"failures=" + fmt.Sprintf("%d", failures) + "\n" +
			"sr_skips=0\n" +
			"note_fail() { failures=$((failures + 1)); echo \"NOTE_FAIL: $*\" >&2; }\n" +
			"sr_skip() { sr_skips=$((sr_skips + 1)); echo \"SKIP-DOWN: $*\" >&2; }\n" +
			"log() { echo \"LOG: $*\"; }\n" +
			"warn() { echo \"WARN: $*\" >&2; }\n" +
			"die() { echo \"DIE: $*\" >&2; exit 1; }\n" +
			block + "\necho AFTER-GATE\n"
		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		return string(out), err
	}

	t.Run("507 clean: skip + exit 0", func(t *testing.T) {
		out, err := run("507", 0)
		require.NoError(t, err, "507 with no prior failures must exit clean: %s", out)
		assert.Contains(t, out, "SKIP-DOWN: SR-6: baseline upload 507")
		assert.NotContains(t, out, "AFTER-GATE", "the 507 gate must exit, not fall through")
	})

	t.Run("507 + prior failure: dies", func(t *testing.T) {
		out, err := run("507", 2)
		require.Error(t, err, "prior failures must propagate, not be masked green")
		assert.Contains(t, out, "DIE: upload stress harness: 2 row(s) failed")
	})

	t.Run("500 (non-507 non-201): note_fail + falls through", func(t *testing.T) {
		out, err := run("500", 0)
		require.NoError(t, err, "the baseline assertion is a note_fail, not a die — must fall through")
		assert.Contains(t, out, "NOTE_FAIL: SR-6: single-upload baseline failed (500)")
		assert.Contains(t, out, "AFTER-GATE", "a non-507 failure must NOT take the gate exit — it reaches the storms")
	})

	t.Run("201: clean fall-through", func(t *testing.T) {
		out, err := run("201", 0)
		require.NoError(t, err)
		assert.Contains(t, out, "AFTER-GATE", "a 201 baseline proceeds to the latency rows")
		assert.NotContains(t, out, "SKIP-DOWN")
		assert.NotContains(t, out, "NOTE_FAIL")
	})
}

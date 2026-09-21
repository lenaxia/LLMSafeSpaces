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

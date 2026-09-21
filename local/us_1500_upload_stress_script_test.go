// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Structural pins for the design-0060 §6 stress harness — same
// philosophy as the other harness pin suites: bash syntax, the row set
// with its assertions (rows cannot be silently dropped), the skip-DOWN
// markers (loud, counted), and the §6.6 precondition math.

import (
	"os"
	"os/exec"
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
		// SR-5: 502 in the terminal set (r2 finding 5: transport is
		// terminal per design §6.5).
		`"${st}" == "502"`,
		// Cleanup.
		`trap cleanup EXIT`,
	} {
		assert.Contains(t, src, needle, "the stress harness must keep its row assertions")
	}

	// The SR-3 backpressure row must be a LOUD skip until the fault
	// seam lands — never silently dropped.
	assert.Contains(t, src, `SR-3: copy-throttle injection absent`, "SR-3 skip must stay explicit")
}

func TestUploadStressScript_WorkflowRegistered(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), "local/"+uploadStressScript),
		"the stress harness must be registered in the nightly workflow")
}

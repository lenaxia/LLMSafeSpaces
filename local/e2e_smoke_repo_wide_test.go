// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// Repo-wide ExecuteSmoke closure (Refs #1474 r6, #1480): every
// nightly-registered harness script runs under the shared shims and
// must produce a ROW VERDICT — the die marker (✗) with a non-zero
// exit, or its green gate — never a runtime abort (unbound variable,
// missing command) and never a webhook-secret leak. Under the generic
// shim surface these scripts die on semantic row assertions, which is
// exactly the deterministic outcome the smoke pins: the property under
// test is that the harness EXECUTES its rows, not that a fake cluster
// satisfies them. (1342 is sequenced after #1478 and is not covered
// here.) Constructing this suite surfaced one more corpse of the #1474
// r4 class — 1455 carried the same never-executable api() — fixed by
// applying the already-ruled no-subshell contract.

import (
	"testing"
)

func TestHarnessExecuteSmoke_RepoWide(t *testing.T) {
	if testing.Short() {
		t.Skip("execution smokes spawn ~hundreds of shim processes")
	}
	tests := []struct {
		name        string
		script      string
		phase       string
		greenMarker string
		extraEnv    map[string]string
	}{
		{"test.sh", "test.sh", "Active", "", nil},
		{"us-68 attachments", "us-68-attachments-e2e.sh", "Active", "all green", nil},
		{"us-70 secret delivery", "us-70-secret-delivery-e2e.sh", "Active", "all rows green", map[string]string{
			"SUSPEND_SECONDS": "1", "RESUME_SCALE": "1", "RESUME_SCALE_TIMEOUT_S": "5", "RECONCILE_INTERVAL_S": "1",
		}},
		{"1455 scriptenv", "issue-1455-scriptenv-e2e.sh", "Active", "all rows green", nil},
		{"us-70 revisions", "us-70-revisions-e2e.sh", "Active", "revisions suite complete", nil},
		{"dev-preview tunnel", "dev-preview-tunnel-e2e.sh", "Active", "ALL LEGS GREEN", nil},
		{"us-63 v2 behavior", "us-63-v2-behavior-e2e.sh", "Active", "all V2 behavioral assertions PASSED", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			combined, exitVal := runScriptUnderShims(t, tt.script, tt.phase, tt.extraEnv)
			assertSmokeTraversal(t, tt.script, combined, exitVal, "✗", tt.greenMarker)
		})
	}
}

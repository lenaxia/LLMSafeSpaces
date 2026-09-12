// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// epic71_3a_walkaway_script_test.go — pin tests for the epic-71 / 3a
// (#1313) walk-away cluster suite (local/epic71-3a-walkaway-e2e.sh) and
// its pool wiring, same philosophy as us70_revisions_script_test.go:
// the cluster rows cannot run without a cluster, but what is pinnable
// deterministically is the structure past failures actually broke —
// bash syntax, the lib sourcing, the distinct UUID base, the L7/S2/S11
// asserts, the whileAway discriminator, and the pool running the row
// BEFORE the fault seam is armed.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const epic71WalkawayScript = "epic71-3a-walkaway-e2e.sh"

func TestEpic71WalkawayScript_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", epic71WalkawayScript).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func TestEpic71WalkawayScript_SourcesCommonLibAndDistinctBase(t *testing.T) {
	src := mustRead(t, epic71WalkawayScript)
	if !strings.Contains(src, "lib/us70-common.sh") {
		t.Fatalf("%s must source lib/us70-common.sh (shared harness helpers)", epic71WalkawayScript)
	}
	if !strings.Contains(src, "e2e71000-0000-4000-8000-000000000000") {
		t.Fatalf("%s must keep its distinct UUID WS_BASE default (workspaces.id is a uuid column; sibling suites share the pool cluster)", epic71WalkawayScript)
	}
}

func TestEpic71WalkawayScript_RowPins(t *testing.T) {
	src := mustRead(t, epic71WalkawayScript)

	// L7: the re-presentation budget must be asserted, not eyeballed.
	if !strings.Contains(src, "L7_BUDGET_S") || !strings.Contains(src, "whileAway") {
		t.Fatalf("walk-away script must assert the L7 budget against a whileAway-tagged event")
	}
	// S2: the exactly-once evidence is the dedupe marker (set AFTER a
	// successful push) plus the duplicate re-POST's duplicate=true body —
	// NOT the queue depth, which the delivery worker drains
	// concurrently (queue→staging) by design.
	if !strings.Contains(src, "outboxdedupe") {
		t.Fatalf("walk-away script must assert the outbox dedupe marker (stable S2 evidence)")
	}
	if !strings.Contains(src, `// .duplicate // empty`) && !strings.Contains(src, ".duplicate // empty") {
		t.Fatalf("walk-away script must assert duplicate=true on the re-POST body")
	}
	// S11: dismiss must assert the terminal status and non-re-presentation.
	if !strings.Contains(src, `"dismissed"`) || !strings.Contains(src, `== "204"`) {
		t.Fatalf("walk-away script must assert the dismiss exit (204 + terminal dismissed status)")
	}
	// A3: the suspension variant must resume and re-assert re-presentation.
	if !strings.Contains(src, "wait_phase") || !strings.Contains(src, "Suspended") || !strings.Contains(src, "/activate") {
		t.Fatalf("walk-away script must include the suspend → resume → re-present variant")
	}
	// Discriminator: the seeded record must carry the exact store shape
	// (status pending, kind question) — a silent typo re-presents nothing
	// and the L7 assert would pass vacuously against an empty capture.
	if !strings.Contains(src, `kind:\"question\"`) && !strings.Contains(src, `kind:"question"`) {
		t.Fatalf("walk-away script seeds must carry kind=question (store shape)")
	}
	if !strings.Contains(src, `status:\"pending\"`) && !strings.Contains(src, `status:"pending"`) {
		t.Fatalf("walk-away script seeds must carry status=pending (store shape)")
	}
}

func TestEpic71WalkawayScript_PoolRunsBeforeFaultArm(t *testing.T) {
	pool := mustRead(t, filepath.Join("..", ".github", "workflows", "us-70-delivery-pool.yml"))
	if !strings.Contains(pool, epic71WalkawayScript) {
		t.Fatalf("us-70-delivery-pool.yml must run %s (the stream's merge gate)", epic71WalkawayScript)
	}
	armIdx := strings.Index(pool, "name: Arm fault seam")
	rowIdx := strings.Index(pool, epic71WalkawayScript)
	if armIdx >= 0 && rowIdx >= 0 && rowIdx > armIdx {
		t.Fatalf("%s must run BEFORE the fault seam is armed (delivery rows are seam-inert)", epic71WalkawayScript)
	}
}

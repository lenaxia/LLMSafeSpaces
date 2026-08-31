// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us70_revisions_script_test.go — pin tests for the US-70.2 revisions
// cluster suite (local/us-70-revisions-e2e.sh) and its workflow wiring,
// same philosophy as us70_harness_script_test.go: the cluster rows
// cannot run in CI-without-a-cluster, but what is pinnable
// deterministically is the structure past failures actually broke —
// bash syntax, the lib sourcing, the 304/ETag + monotonic-seq asserts,
// the two-replica port-forward row, the contract feature-detect skip,
// and the lockstep that BOTH workflows run the script (the pool one
// BEFORE the fault seam is armed).

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const us70RevisionsScript = "us-70-revisions-e2e.sh"

var us70NightlyWorkflow = filepath.Join("..", ".github", "workflows", "e2e-nightly.yml")

func TestUS70RevisionsScript_BashSyntax(t *testing.T) {
	bash := requireBash(t)
	out, err := exec.Command(bash, "-n", us70RevisionsScript).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func TestUS70RevisionsScript_SourcesCommonLib(t *testing.T) {
	src := mustRead(t, us70RevisionsScript)
	if !strings.Contains(src, "lib/us70-common.sh") {
		t.Fatalf("%s must source lib/us70-common.sh (shared harness helpers)", us70RevisionsScript)
	}
	// Distinct workspace prefix: the audit-truncation budget (≤33 chars)
	// collides with the sibling suites' workspaces otherwise.
	if !strings.Contains(src, "e2e57000-0000-4000-8000-000000000000") {
		t.Fatalf("%s must keep its distinct UUID WS_BASE default (workspaces.id is a uuid column — non-UUID names can never resolve)", us70RevisionsScript)
	}
}

func TestUS70RevisionsScript_RowPins(t *testing.T) {
	src := mustRead(t, us70RevisionsScript)

	// Conditional-pull row: status capture + the 304 asserts + ETag.
	if !strings.Contains(src, `'%{http_code}'`) {
		t.Fatalf("revisions script must capture HTTP status via %%{http_code}")
	}
	if strings.Count(src, `== "304"`) < 2 {
		t.Fatalf("revisions script must assert 304 at least twice (before and after the bind), found %d", strings.Count(src, `== "304"`))
	}
	if !strings.Contains(src, "ETag") || !strings.Contains(src, "etag_of") {
		t.Fatalf("revisions script must assert the 304 ETag header (extract via etag_of)")
	}

	// Seq monotonicity: the post-bind 200 must carry a strictly greater seq.
	if !strings.Contains(src, "(( REV2_SEQ2 > REV2_SEQ1 ))") {
		t.Fatalf("revisions script must pin the monotonic-mint assert (( REV2_SEQ2 > REV2_SEQ1 ))")
	}

	// Two-replica row: scale-out + the api-pod selector + per-pod
	// port-forwards on distinct local ports.
	for _, pin := range []string{
		"--replicas=2",
		"app.kubernetes.io/component=api",
		"18091",
		"18092",
		"sort_by(.type, .secretId)",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("revisions script must contain %q (two-replica identical-revision row)", pin)
		}
	}

	// Contract feature-detect: the v2 probe + loud-skip of ALL rows when
	// the deployed API predates the envelope (mixed fleet / pre-merge).
	for _, pin := range []string{
		"contractVersion:2",
		"clientManifestHash",
		".secrets.revision.seq",
		"for ROW in REV-1 REV-2 REV-3 REV-4",
		"skip_row",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("revisions script must contain %q (contract feature-detect / loud-skip)", pin)
		}
	}

	// Anchored spawned_rev row: 3 colon-separated components, seq prefix
	// via cut -d: -f1.
	for _, pin := range []string{
		"secretsDelivery.spawnedRev",
		"cut -d: -f1",
	} {
		if !strings.Contains(src, pin) {
			t.Fatalf("revisions script must contain %q (anchored spawned_rev row)", pin)
		}
	}
}

func TestUS70RevisionsScript_WorkflowLockstep(t *testing.T) {
	const scriptRef = "local/us-70-revisions-e2e.sh"

	nightly := mustRead(t, us70NightlyWorkflow)
	if !strings.Contains(nightly, scriptRef) {
		t.Fatalf("nightly workflow must run %s", scriptRef)
	}
	if !strings.Contains(nightly, "PORTFWD_PORT: 18084") {
		t.Fatalf("nightly workflow must pin the revisions step to PORTFWD_PORT: 18084 (distinct from the sibling suites' ports)")
	}

	pool := mustRead(t, us70PoolWorkflow)
	if !strings.Contains(pool, scriptRef) {
		t.Fatalf("pool workflow must run %s", scriptRef)
	}
	if !strings.Contains(pool, "PORTFWD_PORT: 18087") {
		t.Fatalf("pool workflow must pin the revisions step to PORTFWD_PORT: 18087 (distinct from the sibling suites' ports)")
	}
	// Ordering pin: the revisions rows POST pod-bootstrap repeatedly, so
	// they must run BEFORE the fault seam is armed (a 500-faulted
	// bootstrap would fail the rows and burn the budget). Anchor on the
	// step declaration — earlier prose comments reference the step too.
	revIdx := strings.Index(pool, scriptRef)
	armIdx := strings.Index(pool, "- name: Arm fault seam")
	if armIdx < 0 {
		t.Fatalf("pool workflow must keep the Arm fault seam step (pin anchor missing)")
	}
	if revIdx > armIdx {
		t.Fatalf("pool workflow must run the revisions suite BEFORE the fault seam is armed (revisions at %d, arm at %d)", revIdx, armIdx)
	}
}

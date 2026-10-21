// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package ghaworkflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrReviewPromptCIFirstExecutionPolicy pins the runtime-execution policy
// of the consumer review prompt (.github/prompts/pr-review.md): the reviewer
// must check the head SHA's CI status FIRST and never locally re-execute
// suites that CI already ran green on that exact head, while still executing
// locally the suites CI does not cover.
//
// Basis (LLMSafeSpaces PR #1763 drill-down + revalidation, comments
// 6308423248 / 6309172907, n=11 runs): the reviewer burned a measured mean
// 3.1 min/run (max 11.2) re-running `go vet ./...`, full `-race` suites and
// vitest that ci.yml already runs on the same head (ci.yml:145/264/304/487/
// 514), plus 101-152s `sleep N; gh pr checks` idle waits. The must-stay
// residue the reviewer uniquely covers — `-tags integration` board/agentd
// suites, agentd E2E, mutation/red-checks (revalidation: 494s + ~45s across
// the 11 runs) — is NOT CI-covered (ci.yml:617-629 covers -tags integration
// only for opencode-plugin tests) and must keep running locally.
//
// Quality floor is unchanged by design: CI-green evidence changes WHERE
// execution evidence comes from, never whether the PR's diff must contain
// the mandated test levels. Each marker below is load-bearing; removing one
// silently re-opens the double-execution / sleep-poll waste.
func TestPrReviewPromptCIFirstExecutionPolicy(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, ".github", "prompts", "pr-review.md"))
	if err != nil {
		t.Fatalf("read pr-review.md: %v", err)
	}
	// Whitespace-normalized so prompt re-wrapping can never break a pin;
	// markers are semantic phrases, not layout.
	body := strings.Join(strings.Fields(string(b)), " ")

	for _, marker := range []struct {
		needle string
		why    string
	}{
		// (a) CI-first: the head's checks are queried BEFORE any local run.
		{`gh pr checks`,
			"the reviewer must query the head's CI status (gh pr checks) before running anything locally"},
		// (a) no local re-run of CI-green suites on the same head.
		{`do NOT re-run it locally`,
			"a suite CI runs green on the review head must not be executed again by the reviewer"},
		// (a) the skip is bound to the exact head SHA, not to whatever checks happen to show.
		{`same head SHA`,
			"the CI-green skip evidence must be bound to the exact head SHA under review"},
		// (b) the must-stay residue: CI-uncovered suites stay mandatory local work.
		{`-tags integration`,
			"CI does not run the -tags integration board/agentd/handlers suites beyond its named pins — the reviewer must keep running them locally"},
		{`agentd E2E`,
			"agentd E2E is reviewer-unique verification and must stay a local execution obligation"},
		{`mutation`,
			"mutation/red-checks (break the fix, confirm the pinning test goes red) are reviewer-unique and must stay"},
		// (c) no sleep-polling.
		{`--watch`,
			"waiting on CI must use a single gh pr checks --watch (or one re-poll), never sleep N loops"},
		{`never sleep-poll`,
			"the sleep N; gh pr checks idling pattern (measured 101-152s/run) must be explicitly forbidden"},
		// Quality floor: CI-green is execution evidence only, not a coverage waiver.
		{`does NOT waive`,
			"a green CI run is execution evidence for suites CI runs — it must never waive the structural test-level requirements of the TESTS gate"},
	} {
		if !strings.Contains(body, strings.Join(strings.Fields(marker.needle), " ")) {
			t.Errorf("pr-review.md CI-first execution policy lost its marker %q: %s", marker.needle, marker.why)
		}
	}
}

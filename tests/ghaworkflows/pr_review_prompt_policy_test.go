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
// 3.1 min/run (max 11.2) re-running suites CI already executes on the same
// head — ci.yml's `go vet ./...`, full `-race` suite, `-short` coverage and
// vitest (ci.yml:145/264/304/487/514) plus the NAMED `-tags integration`
// pins (ci.yml:617-629, pkg/agent/opencode/ and cmd/workspace-agentd) and,
// when its path filters match, the secrets-integration.yml board-manager
// `-tags integration` suites (secrets-integration.yml:65,144-179: pkg/board/,
// api/internal/handlers/board*, board_manager_e2e) — plus 101-152s of
// `sleep N; gh pr checks` idle waits. The reviewer-unique residue (board/
// agentd integration suites when secrets-integration did not run, agentd
// E2E, mutation/red-checks; 494s + ~45s across the sample) stays mandatory.
//
// The wait rule is deadlock-aware: `gh pr checks --watch` watches ALL checks
// INCLUDING the review job itself (it is a check on the same PR) and would
// wait on its own completion — so the prompt must direct waiting at the CI
// workflow runs only (`gh run watch`) and must never mention the
// self-watching form as an instruction. Negative markers below pin both
// that and the inversion phrases demonstrated in review of PR #1833.
//
// Quality floor is unchanged by design: CI-green evidence changes WHERE
// execution evidence comes from, never whether the PR's diff must contain
// the mandated test levels.
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
		// (a) CI-first: the head's runs are queried BEFORE any local run.
		{`gh run list --commit <head SHA>`,
			"the reviewer must query the head's CI runs before running anything locally"},
		// (a) no local re-run of CI-green suites on the same head.
		{`do NOT re-run it locally`,
			"a suite CI runs green on the review head must not be executed again by the reviewer"},
		// (a) the skip is bound to the exact head SHA and a SUCCESS conclusion.
		{`same head SHA`,
			"the CI-green skip evidence must be bound to the exact head SHA under review"},
		{`a SKIPPED check is not evidence`,
			"skipped checks (docs-only paths, path-filtered suites) must not count as coverage — the reviewer runs those suites locally when the diff touches them"},
		// (a) conditional board coverage via the secrets-integration JOB.
		{`secrets-integration.yml`,
			"the path-filtered board-manager integration suites are covered when the secrets-integration JOB ran green on the head — without this the prompt double-mandates them (PR #1833 review finding 2)"},
		{`judge the JOB/CHECK itself, never the workflow-level conclusion`,
			"board coverage keys on the pkg/secrets integration JOB concluding SUCCESS — a path-skipped job still shows a green workflow run while the board suites never executed (PR #1833 r1 review finding 1; e.g. board_manager_tools.go changes are NOT matched by the job filter while the E2E suite exercising them is)"},
		// (b) the must-stay residue: CI-uncovered suites stay mandatory local work.
		{`-tags integration`,
			"the integration-tagged surface CI did not run on this head must stay a local execution obligation"},
		{`agentd E2E`,
			"agentd E2E beyond the named CI pins is reviewer-unique verification and must stay"},
		{`mutation`,
			"mutation/red-checks (break the fix, confirm the pinning test goes red) are reviewer-unique and must stay"},
		// (c) deadlock-safe waiting.
		{`gh run watch`,
			"waiting on CI must target the CI workflow runs (gh run watch), never all checks"},
		// (c) red triage: infra noise is not a code finding.
		{`infra/transient failure`,
			"a red check must be classified — infra/transient failures are CI noise, only diff-related failures are review findings"},
		// Quality floor: CI-green is execution evidence only, not a coverage waiver.
		{`does NOT waive`,
			"a green CI run is execution evidence for suites CI runs — it must never waive the structural test-level requirements of the TESTS gate"},
	} {
		if !strings.Contains(body, strings.Join(strings.Fields(marker.needle), " ")) {
			t.Errorf("pr-review.md CI-first execution policy lost its marker %q: %s", marker.needle, marker.why)
		}
	}

	// Negative markers — phrases that must NEVER appear. The inversion
	// phrases are the exact ones demonstrated to defeat a Contains-only pin
	// (PR #1833 review).
	for _, forbidden := range []struct {
		needle string
		why    string
	}{
		{"NEVER run `gh pr checks`",
			"inversion phrase: the reviewer must run the CI-status query, not be told to skip it"},
		{"ignore the same head SHA",
			"inversion phrase: skip evidence must stay bound to the exact head SHA"},
		{"sleep-poll instead",
			"inversion phrase: sleep-polling is the measured waste this policy removes"},
		{"always re-run",
			"inversion phrase: CI-green suites must not be re-run locally"},
		{"when that workflow RAN and is GREEN",
			"workflow-level coverage condition: the secrets-integration workflow ALWAYS runs and a path-skipped job still yields a green workflow run — coverage must key on the JOB/CHECK, not the workflow conclusion (PR #1833 r1 review finding 1)"},
	} {
		if strings.Contains(body, strings.Join(strings.Fields(forbidden.needle), " ")) {
			t.Errorf("pr-review.md CI-first execution policy contains forbidden phrase %q: %s", forbidden.needle, forbidden.why)
		}
	}

	// The self-deadlocking watch form (`gh pr checks --watch` includes THIS
	// review job's own check and waits on its own completion — PR #1833
	// review finding 1) may appear ONLY inside its explicit prohibition
	// ("NEVER `gh pr checks --watch`"). Any additional occurrence is an
	// instruction to deadlock, so total occurrences must equal prohibited
	// occurrences.
	watchTotal := strings.Count(body, "gh pr checks --watch")
	watchProhibited := strings.Count(body, "NEVER `gh pr checks --watch`")
	if watchTotal != watchProhibited {
		t.Errorf("pr-review.md: `gh pr checks --watch` appears %d times but only %d are inside the NEVER-prohibition — a bare occurrence instructs the reviewer to wait on its own review job (deadlock until job timeout)",
			watchTotal, watchProhibited)
	}

	// Ordering: the policy must precede the review checklist — it is a
	// first-action discipline ("before running ANY test locally").
	policyIdx := strings.Index(body, "VERIFICATION EXECUTION POLICY")
	checklistIdx := strings.Index(body, "Review checklist")
	if policyIdx < 0 || checklistIdx < 0 || policyIdx > checklistIdx {
		t.Errorf("policy section must exist and precede 'Review checklist' (first-action discipline); policyIdx=%d checklistIdx=%d", policyIdx, checklistIdx)
	}
}

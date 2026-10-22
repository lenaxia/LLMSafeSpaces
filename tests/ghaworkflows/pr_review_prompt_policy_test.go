// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package ghaworkflows

import (
	"os"
	"path/filepath"
	"regexp"
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
// when the path-filtered `pkg/secrets integration` JOB matched and ran
// (secrets-integration.yml:65,144-179: pkg/board/, api/internal/handlers/
// board*, board_manager_e2e), its board-manager `-tags integration` suites —
// plus 101-152s of `sleep N; gh pr checks` idle waits. The reviewer-unique
// residue (board/agentd integration suites when that job did not run, agentd
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
		// (a) the operative skip action: skipped => run it yourself.
		{`execute that suite locally yourself`,
			"the skip action is mandatory local execution — the sentence carrying the SKIPPED rule must direct the reviewer to run the suite, not merely note that evidence is missing (r2 mutation: 'do not bother running that suite locally' passed)"},
		// (a) conditional board coverage keys on the JOB, polarity bound.
		{`are covered when the ` + "`pkg/secrets integration`" + ` JOB (in secrets-integration.yml) ran and concluded SUCCESS`,
			"the positive coverage sentence binds polarity ('are covered when ... ran and concluded SUCCESS') to the JOB token — a NOT before covered or a workflow-level paraphrase both break the marker (r3 mutations)"},
		{`judge the JOB/CHECK itself, never the workflow-level conclusion`,
			"the workflow always runs and a path-skipped job still shows a green workflow run — the explicit warning must stay (e.g. board_manager_tools.go changes are NOT matched by the job filter while TestIntegrationBoardManagerE2E dispatches tools from that file)"},
		// (b) the must-stay residue: CI-uncovered suites stay mandatory local work.
		{`-tags integration`,
			"the integration-tagged surface CI did not run on this head must stay a local execution obligation"},
		{`suites not covered above`,
			"the residue clause defines itself as NOT covered above — swapping it for 'CI already covered' inverts the mandate (r4 mutation)"},
		{`agentd-handler suites are yours`,
			"the residue mandate assigns the agentd-handler suites to the reviewer — 'are NOT yours' breaks this marker (r4 mutation)"},
		{`stays mandatory on every review`,
			"the residue mandate is mandatory, never optional — 'stays optional' breaks this marker (r4 mutation)"},
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
		{"secrets-integration.yml ran and concluded",
			"workflow-level paraphrase: the verb must bind to the JOB token ('JOB (in secrets-integration.yml) ran'), not the workflow filename (PR #1833 r2 mutation)"},
		{"secrets-integration.yml ran green",
			"workflow-level paraphrase class: 'ran green' keyed on the workflow file — the workflow conclusion stays green even when the board job is path-skipped"},
		{"do not bother running that suite locally",
			"skip-action inversion: the SKIPPED rule must mandate local execution, never discourage it (PR #1833 r2 mutation)"},
		{"NOT covered when",
			"polarity inversion of the coverage claim (PR #1833 r3 mutation: 'are NOT covered when ... JOB ... SUCCESS' passed while the JOB marker matched)"},
	} {
		if strings.Contains(body, strings.Join(strings.Fields(forbidden.needle), " ")) {
			t.Errorf("pr-review.md CI-first execution policy contains forbidden phrase %q: %s", forbidden.needle, forbidden.why)
		}
	}

	// Structural polarity guard (r5: literal enumeration is whack-a-mole —
	// every round converted one spelling into another: lowercase 'do not',
	// 'must not', "don't", optional-weakening 'may'). No command directing
	// the reviewer to EXECUTE local verification may be negated or made
	// optional, case-insensitively, anywhere in the prompt. The legitimate
	// prohibitions ('do NOT re-run it locally', 'never sleep-poll') do not
	// match: the alternation requires execute/run directly after the token
	// ('re-run' is a different token; sleep-poll is not a command here).
	negatedCommand := regexp.MustCompile(`(?i)\b(do not|don't|must not|never|may)\s+(execute|run)\b`)
	if m := negatedCommand.FindAllString(body, -1); len(m) > 0 {
		t.Errorf("pr-review.md: negated/optional local-execution command(s) %q — every 'execute/run that suite locally' directive must be affirmative and mandatory (PR #1833 r4 surviving mutations)", m)
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

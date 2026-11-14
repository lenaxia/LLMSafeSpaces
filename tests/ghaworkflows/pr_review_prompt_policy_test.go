// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package ghaworkflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrReviewPromptV1LedgerPolicies pins the load-bearing policies of the
// consumer review prompt (.github/prompts/pr-review.md) after the
// review-gate v1 ledger port (PR #2033): CI triage without polling, the
// classification vocabulary, the lossless ledger block, verdict validity,
// waivers, the round budget, and the anchor closed list. The v0.2.28
// workflow's check-verdict.sh parses the REVIEW-LEDGER v1 block from every
// verdict — a prompt regression here silently breaks the review gate.
//
// Basis: the executed divergence between the ported prompt and this test's
// previous pin set (the #2033 merge left the old exact-sentence pins in
// place; the v0.44.14 release train went red on them — the pins described
// a policy apparatus the port had replaced). This file now pins the
// SHIPPED prompt.
func TestPrReviewPromptV1LedgerPolicies(t *testing.T) {
	p := filepath.Join("..", "..", ".github", "prompts", "pr-review.md")
	body, err := os.ReadFile(p)
	require_noerror(t, err)
	norm := strings.Join(strings.Fields(string(body)), " ")

	for _, marker := range []struct {
		needle, why string
	}{
		// (a) no CI polling: the injected ci-status is the only channel.
		{`do NOT sleep-and-recheck CI in any loop, and never delay the verdict to wait for runs`,
			"waiting on CI in any loop delays the verdict — the injected ci-status is the only channel (measured: idle waits cost 54% of every review turn)"},
		// (a) green head check = execution evidence; no local re-run.
		{`A green CI check on the exact head is execution evidence — cite it; do not re-run locally what CI already ran green on this head`,
			"a suite CI runs green on the review head must not be executed again by the reviewer"},
		// (b) red triage: infra noise is not a finding; a diff-related
		// failure IS one (the classification boundary).
		{`infra/transient (runner eviction, network 5xx, provider 429) is CI noise, not a finding`,
			"a red check must be classified — infra/transient failures are CI noise, only diff-related failures are review findings"},
		{`a diff-related failure (build break, test regression) is a ` + "`correctness`" + ` finding`,
			"the red-triage boundary must keep diff-related failures INSIDE the finding classes"},
		// (b) skipped checks: not evidence; targeted local execution.
		{`Skipped checks are not evidence; when the diff touches what a skipped suite would have run, run THAT suite locally — targeted (one package/one test), never full-suite`,
			"skipped checks are not evidence and the local execution obligation is targeted, never full-suite"},
		// Local residue bounds: instrument discipline.
		{`at most 2 consecutive runs, targeted lanes, never full-suite babysitting`,
			"local verification instruments must be bounded — no full-suite babysitting"},
		// Findings vocabulary: one closed class set; no "blocking" word.
		{`Classify every finding into exactly one class`,
			"the class vocabulary is a closed set the script maps to merge-gating"},
		{`correctness`, "the correctness class must stay"},
		{`robustness`, "the robustness class must stay"},
		{`security`, "the security class must stay"},
		{`test-integrity`, "the test-integrity class must stay"},
		{`You never write the word "blocking"`,
			"the reviewer never writes the word blocking — the script owns merge-gating"},
		// Verdict validity is script-computed.
		{`verdict: approve`, "the approve verdict is ledger-driven"},
		{`is only valid when the script-computed open merge-gating count is 0`,
			"approve with open merge-gating findings is rejected by the script"},
		// The ledger block: exact format, parsed by check-verdict.sh.
		{`REVIEW-LEDGER v1`, "the ledger header is the verdict contract"},
		{`END-REVIEW-LEDGER`, "the ledger terminator is the verdict contract"},
		{`Do NOT wrap it in HTML comments, do NOT paste the injected ledger JSON, do NOT reformat`,
			"the ledger block shape is the contract — wrapping/reformatting breaks the parser"},
		// Platform-refusal fallback: never deliver nothing.
		{`post the complete review as ONE ordinary PR comment`,
			"the platform-refusal fallback keeps the verdict deliverable"},
		// Waivers: honored, never re-litigated; gating waivers rejected.
		{`A finding with a matching author waiver is closed — do not re-raise it`,
			"author waivers close advisory findings permanently"},
		{`Waiver attempts on merge-gating classes are rejected by the script`,
			"waiver attempts on merge-gating classes are script-rejected"},
		// Round budget: demand-generation caps; fix-verification never capped.
		{`After round 3, new findings must be shipped-code classes`,
			"demand-generation caps after round 3 — inventing fresh demand is a process violation"},
		{`Verifying that previously-raised findings are actually fixed is always in scope`,
			"fix-verification is never capped by the round budget"},
		// Anchors: the closed list + the style sink.
		{`A real observation that anchors to nothing on this list is`, "unanchored observations sink to style"},
		{`Read every changed file`, "the coverage contract: every changed file is read"},
		{`name the regression test that would catch it in`, "correctness/robustness/security findings name their regression test"},
	} {
		if !strings.Contains(norm, strings.Join(strings.Fields(marker.needle), " ")) {
			t.Errorf("pr-review.md lost a load-bearing v1-ledger policy: %s (needle %q)", marker.why, marker.needle)
		}
	}

	// Negative markers — phrases that must NEVER appear (weakenings and
	// the retired-vocabulary regressions observed in review rounds).
	for _, neg := range []struct {
		needle, why string
	}{
		{`waive: id=; class=merge-gating`, "merge-gating findings are not waivable — a template row inviting it must not exist"},
		{`You may write "blocking"`, "the blocking-vocabulary ban must stay"},
		{`sleep-and-recheck is fine`, "the polling ban must stay"},
	} {
		if strings.Contains(norm, strings.Join(strings.Fields(neg.needle), " ")) {
			t.Errorf("pr-review.md grew a forbidden phrase: %q — %s", neg.needle, neg.why)
		}
	}
}

func require_noerror(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

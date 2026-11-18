// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strings"
	"testing"
)

// wait_test.go (unit) — the typed-wait verb's pure halves: the closed
// condition grammar (parseWaitCondition — shared with the wait-broker,
// so the table is the CONTRACT), the composed wire prose
// (composeWaitingOn), and the wait_reason textguard seam
// (validateWaitReason: cap, corruption scan, placeholder tokens).

func TestParseWaitCondition_Valid(t *testing.T) {
	for _, tc := range []struct {
		in   string
		prim string
		arg  string
	}{
		{"pr_merged(2101)", "pr_merged", "2101"},
		{"pr_merged(1)", "pr_merged", "1"},
		{"pr_merged(999999999)", "pr_merged", "999999999"},
		{"ci_green(42)", "ci_green", "42"},
		{"ci_green(7)", "ci_green", "7"},
		{"story_done(tsk-ebony-javelin-tramps)", "story_done", "tsk-ebony-javelin-tramps"},
		{"story_done(tsk-a)", "story_done", "tsk-a"},
	} {
		got, err := parseWaitCondition(tc.in)
		if err != nil {
			t.Errorf("parseWaitCondition(%q) refused a VALID condition: %v", tc.in, err)
			continue
		}
		if got.Primitive != tc.prim || got.Arg != tc.arg {
			t.Errorf("parseWaitCondition(%q) = {%s %s}, want {%s %s}", tc.in, got.Primitive, got.Arg, tc.prim, tc.arg)
		}
		if got.String() != tc.in {
			t.Errorf("WaitCondition.String() = %q, want the canonical spelling %q", got.String(), tc.in)
		}
	}
}

func TestParseWaitCondition_Invalid(t *testing.T) {
	for _, tc := range []struct {
		in    string
		needs string // substring the loud refusal must carry (the accepted-vocabulary discipline)
	}{
		{"", "does not parse"},
		{"pr_merged", "does not parse"},
		{"pr_merged(", "does not parse"},
		{"pr_merged()", "does not parse"},
		{"pr_merged(2101", "does not parse"},
		{"pr_merged 2101", "does not parse"},
		{"pr_merged(2101) ", "does not parse"},                  // trailing junk after the paren
		{"PR_MERGED(2101)", "unknown wait-condition primitive"}, // the grammar is case-closed
		{"deploy_finished(2101)", "unknown wait-condition primitive"},
		{"pr_merged(tsk-ebony-javelin-tramps)", "takes a PR number"},
		{"story_done(2101)", "takes a tsk-"},
		{"ci_green(0)", "takes a PR number"},           // canonical decimal: no zero PR
		{"ci_green(01)", "takes a PR number"},          // no leading zeros
		{"pr_merged(1234567890)", "takes a PR number"}, // 10 digits > the 1..9 cap
		{"pr_merged(-5)", "takes a PR number"},
		{"story_done(TSK-Upper-Case)", "takes a tsk-"},
		{"story_done(tsk-", "does not parse"}, // no closing paren: the grammar-level refusal fires first
		{"story_done(tsk-)", "takes a tsk-"},
		{"story_done(tsk-a_b)", "takes a tsk-"},
		{"story_done(tsk-a b)", "takes a tsk-"},
	} {
		_, err := parseWaitCondition(tc.in)
		if err == nil {
			t.Errorf("parseWaitCondition(%q) accepted an INVALID condition — the grammar is closed at the verb edge", tc.in)
			continue
		}
		guard, ok := err.(*GuardError)
		if !ok {
			t.Errorf("parseWaitCondition(%q) error is %T, want *GuardError", tc.in, err)
			continue
		}
		if guard.Code != GuardInput {
			t.Errorf("parseWaitCondition(%q) code = %s, want invalid_input", tc.in, guard.Code)
		}
		if !strings.Contains(guard.Detail, tc.needs) {
			t.Errorf("parseWaitCondition(%q) detail %q does not name the vocabulary (%q missing) — refusals are loud", tc.in, guard.Detail, tc.needs)
		}
	}
	// Every refusal carries the accepted grammar (the textguard discipline).
	_, err := parseWaitCondition("nonsense")
	if err == nil || !strings.Contains(err.Error(), "pr_merged") || !strings.Contains(err.Error(), "story_done") {
		t.Errorf("a grammar refusal must enumerate the accepted vocabulary, got: %v", err)
	}
}

func TestComposeWaitingOn(t *testing.T) {
	for _, tc := range []struct {
		waitOn, reason, want string
	}{
		{"", "", ""},
		{"", "orphan reason", ""}, // no wait, no prose — the flag is the truth
		{"user", "", "user"},
		{"user", "owner decision: runtime bump scope", "user: owner decision: runtime bump scope"},
		{"event", "PR 2083 merge", "PR 2083 merge"},
		{"event", "", ""}, // unreachable by the verb (reason required) — composes nothing, never fabricates
		{"nonsense", "x", ""},
	} {
		if got := composeWaitingOn(tc.waitOn, tc.reason); got != tc.want {
			t.Errorf("composeWaitingOn(%q,%q) = %q, want %q", tc.waitOn, tc.reason, got, tc.want)
		}
	}
}

func TestValidateWaitReason(t *testing.T) {
	// Clean prose passes (short and at-cap). The at-cap fixture is
	// NON-periodic by construction: the textguard's repetition floor
	// (50 tiles) legitimately fires on any periodic 512-byte string,
	// so the cap pin needs prose that is long but not degenerate.
	if err := validateWaitReason("PR 2083 merge"); err != nil {
		t.Errorf("clean wait_reason refused: %v", err)
	}
	var b strings.Builder
	for i := 0; b.Len() < MaxWaitReasonBytes; i++ {
		b.WriteString("step ")
		b.WriteString(itoa(i))
		b.WriteString(" of the rollout plan; ")
	}
	atCap := b.String()[:MaxWaitReasonBytes]
	if err := validateWaitReason(atCap); err != nil {
		t.Errorf("a wait_reason at the 512-byte cap refused: %v", err)
	}
	// Over the cap: GuardInput naming the field and the cap.
	over := atCap[:MaxWaitReasonBytes] + "x"
	err := validateWaitReason(over)
	if err == nil {
		t.Fatal("an over-cap wait_reason was accepted — the cap is 512")
	}
	guard, ok := err.(*GuardError)
	if !ok || guard.Code != GuardInput {
		t.Fatalf("over-cap wait_reason error = %v, want GuardError invalid_input", err)
	}
	if !strings.Contains(guard.Detail, "wait_reason") || !strings.Contains(guard.Detail, "512") {
		t.Errorf("the cap refusal must name the field and the cap, got: %s", guard.Detail)
	}
	// Placeholder-class tokens refuse (the #1820 emission-corruption
	// substitute — a wait statement is never a placeholder).
	for _, ph := range []string{"placeholder", "TODO", " tbd ", "asdf", "qwerty"} {
		err := validateWaitReason(ph)
		if err == nil {
			t.Errorf("placeholder wait_reason %q accepted — the whole-value token scan is the verb edge's", ph)
			continue
		}
		if g, ok := err.(*GuardError); !ok || g.Code != GuardTextRejected {
			t.Errorf("placeholder wait_reason %q error = %v, want text_rejected", ph, err)
		}
	}
	// Prose that merely MENTIONS a token passes (containment never matches).
	if err := validateWaitReason("waiting on the TODO purge PR to merge"); err != nil {
		t.Errorf("prose mentioning a token refused: %v", err)
	}
}

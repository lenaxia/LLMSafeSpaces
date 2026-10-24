package app

import (
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// #1832 item 5 (2/2): the charter prompt renders the verdict chain
// (the next claimant's iteration history) and the reviewer charter
// carries the verdict contract.

func TestCharterPrompt_RendersVerdictChain(t *testing.T) {
	i := 1
	c := &board.Claim{
		ShortID: "tsk-aaaa0001", ClaimToken: "tok",
		Task: &board.Task{Description: "Do the thing",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: false}}},
		VerdictChain: []board.Verdict{
			{Verdict: board.VerdictFail, Reviewer: "rev-1", Applied: true, Transition: "in_progress",
				Findings: []board.VerdictFinding{{Severity: board.SeverityBlocker, Pointer: "pkg/x.go:9", CriterionIndex: &i}}},
			{Verdict: board.VerdictApprove, Reviewer: "rev-1", Applied: false, Transition: "refused:done_guard"},
		},
	}
	got := boardCharterPrompt(c)
	for _, want := range []string{
		"PRIOR REVIEW VERDICTS",
		"fail by rev-1",
		"blocker: pkg/x.go:9 [criterion 1]",
		"approve by rev-1 (transition refused: refused:done_guard)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("charter prompt missing %q:\n%s", want, got)
		}
	}
}

func TestCharterPrompt_NoChainNoSection(t *testing.T) {
	got := boardCharterPrompt(&board.Claim{ShortID: "tsk-aaaa0001", ClaimToken: "tok"})
	if strings.Contains(got, "PRIOR REVIEW VERDICTS") {
		t.Fatalf("empty chain must not render the section")
	}
}

// Stage-persona claim-then-spawn (owner directive 2026-10-24): the
// charter renders the configured persona — the legacy reviewer
// composition's default persona AND an arbitrary named persona with
// custom instructions — while a persona-less claim stays
// byte-identical to today's worker charter.
func TestCharterPrompt_PersonaRendering(t *testing.T) {
	// The legacy composition's default reviewer persona (a reviewer
	// claim for in_review) renders the verdict contract.
	rev := &board.PersonaConfig{Name: board.DefaultReviewerPersona.Name, Instructions: board.DefaultReviewerPersona.Instructions}
	got := boardCharterPrompt(&board.Claim{
		ShortID: "tsk-aaaa0001", ClaimToken: "tok", Stage: board.StageInReview, Persona: rev,
		Task: &board.Task{Description: "desc",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}, {Text: "c2"}}},
	})
	for _, want := range []string{
		"You are the 'reviewer' persona for task tsk-aaaa0001 at stage in_review (claim token: tok)",
		"board_record_verdict",
		"blocker|major|minor",
		"Never merge",
		"[ ] c1", "[ ] c2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reviewer persona charter missing %q:\n%s", want, got)
		}
	}

	// An ARBITRARY persona (no instructions) renders the generic
	// scaffold; nothing is hardwired to reviewer/in_review.
	got = boardCharterPrompt(&board.Claim{
		ShortID: "tsk-bbbb0002", ClaimToken: "tk2", Stage: board.StageInProgress,
		Persona: &board.PersonaConfig{Name: "implementer"},
	})
	if !strings.Contains(got, "You are the 'implementer' persona for task tsk-bbbb0002 at stage in_progress (claim token: tk2)") {
		t.Fatalf("arbitrary persona scaffold missing:\n%s", got)
	}

	// Persona-less: today's first line, byte-identical.
	got = boardCharterPrompt(&board.Claim{ShortID: "tsk-cccc0003", ClaimToken: "tk3"})
	if !strings.HasPrefix(got, "You are the worker for task tsk-cccc0003 (claim token: tk3).\n") {
		t.Fatalf("persona-less charter must render today's opening verbatim:\n%s", got)
	}
}

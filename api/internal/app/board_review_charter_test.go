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

func TestReviewerCharterPrompt_Contract(t *testing.T) {
	got := reviewerCharterPrompt(
		&board.Task{ShortID: "tsk-aaaa0001", Description: "desc",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}, {Text: "c2"}}},
		&board.Project{ShortID: "prj-aaaa1111"})
	for _, want := range []string{
		"assigned reviewer for task tsk-aaaa0001",
		"[0] c1", "[1] c2",
		"board_record_verdict",
		"blocker|major|minor",
		"Never merge",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reviewer charter missing %q:\n%s", want, got)
		}
	}
}

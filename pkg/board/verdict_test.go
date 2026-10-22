package board

import (
	"testing"
)

// #1832 item 5 (1/2) — the verdict payload's validation matrix (pure
// unit layer; the transition semantics pin in the PG harness suite).

func verdictFixture() VerdictInput {
	i := 1
	return VerdictInput{
		Verdict:  VerdictFail,
		Reviewer: "reviewer-session",
		Findings: []VerdictFinding{{Severity: SeverityBlocker, Pointer: "pkg/x.go:42", CriterionIndex: &i}},
	}
}

func TestValidateVerdict_Matrix(t *testing.T) {
	criteria := []AcceptanceCriterion{{Text: "a", Met: false}, {Text: "b", Met: true}}
	cases := []struct {
		name    string
		mutate  func(*VerdictInput)
		wantErr string
	}{
		{"valid fail with finding", nil, ""},
		{"valid approve (findings optional)", func(in *VerdictInput) { in.Verdict = VerdictApprove; in.Findings = nil }, ""},
		{"unknown verdict kind", func(in *VerdictInput) { in.Verdict = "maybe" }, `verdict must be "approve" or "fail"`},
		{"missing reviewer", func(in *VerdictInput) { in.Reviewer = "" }, "reviewer identity is required"},
		{"fail without findings", func(in *VerdictInput) { in.Findings = nil }, "a fail verdict requires at least one finding"},
		{"unknown severity", func(in *VerdictInput) { in.Findings[0].Severity = "cosmic" }, "severity must be blocker, major, or minor"},
		{"empty pointer", func(in *VerdictInput) { in.Findings[0].Pointer = "" }, "pointer is required"},
		{"criterion index negative", func(in *VerdictInput) { i := -1; in.Findings[0].CriterionIndex = &i }, "out of bounds"},
		{"criterion index beyond criteria", func(in *VerdictInput) { i := 2; in.Findings[0].CriterionIndex = &i }, "out of bounds"},
		{"criterion index at last valid slot", func(in *VerdictInput) { i := 1; in.Findings[0].CriterionIndex = &i }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := verdictFixture()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			err := ValidateVerdict(in, criteria)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

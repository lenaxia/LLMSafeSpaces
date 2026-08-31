// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package scripts_test

// renumber_pr_num_test.go — regression harness for
// scripts/renumber-pr-num.sh (worklog-renumber bot, PR #1191).
//
// Run 33428514243: the bot's tail -1 over all #N refs picked ISSUE 1183
// from a squash body and failed its job commenting on a non-PR. These
// tests execute the REAL script against the exact bug shapes.

import (
	"os/exec"
	"testing"
)

func runExtract(t *testing.T, subject, body string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command(bash, "renumber-pr-num.sh", subject, body).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %s", out)
	}
	for i, c := range out {
		if c == '\n' || c == ' ' {
			out = out[:i]
			break
		}
	}
	return string(out)
}

func TestRenumberPRNum(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		body    string
		want    string
	}{
		// The EXACT bug scenario (run 33428514243): squash title carries
		// the issue ref, body resolves ANOTHER issue — the PR is the
		// trailing parenthesized ref on the subject.
		{"exact bug: issue refs in title+body, PR trails subject",
			"test(harness): US-70.0 (#1182) (#1187)", "Closes #1182. Resolves #1183.", "1187"},
		{"squash with single trailing ref",
			"feat: something (#1182)", "", "1182"},
		{"merge-commit subject (no trailing parens) falls back",
			"Merge pull request #1190 from lenaxia/feat/x", "merge commit body", "1190"},
		{"merge-commit subject, issue ref in body — subject wins",
			"Merge pull request #1187 from lenaxia/x", "Closes #1182", "1187"},
		{"bot commit with no refs — empty (caller skips)",
			"chore(repolint): assign worklog numbers [skip ci]", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runExtract(t, tc.subject, tc.body); got != tc.want {
				t.Errorf("extract(%q, %q) = %q, want %q", tc.subject, tc.body, got, tc.want)
			}
		})
	}
}

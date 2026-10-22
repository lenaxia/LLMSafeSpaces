// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// textguard_test.go — #1837 P1b: the emission-corruption rejection
// heuristics (format-independent, fixture-driven). Every REJECT fixture
// is drawn from the #1832 dogfood evidence class; every PASS fixture is
// legitimate prose/markdown/JSON that MUST NOT be rejected (the review's
// false-positive gate: both directions are pinned).

// --- length caps ---------------------------------------------------------------

func TestTextGuard_LengthCaps(t *testing.T) {
	cases := []struct {
		name    string
		fn      func() error
		wantSub string
	}{
		{"task title over cap", func() error {
			return ValidateTaskInput(TaskInput{Title: strings.Repeat("t", MaxTitleBytes+1)})
		}, "title"},
		{"task description over cap", func() error {
			return ValidateTaskInput(TaskInput{Title: "ok", Description: strings.Repeat("d", MaxDescriptionBytes+1)})
		}, "description"},
		{"epic outcome over cap", func() error {
			return ValidateEpicInput(EpicInput{Title: "ok", Outcome: strings.Repeat("o", MaxOutcomeBytes+1)})
		}, "outcome"},
		{"project name over cap", func() error {
			return ValidateProjectInput(ProjectInput{Name: strings.Repeat("n", MaxNameBytes+1)})
		}, "name"},
		{"criterion text over cap", func() error {
			return ValidateTaskInput(TaskInput{Title: "ok", AcceptanceCriteria: []AcceptanceCriterion{
				{Text: strings.Repeat("c", MaxCriterionBytes+1)},
			}})
		}, "criterion"},
		{"tag over cap", func() error {
			return ValidateTaskInput(TaskInput{Title: "ok", Tags: []string{strings.Repeat("g", MaxTagBytes+1)}})
		}, "tag"},
		{"too many tags", func() error {
			tags := make([]string, MaxTags+1)
			for i := range tags {
				tags[i] = "t"
			}
			return ValidateTaskInput(TaskInput{Title: "ok", Tags: tags})
		}, "tags"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
			var ge *GuardError
			require.ErrorAs(t, err, &ge)
			require.Equal(t, GuardInput, ge.Code, "length caps are plain input validation, not the corruption class")
		})
	}
}

func TestTextGuard_AtCapPasses(t *testing.T) {
	// Content exactly AT the cap is legitimate — the cap is inclusive
	// and long prose is expected (that is what description_file exists
	// to carry, but inline content up to the cap is legal). Varied
	// prose (distinct lines, no tiling) trips no heuristic.
	var b strings.Builder
	for i := 0; b.Len() < MaxDescriptionBytes; i++ {
		b.WriteString("Line ")
		b.WriteString(strings.Repeat("x", i%9+1))
		b.WriteString(" carries unique payload ")
		b.WriteString(strings.Repeat("y", (i*7)%5+1))
		b.WriteString(" numbered ")
		b.WriteString(strings.Repeat("z", i%4+1))
		b.WriteString(".\n")
	}
	atCap := b.String()[:MaxDescriptionBytes]
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: atCap}))
	require.NoError(t, ValidateTaskInput(TaskInput{Title: atCap[:MaxTitleBytes]}))
}

// --- degenerate repetition -------------------------------------------------------

// TestTextGuard_RepeatedFragmentRejected is the acceptance-bar fixture:
// a 50-fold repeated fragment is rejected with a clear error.
func TestTextGuard_RepeatedFragmentRejected(t *testing.T) {
	fragment := "Fix the lexer edge case where the token stream doubles. "
	payload := strings.Repeat(fragment, 50)
	err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
	require.Error(t, err, "a 50-fold repeated fragment is the #1832 corruption signature")
	var ge *GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, GuardTextRejected, ge.Code)
	require.Contains(t, err.Error(), "repetition", "the error must name the class clearly")
}

func TestTextGuard_RepetitionAroundJunkRejected(t *testing.T) {
	// The observed corruption often wraps the loop in prose junk — the
	// line-based detector catches fragments aligned to lines.
	lines := []string{"Ship the fix once the review lands on the main branch."}
	for i := 0; i < 60; i++ {
		lines = append(lines, "Retry the loop step and continue the same task again.")
	}
	payload := strings.Join(lines, "\n")
	err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
	require.Error(t, err)
	require.Contains(t, err.Error(), "repetition")
}

func TestTextGuard_RepetitionBelowFloorPasses(t *testing.T) {
	// 40 repeats is under the 50 floor — legitimate drafting noise is
	// never rejected (the threshold matches the observed corruption
	// class, which repeats fragments hundreds of times).
	payload := strings.Repeat("Step: verify the guard. ", 40)
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: payload}))
}

func TestTextGuard_DividersAndTablesPass(t *testing.T) {
	// Legitimate repeated-structure content that is NOT corruption:
	// ASCII dividers (punctuation-only, no alnum), markdown table
	// separators, and a long single-character bar.
	var b strings.Builder
	for i := 0; i < 80; i++ {
		b.WriteString("------------------------------------------------\n")
	}
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: b.String()}))
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: strings.Repeat("=", 400)}))
}

// --- JSON structural splice ------------------------------------------------------

// TestTextGuard_SpliceArtifactRejected is the acceptance-bar fixture:
// the literal #1832 stored-corruption artifact (a description whose
// text contains raw JSON structural bytes) is rejected, never stored.
func TestTextGuard_SpliceArtifactRejected(t *testing.T) {
	payload := "Fix the thing and note the observed corruption artifact \",\"title\":\"PLACEHOLDER\"} in the emitted arguments."
	err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
	require.Error(t, err, "raw JSON structural splices stored inside string values are the silent-corruption class")
	var ge *GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, GuardTextRejected, ge.Code)
	require.Contains(t, err.Error(), "splice")
}

func TestTextGuard_LegitJSONExamplesPass(t *testing.T) {
	// The review's false-positive gate: legitimate markdown carrying
	// JSON examples must never be rejected.
	payloads := []string{
		// fenced JSON block
		"The request body is:\n\n```json\n{\"name\":\"value\",\"x\":\"y\",\"count\":\"3\"}\n```\n\nShip it.",
		// balanced inline JSON in prose
		`The tool echoes {"a":"1","b":"2","c":"3","d":"4"} back to the caller.`,
		// a line that IS compact JSON
		"{\"key\":\"value\",\"x\":\"y\"}",
		// yaml-ish prose and tables (no splice bytes at all)
		"| col | val |\n|-----|-----|\n| a   | b   |",
	}
	for _, p := range payloads {
		require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: p}),
			"legitimate content must pass: %q", p[:min(40, len(p))])
	}
}

func TestTextGuard_SpliceInFencedBlockPasses(t *testing.T) {
	// A fenced code block QUOTING the corruption artifact (a runbook
	// documenting what corruption looks like) is legitimate content.
	payload := "When you see this in a stored row:\n\n```\n\",\"title\":\"PLACEHOLDER\"}\n```\n\nregenerate the call."
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: payload}))
}

// --- epic/project fields --------------------------------------------------------

func TestTextGuard_EpicAndProjectFields(t *testing.T) {
	err := ValidateEpicInput(EpicInput{Title: "ok", Outcome: strings.Repeat("do it again ", 60)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "repetition")

	err = ValidateProjectInput(ProjectInput{Name: strings.Repeat("p", MaxNameBytes+1)})
	require.Error(t, err)
}

// --- the 4KB round-trip class (inline; the file-backed path is agentd's) --------

func TestTextGuard_LongLegitProsePasses(t *testing.T) {
	// A >4KB charter of legitimate prose passes validation — the caps
	// are kilobytes above this, and varied prose trips no heuristic.
	// Every line is unique (numbered steps — the realistic shape; a
	// cycling generator would itself be the degenerate class).
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("Step ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(" ")
		b.WriteString(strings.Repeat("x", i%7+1))
		b.WriteString(" describes a distinct action with unique numbering ")
		b.WriteString(strings.Repeat("y", (i*3)%5+1))
		b.WriteString(".\n")
	}
	require.Greater(t, b.Len(), 4096)
	require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: b.String()}))
}

// --- #1837 review round 1: evasion paths + exemption holes ----------------------

func TestTextGuard_JunkWrappedRepetitionRejected(t *testing.T) {
	// The #1832 evidence shape: prose junk wrapping the loop — single
	// line AND multiline. Neither the exact-tiling nor line detector
	// alone catches it; runRepetition must.
	frag := "Retry the loop step and continue the same task again. "
	t.Run("single line, prefix+50x+suffix", func(t *testing.T) {
		payload := "prefix junk " + strings.Repeat(frag, 50) + " suffix junk"
		err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
		require.Error(t, err)
		require.Contains(t, err.Error(), "repetition")
	})
	t.Run("multiline junk-wrapped", func(t *testing.T) {
		payload := "Intro line.\n" + strings.Repeat(frag, 55) + "\nOutro line."
		err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
		require.Error(t, err)
		require.Contains(t, err.Error(), "repetition")
	})
	t.Run("49x under the floor passes", func(t *testing.T) {
		payload := "prefix " + strings.Repeat(frag, 49) + " suffix"
		require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: payload}))
	})
}

func TestTextGuard_SpliceExemptionHolesClosed(t *testing.T) {
	// Line-start artifacts are NOT wholesale exempt (a corrupted line
	// beginning with " or { opens no balanced span); only TERMINATED
	// fences exempt (an unterminated fence exempts nothing after it).
	t.Run("line-start splice artifact rejected", func(t *testing.T) {
		payload := "Steps:\n" + "\",\"x\":\"PLACEHOLDER\"} tail prose\nDone."
		err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
		require.Error(t, err, "a line beginning with the artifact bytes is not a legitimate-JSON exemption")
		require.Contains(t, err.Error(), "splice")
	})
	t.Run("artifact after unterminated fence rejected", func(t *testing.T) {
		payload := "Example:\n```\ncode line\n" + "\",\"title\":\"PLACEHOLDER\"} trailing artifact"
		err := ValidateTaskInput(TaskInput{Title: "ok", Description: payload})
		require.Error(t, err, "an unterminated fence exempts nothing after it")
		require.Contains(t, err.Error(), "splice")
	})
	t.Run("legit JSON example line still passes (balanced)", func(t *testing.T) {
		require.NoError(t, ValidateTaskInput(TaskInput{Title: "ok", Description: "{\"key\":\"value\",\"x\":\"y\"}"}))
	})
}

func TestTextGuard_CriterionErrorHasNoFileHint(t *testing.T) {
	err := ValidateTaskInput(TaskInput{Title: "ok", AcceptanceCriteria: []AcceptanceCriterion{
		{Text: strings.Repeat("c", MaxCriterionBytes+1)},
	}})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "criterion_file", "no such param exists — never suggest it")
	require.Contains(t, err.Error(), "criterion[0].text")
}

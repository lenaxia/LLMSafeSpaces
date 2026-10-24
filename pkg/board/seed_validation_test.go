// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// seed_validation_test.go — #1849 items 1+6 (adjudication comment
// 6357191381): the seed-prevention layer composing with #1837's
// guardrails. Item 1: an atomic task CREATE carries a non-empty
// description (the worker charter) AND at least one acceptance
// criterion (Done is undefined without one). Item 6: the #1820
// placeholder-class (#1814 send_message precedent) rejects whole-value
// placeholder tokens at the boundary. Red-first: this file landed
// BEFORE validateTaskSeed.

func TestSeedValidation_RequiresCharterAndCriteria(t *testing.T) {
	cases := []struct {
		name    string
		in      TaskInput
		wantSub string
	}{
		{"no description", TaskInput{Title: "T", AcceptanceCriteria: []AcceptanceCriterion{{Text: "c"}}}, "description"},
		{"whitespace description", TaskInput{Title: "T", Description: "   \n\t ", AcceptanceCriteria: []AcceptanceCriterion{{Text: "c"}}}, "description"},
		{"no criteria", TaskInput{Title: "T", Description: "charter"}, "criterion"},
		{"empty criterion text", TaskInput{Title: "T", Description: "charter", AcceptanceCriteria: []AcceptanceCriterion{{Text: "  "}}}, "criterion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskSeed(tc.in)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantSub)
			var ge *GuardError
			require.ErrorAs(t, err, &ge)
			require.Equal(t, GuardInput, ge.Code)
		})
	}
}

func TestSeedValidation_Pass(t *testing.T) {
	require.NoError(t, validateTaskSeed(TaskInput{
		Title:       "T",
		Description: "a short but real charter",
		AcceptanceCriteria: []AcceptanceCriterion{
			{Text: "tests green"},
		},
	}))
}

func TestSeedValidation_PlaceholderClassRejected(t *testing.T) {
	// The #1820/#1814 set: whole-value equality after trim+case-fold.
	tokens := []string{
		"placeholder", "PLACEHOLDER", "  placeholder  ", "\nplaceholder\t",
		"placeholder-removed", "<placeholder>", "todo", "TODO", "tbd", "TBD",
		"asdf", "qwerty",
	}
	for _, tok := range tokens {
		t.Run("description="+tok, func(t *testing.T) {
			err := validateTaskSeed(TaskInput{Title: "T", Description: tok,
				AcceptanceCriteria: []AcceptanceCriterion{{Text: "c"}}})
			require.Error(t, err)
			require.Contains(t, err.Error(), "placeholder")
			var ge *GuardError
			require.ErrorAs(t, err, &ge)
			require.Equal(t, GuardTextRejected, ge.Code)
		})
	}
	t.Run("placeholder title", func(t *testing.T) {
		err := validateTaskSeed(TaskInput{Title: "placeholder", Description: "real charter",
			AcceptanceCriteria: []AcceptanceCriterion{{Text: "c"}}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "title")
	})
	t.Run("placeholder criterion", func(t *testing.T) {
		err := validateTaskSeed(TaskInput{Title: "T", Description: "real charter",
			AcceptanceCriteria: []AcceptanceCriterion{{Text: "tbd"}}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "criterion")
	})
}

func TestSeedValidation_ContainmentPasses(t *testing.T) {
	// Never whole-value equality: prose that MENTIONS a token passes
	// (the #1814 survivor discipline — short legit payloads and
	// containment cases deliver).
	for _, desc := range []string{
		"TODO: fix the flux capacitor ordering before shipping",
		"A placeholder for the vendored config lives in /etc — do not delete",
		"ok", "x", "test", "ping", "probe",
	} {
		require.NoError(t, validateTaskSeed(TaskInput{Title: "T", Description: desc,
			AcceptanceCriteria: []AcceptanceCriterion{{Text: "c"}}}), "prose mentioning tokens passes: %q", desc)
	}
}

func TestIsPlaceholderClass(t *testing.T) {
	require.True(t, IsPlaceholderClass("  TBD "))
	require.False(t, IsPlaceholderClass("tbd: later"))
	require.False(t, IsPlaceholderClass(strings.Repeat("todo", 60)))
}

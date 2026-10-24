// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// seed_validation.go — #1849 items 1+6 (orchestrator adjudication,
// comment 6357191381): the seed-prevention layer composing with #1837's
// guardrails. An atomic task CREATE carries the worker's whole brief:
// a non-empty description (the charter — a seedless task hands the
// fresh worker nothing) AND at least one acceptance criterion (Done is
// undefined without one). Placeholder-class charters — the #1820
// emission-corruption mode where the mid-write substitute fires as the
// call lands — reject at the boundary, the #1814 send_message
// precedent's discipline: whole-value equality after trim+case-fold
// over a deliberately NARROW set; prose that merely mentions a token
// passes, short legitimate payloads pass.

import (
	"fmt"
	"strings"
)

// placeholderClassSet is the #1820/#1814 exact-match set — the same
// tokens send_message's guard rejects. Containment never matches.
var placeholderClassSet = map[string]bool{
	"placeholder":         true,
	"placeholder-removed": true,
	"<placeholder>":       true,
	"todo":                true,
	"tbd":                 true,
	"asdf":                true,
	"qwerty":              true,
}

// IsPlaceholderClass reports whether the value is ENTIRELY a
// placeholder-class token — trimmed + case-folded first (the exported
// form is caller-friendly; the #1814 send_message guard delegates
// here).
func IsPlaceholderClass(value string) bool {
	return placeholderClassSet[strings.ToLower(strings.TrimSpace(value))]
}

// seedFieldPlaceholderError renders the shared rejection detail.
func seedFieldPlaceholderError(field string) error {
	return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
		"%s is a placeholder-class token (the #1820 emission-corruption substitute) — re-read the draft and re-compose; prose that mentions the token in passing is fine, whole-value tokens are not",
		field)}
}

// validateTaskSeed enforces the atomic-create contract on task seeds:
// non-empty description, at least one non-empty criterion, and no
// whole-value placeholder tokens in title/description/criteria.
func validateTaskSeed(in TaskInput) error {
	if strings.TrimSpace(in.Description) == "" {
		return &GuardError{Code: GuardInput, Detail: "task description is required at create — it is the worker's charter (a seedless task hands the fresh worker nothing; #1849)"}
	}
	if len(in.AcceptanceCriteria) == 0 {
		return &GuardError{Code: GuardInput, Detail: "at least one acceptance criterion is required at create — Done is undefined without one (#1849)"}
	}
	for i, c := range in.AcceptanceCriteria {
		if strings.TrimSpace(c.Text) == "" {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("criterion[%d].text is empty — every criterion must carry checkable text (#1849)", i)}
		}
	}
	if IsPlaceholderClass(strings.TrimSpace(in.Title)) {
		return seedFieldPlaceholderError("task title")
	}
	if IsPlaceholderClass(strings.TrimSpace(in.Description)) {
		return seedFieldPlaceholderError("task description")
	}
	for i, c := range in.AcceptanceCriteria {
		if IsPlaceholderClass(strings.TrimSpace(c.Text)) {
			return seedFieldPlaceholderError(fmt.Sprintf("criterion[%d].text", i))
		}
	}
	return nil
}

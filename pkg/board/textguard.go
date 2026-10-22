// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// textguard.go — #1837 P1b: server-side rejection heuristics for the
// emission-corruption class observed in the #1832 dogfood (long string
// fields corrupted — sometimes silently — by degenerate repetition
// loops and raw JSON structural splices). Format-independent: these
// checks run on the DECODED string values at the Service layer, so
// REST, the external platform MCP, and the agent seam all enforce
// them identically by construction.
//
// Design posture (per the #1837 review): every heuristic is
// fixture-driven with BOTH directions pinned — corruption fixtures
// reject, legitimate prose/markdown/JSON/code passes. The thresholds
// match the observed class (fragments repeated 50+ times; structural
// splice bytes) and sit far above legitimate content; nothing here
// truncates silently — a violation is a loud GuardError.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Field size caps (bytes on the UTF-8 wire form). The caps are
// generous multiples of real charter sizes: inline prose up to the cap
// is legal, and the *_file agent-surface params (P1a) exist so long
// content never needs to ride the emitted-argument path at all.
const (
	MaxNameBytes        = 256
	MaxTitleBytes       = 512
	MaxDescriptionBytes = 128 << 10 // 128 KiB
	MaxOutcomeBytes     = 64 << 10  // 64 KiB
	MaxCriterionBytes   = 8 << 10   // 8 KiB
	MaxTagBytes         = 64
	MaxTags             = 16
)

// repetitionFloor is the degenerate-repetition threshold: a string
// tiled 50+ times by one unit (or a single line repeated 50+ times) is
// the observed corruption shape. Below the floor is never corruption
// (legitimate drafting noise repeats far less).
const repetitionFloor = 50

// GuardTextRejected is the corruption-class guard code (distinct from
// invalid_input so clients can surface "regenerate the argument"
// guidance instead of "fix your field").
const GuardTextRejected = "text_rejected"

// boardSpliceRe matches the raw JSON structural splice signature: a
// quote-comma-quote close/open adjacency carrying a bare key into a
// quote-colon — the bytes of a serialized JSON object leaking INTO a
// string value (the #1832 stored artifact was literally
// `","title":"PLACEHOLDER"}` inside a description).
var boardSpliceRe = regexp.MustCompile(`","[A-Za-z_][A-Za-z0-9_]*":"`)

// ValidateProjectInput enforces the text-field caps on project writes.
func ValidateProjectInput(in ProjectInput) error {
	return validateTextField("name", in.Name, MaxNameBytes)
}

// ValidateEpicInput enforces caps + corruption heuristics on epic
// writes (title, outcome — the steering wheel).
func ValidateEpicInput(in EpicInput) error {
	if err := validateTextField("title", in.Title, MaxTitleBytes); err != nil {
		return err
	}
	return validateTextField("outcome", in.Outcome, MaxOutcomeBytes)
}

// ValidateTaskInput enforces caps + corruption heuristics on task
// writes (title, description, criteria texts, tags).
func ValidateTaskInput(in TaskInput) error {
	if err := validateTextField("title", in.Title, MaxTitleBytes); err != nil {
		return err
	}
	if err := validateTextField("description", in.Description, MaxDescriptionBytes); err != nil {
		return err
	}
	if err := validateTags(in.Tags); err != nil {
		return err
	}
	for i, c := range in.AcceptanceCriteria {
		if err := validateTextField(fmt.Sprintf("criterion[%d].text", i), c.Text, MaxCriterionBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateTags(tags []string) error {
	if len(tags) > MaxTags {
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("tags: %d entries exceed the %d-tag cap", len(tags), MaxTags)}
	}
	for _, tag := range tags {
		if len(tag) > MaxTagBytes {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("tag %q exceeds the %d-byte cap", tag, MaxTagBytes)}
		}
	}
	return nil
}

// validateTextField enforces the byte cap, then the corruption
// heuristics. Empty values (the field is omitted / keeps existing)
// pass — updates patch.
func validateTextField(field, s string, max int) error {
	if s == "" {
		return nil
	}
	if n := len(s); n > max {
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s: %d bytes exceeds the %d-byte cap (send long content via the agent surface's %s_file param)", field, n, max, strings.Split(field, "[")[0])}
	}
	return scanTextCorruption(field, s)
}

// scanTextCorruption runs the two degenerate-content detectors over
// one field's value.
func scanTextCorruption(field, s string) error {
	if unit, count := tiledRepetition(s); count >= repetitionFloor {
		return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
			"%s: degenerate repetition — a %d-byte fragment tiles the content %d times (floor %d); suspected emission corruption — regenerate the argument (long content: use the %s_file param)",
			field, len(unit), count, repetitionFloor, strings.Split(field, "[")[0])}
	}
	if line, count := repeatedLine(s); count >= repetitionFloor {
		return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
			"%s: degenerate repetition — the line %q appears %d times (floor %d); suspected emission corruption — regenerate the argument (long content: use the %s_file param)",
			field, truncateFor(line, 60), count, repetitionFloor, strings.Split(field, "[")[0])}
	}
	if n := spliceHits(s); n > 0 {
		return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
			"%s: %d raw JSON structural splice artifact(s) (`\",\"key\":\"` adjacency) inside the string value — never stored silently; suspected emission corruption — regenerate the argument (a fenced code block quoting the artifact is exempt)",
			field, n)}
	}
	return nil
}

// tiledRepetition reports the smallest tiling unit when s is exactly
// k≥2 repeats of one unit (k, unit), else (s, 1). The unit must carry
// an alphanumeric rune — punctuation-only bars (ASCII dividers) are
// legitimate formatting, never corruption.
func tiledRepetition(s string) (string, int) {
	n := len(s)
	if n < repetitionFloor {
		return s, 1
	}
	// Smallest period: the classic (s+s) rotation search. s tiled by u
	// (s = u^k, k≥2) iff s occurs in (s+s)[1:] at index < n-1.
	doubled := (s + s)[1:]
	idx := strings.Index(doubled, s)
	if idx < 0 || idx+1 >= n {
		return s, 1
	}
	p := idx + 1
	if n%p != 0 {
		return s, 1
	}
	unit := s[:p]
	if !hasAlnum(unit) {
		return s, 1
	}
	return unit, n / p
}

// repeatedLine reports the most-repeated single line and its count.
// Only lines with ≥8 bytes and at least one alphanumeric rune count —
// short/punctuation-only lines (markdown table separators, dividers)
// are formatting, not signal.
func repeatedLine(s string) (string, int) {
	best, bestCount := "", 0
	counts := map[string]int{}
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if len(line) < 8 || !hasAlnum(line) {
			continue
		}
		counts[line]++
		if counts[line] > bestCount {
			best, bestCount = line, counts[line]
		}
	}
	return best, bestCount
}

// spliceHits counts raw JSON structural splice matches NOT inside an
// exempt context. Exempts (the false-positive gates, both pinned by
// test): fenced code blocks (```-delimited), balanced {...}/[...]
// containers (a legitimate inline JSON example), and lines whose
// trimmed form opens with a JSON structural byte ({, [, ").
func spliceHits(s string) int {
	if !strings.Contains(s, `","`) {
		return 0 // fast path: the signature cannot occur
	}
	exempt := spliceExemptSpans(s)
	n := 0
	for _, loc := range boardSpliceRe.FindAllStringIndex(s, -1) {
		if !exempt(loc[0]) {
			n++
		}
	}
	return n
}

// spliceExemptSpans builds the exempt-position predicate: fenced
// blocks, balanced containers, and JSON-opening lines.
func spliceExemptSpans(s string) func(int) bool {
	type span struct{ start, end int }
	var spans []span

	addLineSpans := func() {
		offset := 0
		for _, raw := range strings.Split(s, "\n") {
			start, end := offset, offset+len(raw)
			trimmed := strings.TrimSpace(raw)
			if trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') {
				spans = append(spans, span{start, end})
			}
			offset = end + 1
		}
	}
	addLineSpans()

	// Fenced code blocks: every ```-delimited region.
	inFence := false
	fenceStart := 0
	offset := 0
	for _, raw := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			if !inFence {
				inFence = true
				fenceStart = offset
			} else {
				spans = append(spans, span{fenceStart, offset + len(raw)})
				inFence = false
			}
		}
		offset += len(raw) + 1
	}
	if inFence { // unterminated fence: exempt to the end
		spans = append(spans, span{fenceStart, len(s)})
	}

	// Balanced containers: a stack walk tracking double-quoted string
	// state (backslash escapes) over { and [ openers. A container that
	// closes with its internal string state clean records its span —
	// a legitimate embedded JSON object/array. Stray closers (the
	// corruption artifact's trailing `}` has no opener) open no span.
	type open struct {
		pos     int
		kind    byte
		inStr   bool
		escaped bool
	}
	var stack []open
	for i := 0; i < len(s); i++ {
		c := s[i]
		if len(stack) == 0 {
			if c == '{' || c == '[' {
				stack = append(stack, open{pos: i, kind: c})
			}
			continue
		}
		top := &stack[len(stack)-1]
		if top.inStr {
			if top.escaped {
				top.escaped = false
			} else if c == '\\' {
				top.escaped = true
			} else if c == '"' {
				top.inStr = false
			}
			continue
		}
		switch c {
		case '"':
			top.inStr = true
		case '{', '[':
			stack = append(stack, open{pos: i, kind: c})
		case '}', ']':
			want := byte('}')
			if top.kind == '[' {
				want = ']'
			}
			if c == want {
				spans = append(spans, span{top.pos, i + 1})
				stack = stack[:len(stack)-1]
			} else {
				stack = stack[:len(stack)-1] // mismatched: no span either side
			}
		}
	}

	return func(pos int) bool {
		for _, sp := range spans {
			if pos >= sp.start && pos < sp.end {
				return true
			}
		}
		return false
	}
}

// hasAlnum reports whether s carries at least one letter or digit.
func hasAlnum(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// truncateFor renders a fragment for error messages.
func truncateFor(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

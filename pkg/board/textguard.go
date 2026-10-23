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
var boardSpliceRe = regexp.MustCompile(`",\s*"[A-Za-z_][A-Za-z0-9_]*"\s*:\s*"`)

// ValidateProjectInput enforces the text-field caps on project writes.
func ValidateProjectInput(in ProjectInput) error {
	if err := validateTextField("name", in.Name, MaxNameBytes); err != nil {
		return err
	}
	return validateTags(in.Tags)
}

// ValidateEpicInput enforces caps + corruption heuristics on epic
// writes (title, outcome — the steering wheel).
func ValidateEpicInput(in EpicInput) error {
	if err := validateTextField("title", in.Title, MaxTitleBytes); err != nil {
		return err
	}
	if err := validateTextField("outcome", in.Outcome, MaxOutcomeBytes); err != nil {
		return err
	}
	return validateTags(in.Tags)
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
		// The corruption scan rides the tag channel too (#1837 review
		// round 2): a 24-byte tag carrying the #1832 splice artifact is
		// rejected, never stored silently.
		if err := scanTextCorruption("tag "+truncateFor(tag, 24), tag); err != nil {
			return err
		}
	}
	return nil
}

// validateTextField enforces the byte cap, then the corruption
// heuristics. Empty values (the field is omitted / keeps existing)
// pass — updates patch. NOTE (#1837 review): the deliberately
// UNGUARDED string fields are move-stage/verdict evidence URLs and
// free-form session/verdict reviewer identities — short, non-prose
// identifiers with no corruption surface; the evidence URL carries a
// plain cap at its own call sites.
func validateTextField(field, s string, max int) error {
	if s == "" {
		return nil
	}
	if n := len(s); n > max {
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s: %d bytes exceeds the %d-byte cap%s", field, n, max, fileParamHint(field))}
	}
	return scanTextCorruption(field, s)
}

// fileParamHint names the agent-surface file param ONLY for the fields
// that actually have one (description/outcome) — a "criterion_file"
// suggestion for a field with no such param is misleading guidance.
func fileParamHint(field string) string {
	base := strings.Split(field, "[")[0]
	if base == "description" || base == "outcome" {
		return fmt.Sprintf(" (send long content via the agent surface's %s_file param)", base)
	}
	return ""
}

// scanTextCorruption runs the degenerate-content detectors over one
// field's value.
func scanTextCorruption(field, s string) error {
	if unit, count := tiledRepetition(s); count >= repetitionFloor {
		return repetitionError(field, fmt.Sprintf("a %d-byte fragment tiles the content %d times", len(unit), count))
	}
	if unit, count := runRepetition(s); count >= repetitionFloor {
		return repetitionError(field, fmt.Sprintf("a %d-byte fragment repeats %d times inside a longer string (junk-wrapped)", len(unit), count))
	}
	if line, count := repeatedLine(s); count >= repetitionFloor {
		return repetitionError(field, fmt.Sprintf("the line %q appears %d times", truncateFor(line, 60), count))
	}
	if n := spliceHits(s); n > 0 {
		return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
			"%s: %d raw JSON structural splice artifact(s) (`\",\"key\":\"` adjacency) inside the string value — never stored silently; suspected emission corruption — regenerate the argument (a TERMINATED fenced code block quoting the artifact is exempt)",
			field, n)}
	}
	return nil
}

func repetitionError(field, shape string) error {
	return &GuardError{Code: GuardTextRejected, Detail: fmt.Sprintf(
		"%s: degenerate repetition — %s (floor %d); suspected emission corruption — regenerate the argument%s",
		field, shape, repetitionFloor, fileParamHint(field))}
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

// runRepetition catches the junk-wrapped corruption shape the #1832
// evidence actually shows: prefix + fragment*N + suffix, contiguous,
// on one line or across lines. For every candidate unit length p in
// [1, runUnitMax] it tracks the longest run r such that s[i] ==
// s[i-p] holds throughout — a run of length ≥ repetitionFloor*p is N
// back-to-back repeats of a p-byte unit. The unit must carry an
// alnum rune (divider bars are formatting). SCOPE BOUND (documented,
// deliberate): junk-wrapped units LARGER than runUnitMax bytes and
// repeats broken by unique interleaved separators are outside this
// heuristic — exact tiling at any size stays tiledRepetition's job.
func runRepetition(s string) (string, int) {
	n := len(s)
	if n < repetitionFloor {
		return s, 1 // a 50-unit run needs at least 50 bytes (p=1)
	}
	limit := runUnitMax
	if maxUnit := n / repetitionFloor; maxUnit < limit {
		limit = maxUnit
	}
	for p := 1; p <= limit; p++ {
		bestStart, bestLen := 0, 0
		runStart, runLen := 0, 0
		for i := p; i < n; i++ {
			if s[i] == s[i-p] {
				if runLen == 0 {
					runStart = i - p
				}
				runLen++
			} else {
				if runLen > bestLen {
					bestStart, bestLen = runStart, runLen
				}
				runLen = 0
			}
		}
		if runLen > bestLen {
			bestStart, bestLen = runStart, runLen
		}
		// bestLen counts MATCHING bytes after the first unit; a run of
		// k full units matches (k-1)*p bytes.
		if bestLen >= (repetitionFloor-1)*p {
			unit := s[bestStart : bestStart+p]
			if hasAlnum(unit) {
				return unit, bestLen/p + 1
			}
		}
	}
	return s, 1
}

// runUnitMax bounds runRepetition's unit-length scan. Units up to
// ~half a_kb line cover the observed fragment sizes at 128 KiB worst
// case cost; larger exact tiling is tiledRepetition's.
const runUnitMax = 512

// repeatedLine reports the most-repeated single line and its count.
// Only lines with ≥8 bytes and at least one alphanumeric rune count —
// short/punctuation-only lines (markdown table separators, dividers)
// are formatting, not signal. RULING (documented, pinned by test):
// interleaved boilerplate — e.g. 55 unique steps sharing one identical
// checklist line — is REJECTED at the floor by design; the #1832
// corruption evidence repeats fragments hundreds of times and the
// false-positive risk of exempting interleaved repeats outweighs the
// rare legitimate identical-line run. Content needing >49 identical
// boilerplate lines must vary them (numbering); the error says so.
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
// exempt context. Exempts (the false-positive gates, pinned by test):
// TERMINATED fenced code blocks (```-delimited) and balanced
// {...}/[...] containers (a legitimate inline JSON example — inline
// or line-leading). RULING: the repetition detectors have NO fence
// exemption by design — quoting JSON artifacts inside a fence is
// common legitimate documentation; a 50+-fold repeated line inside a
// fence is still the degenerate class (pinned).
func spliceHits(s string) int {
	exempt := spliceExemptSpans(s)
	n := 0
	for _, loc := range boardSpliceRe.FindAllStringIndex(s, -1) {
		if !exempt(loc[0]) {
			n++
		}
	}
	return n
}

// spliceExemptSpans builds the exempt-position predicate. Exempts
// (the false-positive gates, test-pinned): TERMINATED fenced code
// blocks (```-delimited — an unterminated fence exempts nothing: a
// corruption artifact after a stray fence is still corruption) and
// balanced {...}/[...] containers anywhere (a legitimate inline or
// line-leading JSON example is balanced by construction; a splice
// artifact's stray quotes/unbalanced braces open no span).
func spliceExemptSpans(s string) func(int) bool {
	type span struct{ start, end int }
	var spans []span

	// Fenced code blocks: every TERMINATED ```-delimited region. An
	// unterminated fence exempts nothing (#1837 review: exempting to
	// EOF would let artifacts after a stray fence store silently).
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

// truncateFor renders a fragment for error messages, rune-safe (a
// mid-rune byte slice would marshal to U+FFFD).
func truncateFor(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for i := 0; i < 4 && len(cut) > 0; i++ { // back off to a rune boundary
		r := rune(cut[len(cut)-1])
		if r&0xC0 != 0x80 { // not a continuation byte
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

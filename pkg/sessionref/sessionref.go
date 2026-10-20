// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sessionref implements design 0066 §4.6's did-you-mean
// candidate scoring — the ONE pure, deterministic function every
// resolution surface (API REST 404s, the sessions search filter, and
// the agentd MCP tools' #1667-generalized clause) shares. The platform
// owns the algorithm; clients never re-implement it.
//
// Ranking law (ruling 8 + §4.6): match class exact-prefix > token-
// overlap > edit-distance, then last-active DESC as the deterministic
// tiebreak, top-3 always. SUGGEST-ONLY by construction: Rank returns
// ordered candidates; no surface may act on them.
package sessionref

import (
	"sort"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// SuggestMaxEdits bounds how close an id must be to the requested
// (unresolvable) id to be offered — the #1667 covenant verbatim:
// distance 1 covers single-character drops/inserts/substitutions,
// distance 2 an adjacent transposition or double drop; ≥3 edits
// between ids is noise, not a typo.
const SuggestMaxEdits = 2

// MaxSuggestions is the payload cap (ruling 8: "top-3, always").
const MaxSuggestions = 3

// Match classes, best first (§4.6 ranking).
const (
	classExactPrefix = iota
	classTokenOverlap
	classEditDistance
)

// Candidate is one in-scope session the input may have meant. ShortID,
// Title, Slug, Workspace may be empty; LastActive zero means unknown
// (sorts oldest). Slug is the harness-native adjective-noun name
// (agentd surfaces it live; the API index does not carry it — the
// field is empty there by design).
type Candidate struct {
	SessionID  string
	ShortID    string
	Title      string
	Slug       string
	Workspace  string
	LastActive time.Time
}

// Suggestion is the structured did-you-mean payload row (ruling 8:
// {title, shortId, workspace, lastActive}; title omitted when the
// session has none — omitempty). SessionID rides along additively: it
// is the re-copy target the #1663 clause names, and it is a field the
// session-list surface already exposes to the same caller.
type Suggestion struct {
	Title      string     `json:"title,omitempty"`
	ShortID    string     `json:"shortId,omitempty"`
	Workspace  string     `json:"workspace"`
	LastActive *time.Time `json:"lastActive,omitempty"`
	// SessionID carries the harness id for ranking/clause internals
	// ONLY — never serialized (#1756 §1: the ses_-form leaves default
	// output; the phrase in ShortID is the re-copy affordance).
	SessionID string `json:"-"`
}

// classify returns the input's shape family (design §4.6: the
// classifier is a heuristic for scoring selection, never validation).
type inputForm int

const (
	formAlias inputForm = iota
	formShortID
	formLegacyID
)

// Rank scores candidates against a failed input and returns the top-3
// suggestions, best first. Deterministic: same input + same candidate
// set ⇒ same output. Empty when nothing scores (silence is correct
// then — there is genuinely nothing near). Id-form inputs score by
// bounded edit distance over short ids (case-insensitive) and legacy
// harness ids; alias-form inputs score by word-boundary prefix and
// token overlap over titles and slugs — the families never mix, so a
// typo'd id can never masquerade as a name match or vice versa.
func Rank(input string, cands []Candidate) []Suggestion {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	form := classify(input)
	inFold := fold(input)

	type scored struct {
		cand  Candidate
		class int
		dist  int
	}
	var matches []scored
	for _, c := range cands {
		if c.SessionID == "" && c.ShortID == "" {
			continue
		}
		switch form {
		case formShortID, formLegacyID:
			if d, ok := idScore(input, form, c); ok {
				matches = append(matches, scored{cand: c, class: classEditDistance, dist: d})
			}
		default:
			if class, ok := aliasScore(inFold, c); ok {
				matches = append(matches, scored{cand: c, class: class, dist: 0})
			}
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if a.dist != b.dist {
			return a.dist < b.dist
		}
		if !a.cand.LastActive.Equal(b.cand.LastActive) {
			return a.cand.LastActive.After(b.cand.LastActive)
		}
		return a.cand.SessionID < b.cand.SessionID
	})
	if len(matches) > MaxSuggestions {
		matches = matches[:MaxSuggestions]
	}
	out := make([]Suggestion, 0, len(matches))
	for _, m := range matches {
		s := Suggestion{
			Title:     m.cand.Title,
			ShortID:   m.cand.ShortID,
			SessionID: m.cand.SessionID,
			Workspace: m.cand.Workspace,
		}
		if !m.cand.LastActive.IsZero() {
			t := m.cand.LastActive.UTC()
			s.LastActive = &t
		}
		out = append(out, s)
	}
	return out
}

func classify(input string) inputForm {
	switch {
	case shortid.IsShortIDForm(input):
		return formShortID
	case shortid.IsLegacyIDForm(input):
		return formLegacyID
	default:
		return formAlias
	}
}

// idScore computes the best bounded edit distance between an id-form
// input and a candidate's ids: case-insensitive over the short id
// (ruling 2), byte-exact over the legacy harness id.
func idScore(input string, form inputForm, c Candidate) (int, bool) {
	best := SuggestMaxEdits + 1
	if c.ShortID != "" {
		if d := EditDistanceBounded(shortid.Normalize(input), shortid.Normalize(c.ShortID), SuggestMaxEdits); d <= SuggestMaxEdits && d < best {
			best = d
		}
	}
	if c.SessionID != "" {
		if d := EditDistanceBounded(input, c.SessionID, SuggestMaxEdits); d <= SuggestMaxEdits && d < best {
			best = d
		}
	}
	if best > SuggestMaxEdits {
		return 0, false
	}
	return best, true
}

// aliasScore scores an alias-form (folded) input against a candidate's
// names: word-boundary prefix (class 0) — an incidental mid-word
// substring prefix ("swift heron" inside "swift herons migration") is
// NOT an exact-prefix — then token overlap (class 1). Tokens shorter
// than 3 characters never overlap (a "w" would match half the fleet).
func aliasScore(inFold string, c Candidate) (int, bool) {
	title := fold(c.Title)
	slug := fold(c.Slug)
	if wordPrefix(inFold, title) || wordPrefix(inFold, slug) {
		return classExactPrefix, true
	}
	inTokens := tokens(inFold)
	if len(inTokens) == 0 {
		return 0, false
	}
	candTokens := tokens(title + " " + slug)
	for _, it := range inTokens {
		for _, ct := range candTokens {
			if it == ct {
				return classTokenOverlap, true
			}
		}
	}
	return 0, false
}

// wordPrefix reports whether in is a prefix of s ending on a word
// boundary: s == in, or s continues with a non-alphanumeric character.
func wordPrefix(in, s string) bool {
	if in == "" || s == "" {
		return false
	}
	if !strings.HasPrefix(s, in) {
		return false
	}
	if len(s) == len(in) {
		return true
	}
	r := rune(s[len(in)])
	return !isWordRune(r)
}

func isWordRune(r rune) bool {
	return r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// tokens splits a folded string into lowercase alphanumeric runs of
// length ≥ 3.
func tokens(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return !isWordRune(r) }) {
		if len(tok) >= 3 {
			out = append(out, tok)
		}
	}
	return out
}

// EditDistanceBounded is Levenshtein distance with an early exit:
// once every cell of the current row exceeds bound, the distance
// cannot come back under it, so bound+1 is returned immediately.
// Shared verbatim with the #1667 agentd layer (single algorithm).
func EditDistanceBounded(a, b string, bound int) int {
	la, lb := len(a), len(b)
	if la == 0 {
		if lb <= bound {
			return lb
		}
		return bound + 1
	}
	if lb == 0 {
		if la <= bound {
			return la
		}
		return bound + 1
	}
	if abs(la-lb) > bound {
		return bound + 1
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
			if curr[j] < rowMin {
				rowMin = curr[j]
			}
		}
		if rowMin > bound {
			return bound + 1
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// fold is the alias-comparison normal form: casefold + trim.
func fold(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}

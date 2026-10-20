// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package shortid mints and classifies the platform's session short
// IDs.
//
// The CURRENT format (#1756, superseding design 0066 ruling 2's
// hash form) is a 3-word grammatical phrase — adjective-noun-verb
// from the frozen curated lists in wordlists.go (e.g.
// "amber-falcon-glides") — minted at the registration seam, per-
// workspace unique via the (workspace_id, short_id) index with
// check-and-retry, and a deterministic counter suffix ("-2", "-3", …)
// on collision. No hash characters, no harness prefix, no checksum
// word: words are high-probability LLM tokens, truncation is visible,
// and the did-you-mean ranker recovers near-misses on words far
// better than it ever could on hash fragments.
//
// The PREVIOUS hash form ("oc-ab12cd34", #1745's release) exists on
// clusters that ran main between the two rulings: those rows stay
// addressable as INPUT — classified by IsLegacyShortIDForm, accepted
// by resolution with the #1756 deprecation notice, never minted
// again.
package shortid

import (
	"sort"
	"strings"
)

// Normalize lowercases a candidate short id for case-insensitive
// resolution ("Amber-Falcon-Glides" resolves).
func Normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IsShortIDForm reports whether s carries the #1756 session-phrase
// shape: EXACTLY three lowercase word tokens (case-insensitive on
// input), optionally plus a deterministic counter suffix token ("-2",
// "-10", …) from a mint collision. Exact word count is the format's
// malformed-shape guard: partial IDs (one or two words) are
// categorically invalid, and shapes that belong to other namespaces —
// the harness's 2-word adjective-noun slugs, #1757's 2-word+digit
// workspace phrases, the #1745 hash short ids — never classify here.
func IsShortIDForm(s string) bool {
	words := strings.Split(Normalize(s), "-")
	if len(words) != 3 && len(words) != 4 {
		return false
	}
	for _, w := range words[:3] {
		if !lowerAlphaWord.MatchString(w) {
			return false
		}
	}
	if len(words) == 4 {
		return counterSuffix.MatchString(words[3])
	}
	return true
}

// IsLegacyIDForm reports whether s looks like a harness-native legacy
// id (the `ses_…` shape) — the FULL id whose external emission #1756
// retires and whose INPUT stays accepted (one-release deprecation).
func IsLegacyIDForm(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "ses_")
}

// IsDeprecatedInputForm reports whether the ref is an INPUT-only
// class whose acceptance is deprecated by #1756: the harness full id
// or the #1745 hash short id. Canonical addressing is the phrase;
// aliases aside, everything else carries the deprecation notice.
func IsDeprecatedInputForm(s string) bool {
	return IsLegacyIDForm(s) || IsLegacyShortIDForm(s)
}

// IsSeamIDForm reports whether the ref matches the harness seam's id
// charset (^([A-Za-z0-9_-]{1,128})$) — a ref matching it can be handed
// to the seam verbatim; anything else can only ever have been a name.
// Shared by the API edge middleware and the agentd tools so the
// degraded-lookup write guard (design 0066, review r2 finding B) uses
// ONE definition.
func IsSeamIDForm(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// IsIDForm reports whether the ref is id-shaped at all (phrase,
// legacy hash short id, or full id). A ref that is not id-shaped can
// only be an alias (title or slug) — resolution surfaces use this to
// decide between pass-through (ids: the adapter remains the authority)
// and must-resolve (aliases: an unresolvable alias is an immediate
// 404-with-suggestions).
func IsIDForm(s string) bool {
	return IsShortIDForm(s) || IsDeprecatedInputForm(s)
}

// sortedSlice is the deterministic slice mirror of a frozen set.
func sortedSlice(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package shortid implements design 0066's platform-minted short session
// IDs: `<prefix><8 chars>` over the [0-9a-z] alphabet, minted with
// crypto/rand at the registration seam, per-workspace unique via the
// (workspace_id, short_id) composite unique index with check-and-retry
// (ruling 3: expected lifetime collision rate ~0.002% in a 10k-session
// workspace — a bounded retry loop is not an availability risk).
//
// The prefix is mint-time origin metadata only (ruling 4): it records
// which harness family created the session and is never dispatched on
// at resolution — a `cc-` id resolves identically in any workspace.
package shortid

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// BodyLen is the character count after the prefix (ruling 2).
const BodyLen = 8

// Alphabet is the 36-symbol charset (lowercase digits + letters).
const Alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// harnessPrefixes is the closed prefix registry (ruling 2 + ruling 4).
// New harnesses take a prefix at their adapter's landing PR; the
// reserved values are not reassignable once minted under.
var harnessPrefixes = map[string]string{
	"opencode":    "oc-",
	"claude-code": "cc-",
	"codex":       "cx-",
	"pi":          "pi-",
}

// PrefixForHarness returns the mint prefix for a harness type. The
// empty/unknown harness maps to opencode — the only live adapter today
// (design 0066 §4.2); multi-prefix paths are exercised by tests until a
// second adapter lands.
func PrefixForHarness(harnessType string) (string, bool) {
	p, ok := harnessPrefixes[strings.TrimSpace(strings.ToLower(harnessType))]
	if !ok {
		return "oc-", false
	}
	return p, true
}

// Mint returns a fresh short id for the harness type: prefix + 8 chars
// over Alphabet, minted from crypto/rand (ruling 6: mint-and-persist,
// never a deterministic derivation of the harness id — a hash would
// preserve the coupling in disguise).
func Mint(harnessType string) (string, error) {
	prefix, _ := PrefixForHarness(harnessType)
	buf := make([]byte, BodyLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("shortid: mint: %w", err)
	}
	var b strings.Builder
	b.Grow(len(prefix) + BodyLen)
	b.WriteString(prefix)
	for _, c := range buf {
		b.WriteByte(Alphabet[int(c)%len(Alphabet)])
	}
	return b.String(), nil
}

// Normalize lowercases a candidate short id for case-insensitive
// resolution (ruling 2: `OC-AB12CD34` resolves).
func Normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IsShortIDForm reports whether s looks like a short id (any registry
// prefix, body of 4-12 lowercase-or-case letters/digits after the
// hyphen). ~8 is the canonical body; the wider window keeps the
// classifier useful for truncated/typo'd inputs, which belong to
// suggestion scoring, not validation.
func IsShortIDForm(s string) bool {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	for _, p := range harnessPrefixes {
		if !strings.HasPrefix(lower, p) {
			continue
		}
		body := s[len(p):]
		if len(body) < 4 || len(body) > 12 {
			return false
		}
		for _, c := range body {
			if !strings.ContainsRune(Alphabet, c) && !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ", c) {
				return false
			}
		}
		return true
	}
	return false
}

// IsLegacyIDForm reports whether s looks like a harness-native legacy
// id (the `ses_…` shape). Heuristic, same discipline as IsShortIDForm.
func IsLegacyIDForm(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "ses_")
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

// IsIDForm reports whether the ref is id-shaped at all (short or
// legacy). A ref that is not id-shaped can only be an alias (title or
// slug) — resolution surfaces use this to decide between pass-through
// (ids: the adapter remains the authority) and must-resolve (aliases:
// an unresolvable alias is an immediate 404-with-suggestions).
func IsIDForm(s string) bool {
	return IsShortIDForm(s) || IsLegacyIDForm(s)
}

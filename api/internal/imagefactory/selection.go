// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package imagefactory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// extIDRe constrains extension IDs to a safe charset. Uppercase and shell
// metacharacters are rejected — extensions are operator-authored catalog
// entries, not user free text, but a typo'd ID must fail loudly.
var extIDRe = regexp.MustCompile(`^[a-z0-9@._-]+$`)

// hashRe matches the HashSelection output shape: "s-" + 16 hex chars.
var hashRe = regexp.MustCompile(`^s-[0-9a-f]{16}$`)

// IsValidHash reports whether s is a well-formed schematic hash.
func IsValidHash(s string) bool {
	return hashRe.MatchString(s)
}

// ValidateSelection checks that the selection is non-empty and every ID
// matches the extension ID charset. It does NOT check existence in the
// catalog — that is the store's concern (ResolveSelection does it).
func ValidateSelection(sel Selection) error {
	if len(sel) == 0 {
		return fmt.Errorf("selection: empty")
	}
	for i, id := range sel {
		if id == "" {
			return fmt.Errorf("selection[%d]: empty id", i)
		}
		if !extIDRe.MatchString(id) {
			return fmt.Errorf("selection[%d]: %q fails charset (allowed: [a-z0-9@._-])", i, id)
		}
	}
	return nil
}

// HashSelection computes the content-addressed schematic hash over the
// sorted-deduped selection IDs + base name. Pure, deterministic, no I/O.
// Returns "s-" + first 16 hex chars of SHA-256 (design/0046 #1, #2).
//
// Version is deliberately NOT in the preimage — version is a separate axis
// (design/0046 #2): the same schematic hash identifies a family of images,
// one per base version that has been built.
func HashSelection(sel Selection, baseName string) (string, error) {
	if err := ValidateSelection(sel); err != nil {
		return "", err
	}
	if baseName == "" || containsWhitespace(baseName) {
		return "", fmt.Errorf("baseName: must be non-empty without whitespace, got %q", baseName)
	}
	canonical := canonicalHashPreimage(sel, baseName)
	sum := sha256.Sum256([]byte(canonical))
	return "s-" + hex.EncodeToString(sum[:])[:16], nil
}

// canonicalHashPreimage is the stable string the hash is over. Two
// selections that differ only in input ordering produce identical output,
// so equivalent schematics share a hash and a cache slot.
func canonicalHashPreimage(sel Selection, baseName string) string {
	dedup := sortedDedup(sel)
	// Use a delimiter that cannot appear inside an ID (charset rejects it)
	// nor inside a base name (rejected if it contains whitespace; we further
	// trust operator base names not to contain "," — enforced in store).
	return strings.Join(dedup, ",") + "|" + baseName
}

func sortedDedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func containsWhitespace(s string) bool {
	return strings.ContainsAny(s, " \t\n\r\v\f")
}

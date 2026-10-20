// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"fmt"
	mathRand "math/rand"
	"regexp"
	"strings"
)

// mintRand selects words uniformly. The phrase space is not a secret
// (#1756: enumeration is ruled a non-issue — ids are references under
// authz, never capabilities); crypto/rand seeds the process PRNG once
// so the hot registration path pays no crypto overhead per mint.
//
//nolint:gosec // G404: the phrase space is not a secret (#1756: enumeration ruled a non-issue; ids are references under authz); crypto/rand seeds once so the hot path pays no crypto cost.
var mintRand = mathRand.New(mathRand.NewSource(cryptoSeed()))

func cryptoSeed() int64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// Unreachable in practice; a fixed seed would be a mint
		// determinism bug, so panic loudly rather than degrade.
		panic("shortid: crypto/rand unavailable: " + err.Error())
	}
	//nolint:gosec // G115: any nonzero seed suffices; the high-bit
	// truncation only reduces the seed space, never correctness.
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

// Slice mirrors of the frozen sets for uniform random selection.
var (
	adjectiveWords = sortedSlice(Adjectives)
	nounWords      = sortedSlice(Nouns)
	verbWords      = sortedSlice(Verbs)
)

// MintPhrase returns a fresh 3-word adjective-noun-verb phrase from
// the frozen lists — the #1756 canonical session short id. Collisions
// are NOT retried here; the registration seam's check-and-retry loop
// applies WithCounterSuffix deterministically.
func MintPhrase() (string, error) {
	return adjectiveWords[mintRand.Intn(len(adjectiveWords))] + "-" +
		nounWords[mintRand.Intn(len(nounWords))] + "-" +
		verbWords[mintRand.Intn(len(verbWords))], nil
}

// WithCounterSuffix renders a collision's deterministic
// disambiguator: base-2, base-3, … (the unsuffixed form is the first
// mint; N < 2 clamps to 2).
func WithCounterSuffix(base string, n int) string {
	if n < 2 {
		n = 2
	}
	return fmt.Sprintf("%s-%d", base, n)
}

// legacyShortIDForm recognizes the #1745 hash-format short id
// (registry prefix + 4..12 alphanumerics). Clusters that ran main
// between the two rulings hold such rows: the form stays addressable
// as INPUT (with the deprecation notice) and is never minted again.
var legacyShortIDForm = regexp.MustCompile(`^(oc|cc|cx|pi)-[0-9a-zA-Z]{4,12}$`)

// IsLegacyShortIDForm classifies the #1745 hash short-id form.
func IsLegacyShortIDForm(s string) bool {
	return legacyShortIDForm.MatchString(strings.ToLower(strings.TrimSpace(s)))
}

// counterSuffix matches the deterministic collision suffix (-2, -10, …).
var counterSuffix = regexp.MustCompile(`^(?:[2-9]|[1-9][0-9]+)$`)
var lowerAlphaWord = regexp.MustCompile(`^[a-z]+$`)
